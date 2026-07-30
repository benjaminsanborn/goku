package server

import (
	"net/http"
	"sort"

	"gopkg.in/yaml.v3"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

func (s *Server) handleBranches(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	branches, err := gitrepo.Branches(s.RepoPath(orgFrom(r.Context()), p.Name))
	respond(w, map[string]any{"branches": branches}, err)
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
