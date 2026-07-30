package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
	"sort"
	"time"

	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"github.com/benjaminsanborn/goku/internal/store"
)

// handleDeploy kicks a kamal-style container deployment of a branch.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Branch == "" {
		in.Branch = "main"
	}
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	// Linked projects deploy what GitHub has: sync first.
	if p.Upstream != "" {
		_ = gitrepo.FetchUpstream(s.RepoPath(org, p.Name), s.upstreamFetchURL(r.Context(), org, p.Upstream))
	}
	d, err := s.startDeploy(r.Context(), org, p, in.Branch, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	w.WriteHeader(http.StatusAccepted)
	respond(w, d, nil)
}

// startDeploy validates a branch is deployable, records the deployment, and
// runs the pipeline async. Shared by the API, merge auto-deploy, and the
// GitHub webhook.
func (s *Server) startDeploy(ctx context.Context, org string, p *store.Project, branch, actor string) (*store.Deployment, error) {
	repo := s.RepoPath(org, p.Name)
	sha, err := gitrepo.Head(repo, branch)
	if err != nil {
		return nil, fmt.Errorf("branch %q not found", branch)
	}
	raw, err := gitrepo.FileAt(repo, branch, "goku.yaml")
	if err != nil {
		return nil, fmt.Errorf("no goku.yaml on this branch — adopt goku before deploying")
	}
	manifest, err := deploy.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if len(manifest.Services) == 0 {
		return nil, fmt.Errorf("goku.yaml declares no services — nothing to deploy")
	}
	needsDockerfile := false
	for _, svc := range manifest.Services {
		if svc.Type != "web" {
			needsDockerfile = true
		}
		// Host mounts give a container the machine — operator org only.
		if len(svc.HostMounts) > 0 && org != s.Store.DefaultOrgID {
			return nil, fmt.Errorf("host_mounts is restricted to operator projects")
		}
	}
	if needsDockerfile && !gitrepo.HasFile(repo, branch, "Dockerfile") {
		return nil, fmt.Errorf("no Dockerfile on this branch — goku builds services from a Dockerfile")
	}
	domain := ""
	if len(manifest.Routes) > 0 && manifest.Routes[0].Domain != "default" {
		domain = manifest.Routes[0].Domain
	}

	s.syncMu.Lock()
	if s.deploying == nil {
		s.deploying = map[string]bool{}
	}
	if s.deploying[p.ID] {
		s.syncMu.Unlock()
		return nil, fmt.Errorf("a deployment is already in progress for this project")
	}
	s.deploying[p.ID] = true
	s.syncMu.Unlock()

	d, err := s.Store.CreateDeployment(ctx, org, p, branch, sha, actor, domain)
	if err != nil {
		s.clearDeploying(p.ID)
		return nil, err
	}
	go s.runDeploy(org, p, d, manifest, repo)
	return d, nil
}

// autoDeployMain deploys main after a merge when the project is adopted.
func (s *Server) autoDeployMain(org string, p *store.Project) {
	ctx := context.Background()
	repo := s.RepoPath(org, p.Name)
	if !gitrepo.HasFile(repo, "main", "goku.yaml") || !gitrepo.HasFile(repo, "main", "Dockerfile") {
		return
	}
	if _, err := s.startDeploy(ctx, org, p, "main", "system:merge"); err != nil {
		log.Printf("auto-deploy %s: %v", p.Name, err)
	}
}

func (s *Server) clearDeploying(projectID string) {
	s.syncMu.Lock()
	delete(s.deploying, projectID)
	s.syncMu.Unlock()
}

