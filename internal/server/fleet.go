package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/benjaminsanborn/goku/internal/cloud"

	"github.com/benjaminsanborn/goku/internal/store"
)

// Fleet: SSH-attached deploy targets (design doc 10). Instances are uniform —
// the control-plane host is just the fleet member registered with the local
// driver.

func (s *Server) handleListInstances(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	instances, err := s.Store.ListInstances(r.Context(), org)
	if err != nil {
		respond(w, nil, err)
		return
	}
	// Assignments come from live deployments' recorded placement; rows that
	// predate the instance column count as local.
	assignments := map[string][]string{}
	localName := "local"
	for _, i := range instances {
		if i.Driver == "local" {
			localName = i.Name
		}
	}
	if rows, err := s.Store.LiveAssignments(r.Context(), org); err == nil {
		for _, a := range rows {
			name := a.Instance
			if name == "" {
				name = localName
			}
			assignments[name] = append(assignments[name], a.Project+" · "+a.Branch)
		}
	}
	out := []map[string]any{}
	for _, i := range instances {
		out = append(out, map[string]any{
			"instance": i, "assignments": append([]string{}, assignments[i.Name]...),
		})
	}
	respond(w, map[string]any{"instances": out}, nil)
}

func (s *Server) handleAddInstance(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name    string `json:"name"`
		Address string `json:"address"` // user@host[:port]
		SSHKey  string `json:"ssh_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if !strings.Contains(in.Address, "@") {
		httpError(w, http.StatusUnprocessableEntity, "address must look like user@host or user@host:port")
		return
	}
	if !strings.Contains(in.SSHKey, "PRIVATE KEY") {
		httpError(w, http.StatusUnprocessableEntity, "ssh_key must be a PEM private key (the .pem contents)")
		return
	}
	org := orgFrom(r.Context())
	inst, err := s.Store.CreateInstance(r.Context(), org, store.NewInstance{
		Name: strings.ToLower(in.Name), Driver: "ssh", Address: in.Address, SSHKey: in.SSHKey,
	}, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go s.verifyInstance(org, inst.ID)
	respond(w, map[string]any{"instance": inst}, nil)
}

func (s *Server) handleVerifyInstance(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	inst, err := s.Store.GetInstance(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	go s.verifyInstance(org, inst.ID)
	respond(w, map[string]any{"verifying": inst.Name}, nil)
}

// handleDeleteInstance deregisters an instance. Machines goku provisioned are
// also terminated in the provider account — leaving them running would bill
// the operator for a machine nothing can reach anymore.
func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	inst, err := s.Store.GetInstance(r.Context(), org, r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	provider := (*store.Provider)(nil)
	if inst.ProviderID != "" && inst.ExternalID != "" {
		provider, _ = s.Store.GetProvider(r.Context(), org, inst.ProviderID)
	}
	deleted, err := s.Store.DeleteInstance(r.Context(), org, inst.ID, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	// A provisioned machine is also torn down at the provider, and dropped
	// from the tailnet so terminated instances don't linger as devices.
	netProvider, _ := s.Store.ProviderByKind(r.Context(), org, "tailscale")
	terminated := false
	if provider != nil && provider.Kind == "aws" {
		terminated = true
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			aws := cloud.AWSFrom(provider.Credentials, provider.Region)
			if err := aws.Terminate(ctx, deleted.ExternalID, deleted.KeyName); err != nil {
				log.Printf("terminate %s: %v", deleted.Name, err)
			}
			if netProvider != nil {
				if err := cloud.TailscaleFrom(netProvider.Credentials).RemoveDevice(ctx, "goku-"+deleted.Name); err != nil {
					log.Printf("tailnet cleanup %s: %v", deleted.Name, err)
				}
			}
		}()
	}
	respond(w, map[string]any{"deleted": true, "terminated": terminated}, nil)
}

// VerifyInstanceByID re-runs verification (exported for boot registration).
func (s *Server) VerifyInstanceByID(org, id string) { s.verifyInstance(org, id) }

// verifyInstance runs the enrollment checks: reachable, docker usable,
// facts collected. Results land in the instance's check log.
func (s *Server) verifyInstance(org, id string) {
	ctx := context.Background()
	inst, err := s.Store.GetInstance(ctx, org, id)
	if err != nil {
		return
	}

	var log strings.Builder
	facts := map[string]any{}
	step := func(name string, run func() (string, error)) bool {
		out, err := run()
		if err != nil {
			fmt.Fprintf(&log, "✗ %s: %v\n", name, err)
			return false
		}
		fmt.Fprintf(&log, "✓ %s: %s\n", name, strings.TrimSpace(out))
		return true
	}

	exe := s.executorFor(inst)
	defer exe.cleanup()

	ok := step("reachable", func() (string, error) { return exe.run("echo ok") }) &&
		step("arch", func() (string, error) {
			out, err := exe.run("uname -m")
			facts["arch"] = strings.TrimSpace(out)
			return out, err
		}) &&
		step("os", func() (string, error) {
			out, err := exe.run(". /etc/os-release 2>/dev/null && echo $PRETTY_NAME || uname -s")
			facts["os"] = strings.TrimSpace(out)
			return out, err
		}) &&
		step("docker", func() (string, error) {
			out, err := exe.run("docker info --format '{{.ServerVersion}}'")
			if err != nil {
				return "", fmt.Errorf("docker unusable — install it and grant access:\n  curl -fsSL https://get.docker.com | sh && sudo usermod -aG docker $USER\n%v", err)
			}
			facts["docker"] = strings.TrimSpace(out)
			return "v" + strings.TrimSpace(out), err
		}) &&
		step("resources", func() (string, error) {
			out, err := exe.run(`echo "$(nproc) cpus, $(free -m 2>/dev/null | awk '/^Mem:/{print $2}')MB ram, $(df -h / | awk 'NR==2{print $4}') disk free"`)
			facts["resources"] = strings.TrimSpace(out)
			return out, err
		})

	status := "ready"
	if !ok {
		status = "failed"
		if !strings.Contains(log.String(), "✓ reachable") {
			status = "unreachable"
		}
	}
	s.Store.SetInstanceCheck(ctx, id, status, log.String(), facts)
}

// executor abstracts "run a shell command on the instance".
type executor struct {
	run     func(cmd string) (string, error)
	cleanup func()
}

func (s *Server) executorFor(inst *store.Instance) executor {
	if inst.Driver == "local" {
		return executor{
			run: func(cmd string) (string, error) {
				out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
				return string(out), err
			},
			cleanup: func() {},
		}
	}
	return s.sshExecutor(inst.Address, inst.SSHKey)
}

// sshExecutor runs commands on any machine goku holds a key for — fleet
// instances and managed databases alike.
func (s *Server) sshExecutor(address, sshKey string) executor {
	// key to a 0600 temp file; host keys pinned per data dir.
	keyFile, _ := os.CreateTemp("", "goku-ssh-*")
	keyFile.Chmod(0o600)
	keyFile.WriteString(sshKey)
	if !strings.HasSuffix(sshKey, "\n") {
		keyFile.WriteString("\n")
	}
	keyFile.Close()

	target, port := address, "22"
	if host, p, ok := strings.Cut(address, ":"); ok {
		target, port = host, p
	}
	knownHosts := filepath.Join(s.DataDir, "ssh_known_hosts")

	return executor{
		run: func(cmd string) (string, error) {
			ssh := exec.Command("ssh",
				"-i", keyFile.Name(), "-p", port,
				"-o", "BatchMode=yes",
				"-o", "ConnectTimeout=8",
				"-o", "StrictHostKeyChecking=accept-new",
				"-o", "UserKnownHostsFile="+knownHosts,
				target, cmd)
			out, err := ssh.CombinedOutput()
			if err != nil {
				return "", fmt.Errorf("%s", strings.TrimSpace(string(out)))
			}
			return string(out), nil
		},
		cleanup: func() { os.Remove(keyFile.Name()) },
	}
}
