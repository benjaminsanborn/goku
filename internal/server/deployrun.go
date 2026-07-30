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
	"sort"
	"strings"
	"time"

	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"github.com/benjaminsanborn/goku/internal/store"
)

// handleDeploy kicks a kamal-style container deployment of a branch.
func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Branch   string `json:"branch"`
		Instance string `json:"instance"`
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
	inst, err := s.validatePlacement(r.Context(), org, in.Instance)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	d, err := s.startDeployOn(r.Context(), org, p, in.Branch, s.actorFrom(r), inst)
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
func (s *Server) localInstanceName(ctx context.Context, org string) string {
	if instances, err := s.Store.ListInstances(ctx, org); err == nil {
		for _, i := range instances {
			if i.Driver == "local" {
				return i.Name
			}
		}
	}
	return "local"
}

// validatePlacement enforces the fleet rules for an explicit instance
// choice: it must exist and be ready; ssh members are capacity-1. Returns
// the instance for the ssh driver (nil = local).
func (s *Server) validatePlacement(ctx context.Context, org, instance string) (*store.Instance, error) {
	if instance == "" || instance == s.localInstanceName(ctx, org) {
		return nil, nil
	}
	inst, err := s.Store.GetInstanceByName(ctx, org, instance)
	if err != nil {
		return nil, fmt.Errorf("no instance named %q in your fleet", instance)
	}
	if inst.Driver != "ssh" {
		return nil, nil
	}
	if inst.Status != "ready" {
		return nil, fmt.Errorf("instance %s is %s — re-check it in the Fleet tab", inst.Name, inst.Status)
	}
	if running, busy := s.Store.InstanceOccupied(ctx, inst.Name); busy {
		return nil, fmt.Errorf("instance %s is running %s — stop that environment first", inst.Name, running)
	}
	return inst, nil
}

// remoteFor prepares the ssh driver connection (temp key file; caller must
// call cleanup).
func (s *Server) remoteFor(inst *store.Instance) (*deploy.Remote, func(), error) {
	keyFile, err := os.CreateTemp("", "goku-deploy-key-*")
	if err != nil {
		return nil, nil, err
	}
	keyFile.Chmod(0o600)
	keyFile.WriteString(inst.SSHKey)
	if !strings.HasSuffix(inst.SSHKey, "\n") {
		keyFile.WriteString("\n")
	}
	keyFile.Close()
	target, port := inst.Address, "22"
	if h, p, ok := strings.Cut(inst.Address, ":"); ok {
		target, port = h, p
	}
	r := &deploy.Remote{Target: target, Port: port, KeyFile: keyFile.Name(),
		KnownHosts: filepath.Join(s.DataDir, "ssh_known_hosts")}
	return r, func() { os.Remove(keyFile.Name()) }, nil
}

func (s *Server) startDeploy(ctx context.Context, org string, p *store.Project, branch, actor string) (*store.Deployment, error) {
	return s.startDeployOn(ctx, org, p, branch, actor, nil)
}

