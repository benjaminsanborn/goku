package server

import (
	"encoding/json"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"strings"

	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

// serviceUnit is one deployed (or declared) runtime unit: a service
// container or a database container, with its placement.
type serviceUnit struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"` // service | database
	Type      string `json:"type"` // api, web, database…
	Container string `json:"container,omitempty"`
	Instance  string `json:"instance,omitempty"`
	Status    string `json:"status"` // running | stopped | not_deployed
	Uptime    string `json:"uptime,omitempty"`
	Image     string `json:"image,omitempty"`
	Port      int    `json:"port,omitempty"`
}

// handleServices enumerates the project's runtime units and where they run.
// Everything runs on the org's local-driver instance until fleet targeting
// lands — placement is per-unit in the payload so the UI is already
// multi-instance shaped.
func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}

	instance := "local"
	if instances, err := s.Store.ListInstances(r.Context(), org); err == nil {
		for _, i := range instances {
			if i.Driver == "local" {
				instance = i.Name
			}
		}
	}

	// Ports per service from the active deployment's route set.
	svcPorts := map[string]int{}
	if active, err := s.Store.ActiveDeployment(r.Context(), p.ID); err == nil && active.Routes != nil {
		var sites map[string][]deploy.SiteEntry
		if json.Unmarshal(active.Routes, &sites) == nil {
			for _, entries := range sites {
				for _, e := range entries {
					svcPorts[e.Service] = e.Port
				}
			}
		}
	}

	// Running containers, keyed by name.
	type dockerInfo struct{ image, status string }
	containers := map[string]dockerInfo{}
	for _, prefix := range []string{deploy.ServiceContainerPrefix(p.Name), "goku-db-"} {
		out, err := exec.Command("docker", "ps", "-a", "--filter", "name="+prefix, "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}").Output()
		if err != nil {
			continue
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			parts := strings.SplitN(line, "\t", 3)
			if len(parts) == 3 {
				containers[parts[0]] = dockerInfo{parts[1], parts[2]}
			}
		}
	}

	units := []serviceUnit{}
	if raw, err := gitrepo.FileAt(s.RepoPath(org, p.Name), "main", "goku.yaml"); err == nil {
		if manifest, err := deploy.ParseManifest(raw); err == nil {
			svcPrefix := deploy.ServiceContainerPrefix(p.Name)
			for name, svc := range manifest.Services {
				u := serviceUnit{Name: name, Kind: "service", Type: svc.Type, Instance: instance, Status: "not_deployed", Port: svcPorts[name]}
				// newest matching container: goku-svc-<p>-<name>-<ts>
				best := ""
				for cname := range containers {
					if strings.HasPrefix(cname, svcPrefix+name+"-") && cname > best {
						best = cname
					}
				}
				if best != "" {
					u.Container = best
					u.Image = containers[best].image
					u.Uptime = containers[best].status
					u.Status = "stopped"
					if strings.HasPrefix(containers[best].status, "Up") {
						u.Status = "running"
					}
				}
				units = append(units, u)
			}
			for name, res := range manifest.Resources {
				if res.Type != "database" {
					continue
				}
				cname := deploy.DBContainerName(p.Name, name)
				u := serviceUnit{Name: name, Kind: "database", Type: "database", Instance: instance,
					Container: cname, Status: "not_deployed", Port: deploy.DBPort(p.Name, name)}
				if info, ok := containers[cname]; ok {
					u.Image = info.image
					u.Uptime = info.status
					u.Status = "stopped"
					if strings.HasPrefix(info.status, "Up") {
						u.Status = "running"
					}
				}
				units = append(units, u)
			}
		}
	}
	sort.Slice(units, func(i, j int) bool {
		if units[i].Kind != units[j].Kind {
			return units[i].Kind == "service"
		}
		return units[i].Name < units[j].Name
	})
	respond(w, map[string]any{"units": units}, nil)
}

// handleServiceLogs streams docker logs for one of the project's containers
// (live tail with ?follow=1; the stream ends when the client disconnects).
func (s *Server) handleServiceLogs(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	container := r.URL.Query().Get("container")
	// The container must belong to this project.
	if !strings.HasPrefix(container, deploy.ServiceContainerPrefix(p.Name)) &&
		!strings.HasPrefix(container, deploy.DBContainerName(p.Name, "")) {
		httpError(w, http.StatusUnprocessableEntity, "container does not belong to this project")
		return
	}
	tail := 200
	if t, err := strconv.Atoi(r.URL.Query().Get("tail")); err == nil && t > 0 && t <= 5000 {
		tail = t
	}
	args := []string{"logs", "--tail", strconv.Itoa(tail), "--timestamps"}
	if r.URL.Query().Get("follow") == "1" {
		args = append(args, "-f")
	}
	args = append(args, container)

	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)

	flusher, _ := w.(http.Flusher)
	fw := &flushWriter{w: w, f: flusher}
	// CommandContext kills docker logs when the client goes away.
	cmd := exec.CommandContext(r.Context(), "docker", args...)
	cmd.Stdout = fw
	cmd.Stderr = fw
	_ = cmd.Run()
}

type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}
