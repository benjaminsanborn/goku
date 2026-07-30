package server

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

// conventionalKind extracts the conventionalbranch.org prefix (feature,
// bugfix, hotfix, release, chore) from a branch name; empty otherwise.
func conventionalKind(name string) string {
	prefix, _, ok := strings.Cut(name, "/")
	if !ok {
		return ""
	}
	switch prefix {
	case "feature", "bugfix", "hotfix", "release", "chore":
		return prefix
	}
	return ""
}

type branchView struct {
	gitrepo.Branch
	Kind   string `json:"kind"`
	Merged bool   `json:"merged"`
}

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	repo := s.RepoPath(orgFrom(r.Context()), p.Name)
	branches, err := gitrepo.Branches(repo)
	if err != nil {
		respond(w, nil, err)
		return
	}
	views := []branchView{}
	for _, b := range branches {
		v := branchView{Branch: b, Kind: conventionalKind(b.Name)}
		if b.Name != "main" {
			v.Merged = gitrepo.IsMerged(repo, b.Name)
		}
		views = append(views, v)
	}
	respond(w, map[string]any{"branches": views}, nil)
}

// handleBranchDetail returns a branch's state relative to main: ahead/behind,
// merged, and the file diff — the review surface for the branch-based flow.
func (s *Server) handleBranchDetail(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		httpError(w, http.StatusUnprocessableEntity, "branch name is required")
		return
	}
	repo := s.RepoPath(orgFrom(r.Context()), p.Name)
	sha, err := gitrepo.Head(repo, name)
	if err != nil {
		httpError(w, http.StatusNotFound, "branch not found")
		return
	}
	ahead, behind := gitrepo.AheadBehind(repo, name)
	files, err := gitrepo.DiffFiles(repo, name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{
		"name": name, "sha": sha, "kind": conventionalKind(name),
		"merged": name != "main" && gitrepo.IsMerged(repo, name),
		"ahead":  ahead, "behind": behind,
		"files": storeFiles(files),
	}, nil)
}

func (s *Server) handleMergeBranch(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Branch string `json:"branch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	mainSHA, err := s.mergeBranch(r.Context(), orgFrom(r.Context()), r.PathValue("ref"), in.Branch, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"merged": in.Branch, "main": mainSHA}, nil)
}

// handleManifest returns the parsed goku.yaml at a branch tip, normalized for
// the UI's architecture diagram.
func (s *Server) handleManifest(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	branch := r.URL.Query().Get("branch")
	if branch == "" {
		branch = "main"
	}
	repo := s.RepoPath(orgFrom(r.Context()), p.Name)
	raw, err := gitrepo.FileAt(repo, branch, "goku.yaml")
	if err != nil {
		respond(w, map[string]any{"adopted": false, "branch": branch}, nil)
		return
	}
	respond(w, parseManifestView(raw, branch), nil)
}

// handleDeployments is a stub until the deploy pipeline ships: the UI is
// built against it so deployment history has a home from day one.
func (s *Server) handleDeployments(w http.ResponseWriter, r *http.Request) {
	if _, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref")); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"deployments": []any{}}, nil)
}

type serviceView struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Size        string `json:"size,omitempty"`
	Port        int    `json:"port,omitempty"`
	HealthCheck string `json:"health_check,omitempty"`
}

type resourceView struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type routeView struct {
	Domain  string `json:"domain"`
	Service string `json:"service"`
}

func parseManifestView(raw, branch string) map[string]any {
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(raw), &doc); err != nil {
		return map[string]any{"adopted": true, "branch": branch, "error": "goku.yaml does not parse: " + err.Error()}
	}
	services := []serviceView{}
	if m, ok := doc["services"].(map[string]any); ok {
		for name, v := range m {
			sv := serviceView{Name: name}
			if spec, ok := v.(map[string]any); ok {
				sv.Type, _ = spec["type"].(string)
				sv.Size, _ = spec["size"].(string)
				sv.HealthCheck, _ = spec["health_check"].(string)
				if port, ok := spec["port"].(int); ok {
					sv.Port = port
				}
			}
			services = append(services, sv)
		}
	}
	resources := []resourceView{}
	if m, ok := doc["resources"].(map[string]any); ok {
		for name, v := range m {
			rv := resourceView{Name: name}
			if spec, ok := v.(map[string]any); ok {
				rv.Type, _ = spec["type"].(string)
			}
			resources = append(resources, rv)
		}
	}
	routes := []routeView{}
	if list, ok := doc["routes"].([]any); ok {
		for _, v := range list {
			if spec, ok := v.(map[string]any); ok {
				rt := routeView{}
				rt.Domain, _ = spec["domain"].(string)
				rt.Service, _ = spec["service"].(string)
				routes = append(routes, rt)
			}
		}
	}
	sort.Slice(services, func(i, j int) bool { return services[i].Name < services[j].Name })
	sort.Slice(resources, func(i, j int) bool { return resources[i].Name < resources[j].Name })
	return map[string]any{
		"adopted": true, "branch": branch,
		"services": services, "resources": resources, "routes": routes,
	}
}