func (s *Server) startDeployOn(ctx context.Context, org string, p *store.Project, branch, actor string, inst *store.Instance) (*store.Deployment, error) {
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
		if inst != nil {
			if svc.Type == "web" {
				return nil, fmt.Errorf("web services on remote instances aren't supported yet — api services and databases only")
			}
			if len(svc.HostMounts) > 0 {
				return nil, fmt.Errorf("host_mounts is local-instance only")
			}
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

	instName := s.localInstanceName(ctx, org)
	if inst != nil {
		instName = inst.Name
	}
	d, err := s.Store.CreateDeployment(ctx, org, p, branch, sha, actor, domain, instName)
	if err != nil {
		s.clearDeploying(p.ID)
		return nil, err
	}
	if inst != nil {
		go s.runRemoteDeploy(org, p, d, manifest, repo, inst)
	} else {
		go s.runDeploy(org, p, d, manifest, repo)
	}
	return d, nil
}

// runRemoteDeploy is the ssh driver: build on the instance from a piped tar,
// databases and services as remote containers, health over ssh, central
// routing dialing the instance.
func (s *Server) runRemoteDeploy(org string, p *store.Project, d *store.Deployment, manifest *deploy.Manifest, repo string, inst *store.Instance) {
	ctx := context.Background()
	defer s.clearDeploying(p.ID)
	logf := func(format string, args ...any) {
		s.Store.AppendDeployLog(ctx, d.ID, fmt.Sprintf(format, args...))
	}
	keep := map[string]bool{}
	rmt, cleanup, err := s.remoteFor(inst)
	if err != nil {
		logf("FAILED: %v", err)
		s.Store.SetDeploymentState(ctx, d.ID, "failed", nil)
		return
	}
	defer cleanup()
	fail := func(err error) {
		logf("FAILED: %v", err)
		// Remove containers this deployment started; older ones keep serving.
		for c := range keep {
			_, _ = rmt.RunCommand("docker rm -f " + c)
		}
		s.Store.SetDeploymentState(ctx, d.ID, "failed", nil)
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Errorf("panic: %v", r))
		}
	}()

	pw, err := s.Store.AppDBPassword(ctx, p.ID)
	if err != nil {
		fail(err)
		return
	}
	dbEnv, err := deploy.RemoteEnsureDatabaseContainers(p.Name, d.Branch, pw, manifest, rmt, logf)
	if err != nil {
		fail(err)
		return
	}
	secrets, _ := s.Store.SecretValues(ctx, p.ID)

	image, err := deploy.RemoteBuild(repo, p.Name, d.SHA, rmt, logf)
	if err != nil {
		fail(err)
		return
	}

	usedPorts := map[int]bool{}
	svcPorts := map[string]int{}
	names := []string{}
	for name := range manifest.Services {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		svc := manifest.Services[name]
		env := map[string]string{}
		for k, v := range svc.Env {
			env[k] = v
		}
		for k, v := range dbEnv {
			env[k] = v
		}
		for k, v := range secrets {
			env[k] = v
		}
		port := deploy.Port(p.Name, name, d.ID, usedPorts)
		usedPorts[port] = true
		svcPorts[name] = port
		container, err := deploy.RemoteRun(p.Name, d.Branch, name, image, port, env, rmt, logf)
		if err != nil {
			fail(err)
			return
		}
		keep[container] = true
		if name == "api" {
			s.Store.SetDeploymentState(ctx, d.ID, "starting", map[string]any{"image": image, "port": port})
		}
		if err := deploy.RemoteHealthCheck(rmt, port, svc.HealthCheck, 120*time.Second, logf); err != nil {
			fail(err)
			return
		}
	}

	envHost := deploy.HostSlug(d.Branch) + "--" + p.Name + "." + s.Deploy.AppDomain
	if d.Branch == "main" {
		envHost = p.Name + "." + s.Deploy.AppDomain
	}
	sites := map[string][]deploy.SiteEntry{}
	for _, rt := range manifest.Routes {
		port, ok := svcPorts[rt.Service]
		if !ok {
			continue
		}
		sites[envHost] = append(sites[envHost], deploy.SiteEntry{Service: rt.Service, Paths: rt.Paths, Port: port, Upstream: rmt.Host()})
	}
	if len(sites) == 0 {
		if port, ok := svcPorts["api"]; ok {
			sites[envHost] = []deploy.SiteEntry{{Service: "api", Port: port, Upstream: rmt.Host()}}
		}
	}
	routesJSON, _ := json.Marshal(sites)

	s.Store.SupersedePrevious(ctx, p.ID, d.Branch, d.ID)
	url := "https://" + envHost
	s.Store.SetDeploymentState(ctx, d.ID, "healthy", map[string]any{"url": url, "routes": string(routesJSON)})
	s.regenRoutes(ctx, logf)
	logf("live at %s (on %s)", url, inst.Name)
	deploy.RemoteStopPrevious(p.Name, d.Branch, keep, rmt, logf)
}

