package server

import (
	"context"
	"encoding/json"
	"fmt"
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
	repo := s.RepoPath(org, p.Name)

	// Linked projects deploy what GitHub has: sync first.
	if p.Upstream != "" {
		_ = gitrepo.FetchUpstream(repo, s.upstreamFetchURL(r.Context(), org, p.Upstream))
	}
	sha, err := gitrepo.Head(repo, in.Branch)
	if err != nil {
		httpError(w, http.StatusNotFound, "branch not found")
		return
	}
	raw, err := gitrepo.FileAt(repo, in.Branch, "goku.yaml")
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, "no goku.yaml on this branch — adopt goku before deploying")
		return
	}
	manifest, err := deploy.ParseManifest(raw)
	if err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	if _, ok := manifest.Services["api"]; !ok {
		httpError(w, http.StatusUnprocessableEntity, "goku.yaml declares no api service — nothing to deploy")
		return
	}

	s.syncMu.Lock()
	if s.deploying == nil {
		s.deploying = map[string]bool{}
	}
	if s.deploying[p.ID] {
		s.syncMu.Unlock()
		httpError(w, http.StatusConflict, "a deployment is already in progress for this project")
		return
	}
	s.deploying[p.ID] = true
	s.syncMu.Unlock()

	d, err := s.Store.CreateDeployment(r.Context(), org, p, in.Branch, sha, s.actorFrom(r))
	if err != nil {
		s.clearDeploying(p.ID)
		respond(w, nil, err)
		return
	}
	go s.runDeploy(org, p, d, manifest, repo)
	w.WriteHeader(http.StatusAccepted)
	respond(w, d, nil)
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
	// Manifest env is plain (non-secret) config; provisioned values win.
	for k, v := range manifest.Services["api"].Env {
		if _, taken := env[k]; !taken {
			env[k] = v
		}
	}
	port := deploy.Port(p.Name)
	container, err := deploy.Run(p.Name, image, port, env, logf)
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

	deploy.StopPrevious(p.Name, container, logf)
	s.Store.SupersedePrevious(ctx, p.ID, d.ID)
	url := "https://" + p.Name + "." + s.Deploy.AppDomain
	s.Store.SetDeploymentState(ctx, d.ID, "healthy", map[string]any{"url": url})

	if routes, err := s.Store.AllHealthyDeployments(ctx); err == nil {
		if err := deploy.WriteRoutes(s.Deploy, routes, logf); err != nil {
			logf("routing warning: %v", err)
		}
	}
	logf("live at %s", url)
}
