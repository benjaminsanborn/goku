package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os/exec"
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
	if _, ok := manifest.Services["api"]; !ok {
		return nil, fmt.Errorf("goku.yaml declares no api service — nothing to deploy")
	}
	if !gitrepo.HasFile(repo, branch, "Dockerfile") {
		return nil, fmt.Errorf("no Dockerfile on this branch — goku builds services from a Dockerfile")
	}
	// Host mounts give a container the machine (docker.sock, repos, proxy
	// config) — only the operator's own org may use them (self-hosting).
	if len(manifest.Services["api"].HostMounts) > 0 && org != s.Store.DefaultOrgID {
		return nil, fmt.Errorf("host_mounts is restricted to operator projects")
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

// autoDeployMain deploys main after a merge when the project is adopted
// (goku.yaml + Dockerfile); silently skips otherwise.
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

// runDeploy executes build → databases → run → health → route → supersede,
// appending progress to the deployment log as it goes.
func (s *Server) runDeploy(org string, p *store.Project, d *store.Deployment, manifest *deploy.Manifest, repo string) {
	ctx := context.Background()
	defer s.clearDeploying(p.ID)
	logf := func(format string, args ...any) {
		s.Store.AppendDeployLog(ctx, d.ID, fmt.Sprintf(format, args...))
	}
	fail := func(err error) {
		logf("FAILED: %v", err)
		s.Store.SetDeploymentState(ctx, d.ID, "failed", nil)
	}
	defer func() {
		if r := recover(); r != nil {
			fail(fmt.Errorf("panic: %v", r))
		}
	}()

	image, err := deploy.Build(repo, p.Name, d.SHA, logf)
	if err != nil {
		fail(err)
		return
	}
	pw, err := s.Store.AppDBPassword(ctx, p.ID)
	if err != nil {
		fail(err)
		return
	}
	env, err := deploy.EnsureAppDatabases(s.Deploy, p.Name, pw, manifest, logf)
	if err != nil {
		fail(err)
		return
	}
	// Precedence: manifest env < provisioned databases < secrets.
	for k, v := range manifest.Services["api"].Env {
		if _, taken := env[k]; !taken {
			env[k] = v
		}
	}
	if secrets, err := s.Store.SecretValues(ctx, p.ID); err == nil {
		for k, v := range secrets {
			env[k] = v
		}
		if len(secrets) > 0 {
			logf("injecting %d secret(s)", len(secrets))
		}
	}
	avoid := 0
	if active, err := s.Store.ActiveDeployment(ctx, p.ID); err == nil {
		avoid = active.Port
	}
	port := deploy.Port(p.Name, d.SHA, avoid)
	container, err := deploy.Run(p.Name, image, port, env, manifest.Services["api"].HostMounts, logf)
	if err != nil {
		fail(err)
		return
	}
	s.Store.SetDeploymentState(ctx, d.ID, "starting", map[string]any{"image": image, "port": port})

	api := manifest.Services["api"]
	if err := deploy.HealthCheck(port, api.HealthCheck, 120*time.Second, logf); err != nil {
		if out, _ := exec.Command("docker", "logs", "--tail", "40", container).CombinedOutput(); len(out) > 0 {
			logf("container logs:\n%s", string(out))
		}
		_ = exec.Command("docker", "rm", "-f", container).Run()
		fail(err)
		return
	}

	// Bookkeeping and routing happen BEFORE stopping the old container: for
	// self-hosted goku, "the old container" is the process running this very
	// pipeline — everything must be durable before it removes itself.
	s.Store.SupersedePrevious(ctx, p.ID, d.ID)
	host := p.Name + "." + s.Deploy.AppDomain
	if d.Domain != "" {
		host = d.Domain
	}
	url := "https://" + host
	s.Store.SetDeploymentState(ctx, d.ID, "healthy", map[string]any{"url": url})

	if healthy, err := s.Store.AllHealthyDeployments(ctx); err == nil {
		routes := map[string]int{}
		for _, hr := range healthy {
			h := hr.Project + "." + s.Deploy.AppDomain
			if hr.Domain != "" {
				h = hr.Domain
			}
			routes[h] = hr.Port
		}
		if err := deploy.WriteRoutes(s.Deploy, routes, logf); err != nil {
			logf("routing warning: %v", err)
		}
	}
	logf("live at %s", url)
	deploy.StopPrevious(p.Name, container, logf)
}