// regenRoutes rebuilds the proxy config from every healthy deployment.
func (s *Server) regenRoutes(ctx context.Context, logf deploy.Logf) {
	healthy, err := s.Store.AllHealthyDeployments(ctx)
	if err != nil {
		return
	}
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
	dbEnv, err := deploy.EnsureDatabaseContainers(p.Name, d.Branch, pw, manifest, logf)
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

		container, err := deploy.Run(p.Name, d.Branch, name, image, port, env, svc.HostMounts, logf)
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

	// Site entries per host from manifest routes. main keeps its manifest
	// domains; branch environments live at <branch>--<project>.<domain>.
	envHost := p.Name + "." + s.Deploy.AppDomain
	if d.Branch != "main" {
		envHost = deploy.HostSlug(d.Branch) + "--" + p.Name + "." + s.Deploy.AppDomain
	}
	sites := map[string][]deploy.SiteEntry{}
	for _, rt := range manifest.Routes {
		port, ok := svcPorts[rt.Service]
		if !ok {
			continue
		}
		host := envHost
		if d.Branch == "main" && rt.Domain != "default" && rt.Domain != "" {
			host = rt.Domain
		}
		sites[host] = append(sites[host], deploy.SiteEntry{Service: rt.Service, Paths: rt.Paths, Port: port})
	}
	if len(sites) == 0 {
		if port, ok := svcPorts["api"]; ok {
			sites[envHost] = []deploy.SiteEntry{{Service: "api", Port: port}}
		}
	}
	routesJSON, _ := json.Marshal(sites)

	// Bookkeeping and routing happen BEFORE stopping the old containers: for
	// self-hosted goku, "the old containers" include the process running this
	// very pipeline.
	s.Store.SupersedePrevious(ctx, p.ID, d.Branch, d.ID)
	primaryHost := envHost
	if d.Branch == "main" && d.Domain != "" {
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
	deploy.StopPrevious(p.Name, d.Branch, keep, logf)
}

// handleStopEnv tears down a branch environment: containers stopped,
// deployments marked stopped, routes regenerated. main is not stoppable —
// deploy over it instead.
func (s *Server) handleStopEnv(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Branch == "" || in.Branch == "main" {
		httpError(w, http.StatusUnprocessableEntity, "main is the primary environment — deploy over it instead of stopping it")
		return
	}
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	// Stop containers wherever the environment lives.
	instName := ""
	if deployments, err := s.Store.ListDeployments(r.Context(), org, p.ID, 100); err == nil {
		for _, dep := range deployments {
			if dep.Branch == in.Branch && (dep.Status == "healthy" || dep.Status == "starting") {
				instName = dep.Instance
				break
			}
		}
	}
	if instName != "" && instName != s.localInstanceName(r.Context(), org) {
		if inst, err := s.Store.GetInstanceByName(r.Context(), org, instName); err == nil && inst.Driver == "ssh" {
			if rmt, cleanup, err := s.remoteFor(inst); err == nil {
				deploy.RemoteStopEnvironment(p.Name, in.Branch, rmt, func(string, ...any) {})
				cleanup()
			}
		}
	} else {
		deploy.StopEnvironment(p.Name, in.Branch, func(string, ...any) {})
	}
	s.Store.StopEnvironmentDeployments(r.Context(), org, p.ID, p.Name, in.Branch, s.actorFrom(r))
	if healthy, err := s.Store.AllHealthyDeployments(r.Context()); err == nil {
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
			host := hr.Project + "." + s.Deploy.AppDomain
			if hr.Domain != "" {
				host = hr.Domain
			}
			all[host] = []deploy.SiteEntry{{Service: "api", Port: hr.Port}}
		}
		_ = deploy.WriteRoutes(s.Deploy, all, func(string, ...any) {})
	}
	respond(w, map[string]any{"stopped": in.Branch}, nil)
}