// runDeploy materializes the manifest: one long-lived postgres container per
// database resource, one fresh container per service (api from the
// Dockerfile, web as a synthesized static server), health checks, path-based
// routing, then supersede — stopping the old containers last so a control
// plane can replace itself.
func (s *Server) runDeploy(org string, p *store.Project, d *store.Deployment, manifest *deploy.Manifest, repo string) {
	ctx := context.Background()
	defer s.clearDeploying(p.ID)
	logf := func(format string, args ...any) {
		s.Store.AppendDeployLog(ctx, d.ID, fmt.Sprintf(format, args...))
	}
	keep := map[string]bool{}
	fail := func(err error) {
		logf("FAILED: %v", err)
		// Remove any containers this deployment already started — the
		// previous deployment keeps serving.
		for c := range keep {
			_ = exec.Command("docker", "rm", "-f", c).Run()
		}
		s.Store.SetDeploymentState(ctx, d.ID, "failed", nil)
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Errorf("panic: %v", r))
		}
	}()

	// Databases first: long-lived containers, shared across deployments.
	pw, err := s.Store.AppDBPassword(ctx, p.ID)
	if err != nil {
		fail(err)
		return
	}
	dbEnv, err := deploy.EnsureDatabaseContainers(p.Name, pw, manifest, logf)
	if err != nil {
		fail(err)
		return
	}
	secrets, _ := s.Store.SecretValues(ctx, p.ID)

	// Deterministic service order: api first, then the rest alphabetically.
	names := []string{}
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		if names[i] == "api" {
			return true
		}
		if names[j] == "api" {
			return false
		}
		return names[i] < names[j]
	})

	var baseImage string
	usedPorts := map[int]bool{}
	svcPorts := map[string]int{}

	for _, name := range names {
		svc := manifest.Services[name]
		image := ""
		switch svc.Type {
		case "web":
			image, err = deploy.BuildWebImage(repo, p.Name, d.SHA, svc, logf)
		default:
			if baseImage == "" {
				baseImage, err = deploy.Build(repo, p.Name, d.SHA, logf)
			}
			image = baseImage
		}
		if err != nil {
			fail(err)
			return
		}

		env := map[string]string{}
		for k, v := range svc.Env {
			env[k] = v
		}
		if svc.Type != "web" {
			// Precedence: manifest env < databases < secrets.
			for k, v := range dbEnv {
				env[k] = v
			}
			for k, v := range secrets {
				env[k] = v
			}
		}

		port := deploy.Port(p.Name, name, d.ID, usedPorts)
		usedPorts[port] = true
		svcPorts[name] = port

		container, err := deploy.Run(p.Name, name, image, port, env, svc.HostMounts, logf)
		if err != nil {
			fail(err)
			return
		}
		keep[container] = true
		if name == "api" {
			s.Store.SetDeploymentState(ctx, d.ID, "starting", map[string]any{"image": image, "port": port})
		}

		if err := deploy.HealthCheck(port, svc.HealthCheck, 120*time.Second, logf); err != nil {
			if out, _ := exec.Command("docker", "logs", "--tail", "40", container).CombinedOutput(); len(out) > 0 {
				logf("container logs (%s):\n%s", name, string(out))
			}
			fail(err)
			return
		}
	}

	// Site entries per host from manifest routes; default: api on the
	// project subdomain when no routes are declared.
	sites := map[string][]deploy.SiteEntry{}
	for _, rt := range manifest.Routes {
		port, ok := svcPorts[rt.Service]
		if !ok {
			continue
		}
		host := rt.Domain
		if host == "default" || host == "" {
			host = p.Name + "." + s.Deploy.AppDomain
		}
		sites[host] = append(sites[host], deploy.SiteEntry{Service: rt.Service, Paths: rt.Paths, Port: port})
	}
	if len(sites) == 0 {
		if port, ok := svcPorts["api"]; ok {
			sites[p.Name+"."+s.Deploy.AppDomain] = []deploy.SiteEntry{{Service: "api", Port: port}}
		}
	}
	routesJSON, _ := json.Marshal(sites)

	// Bookkeeping and routing happen BEFORE stopping the old containers: for
	// self-hosted goku, "the old containers" include the process running this
	// very pipeline.
	s.Store.SupersedePrevious(ctx, p.ID, d.ID)
	primaryHost := p.Name + "." + s.Deploy.AppDomain
	if d.Domain != "" {
		primaryHost = d.Domain
	}
	url := "https://" + primaryHost
	s.Store.SetDeploymentState(ctx, d.ID, "healthy", map[string]any{"url": url, "routes": string(routesJSON)})

	if healthy, err := s.Store.AllHealthyDeployments(ctx); err == nil {
		all := map[string][]deploy.SiteEntry{}
		for _, hr := range healthy {
			if hr.Routes != nil {
				var m map[string][]deploy.SiteEntry
				if json.Unmarshal(hr.Routes, &m) == nil {
					for host, entries := range m {
						all[host] = entries
					}
					continue
				}
			}
			// Legacy rows: single api port on the project subdomain.
			host := hr.Project + "." + s.Deploy.AppDomain
			if hr.Domain != "" {
				host = hr.Domain
			}
			all[host] = []deploy.SiteEntry{{Service: "api", Port: hr.Port}}
		}
		if err := deploy.WriteRoutes(s.Deploy, all, logf); err != nil {
			logf("routing warning: %v", err)
		}
	}
	logf("live at %s", url)
	deploy.StopPrevious(p.Name, keep, logf)
}
