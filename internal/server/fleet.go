package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	// Assignments: what each instance is running. Every deployment so far
	// runs on the local-driver instance; ssh targets come next.
	healthy, _ := s.Store.AllHealthyDeployments(r.Context())
	local := []string{}
	for _, h := range healthy {
		local = append(local, h.Project+" · main")
	}
	out := []map[string]any{}
	for _, i := range instances {
		assignments := []string{}
		if i.Driver == "local" {
			assignments = local
		}
		out = append(out, map[string]any{
			"instance": i, "assignments": assignments,
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
	inst, err := s.Store.CreateInstance(r.Context(), org, strings.ToLower(in.Name), "ssh", in.Address, in.SSHKey, s.actorFrom(r))
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

func (s *Server) handleDeleteInstance(w http.ResponseWriter, r *http.Request) {
	err := s.Store.DeleteInstance(r.Context(), orgFrom(r.Context()), r.PathValue("id"), s.actorFrom(r))
	respond(w, map[string]any{"deleted": true}, err)
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

	// ssh driver: key to a 0600 temp file; host keys pinned per data dir.
	keyFile, _ := os.CreateTemp("", "goku-ssh-*")
	keyFile.Chmod(0o600)
	keyFile.WriteString(inst.SSHKey)
	if !strings.HasSuffix(inst.SSHKey, "\n") {
		keyFile.WriteString("\n")
	}
	keyFile.Close()

	target, port := inst.Address, "22"
	if host, p, ok := strings.Cut(inst.Address, ":"); ok {
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
