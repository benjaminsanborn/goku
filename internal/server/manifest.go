package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"gopkg.in/yaml.v3"
)

// The visual architecture builder edits goku.yaml through this endpoint: the
// UI sends the shapes on its canvas, the server renders them back into the
// manifest and commits it.
//
// Rendering happens here rather than in the browser for two reasons. The
// builder models only what it can draw, so writing a whole file from canvas
// state would silently drop anything it doesn't — env blocks, host_mounts, a
// comment-only key someone added by hand. Instead each service is merged over
// its existing mapping, and untouched keys survive. And because the same code
// path serves dry runs, the YAML preview in the UI is exactly the bytes that
// will be committed.

// nameRe matches manifest keys: lowercase, digit, dash, underscore.
var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]*$`)

type manifestInput struct {
	Branch  string `json:"branch"`
	Message string `json:"message"`
	DryRun  bool   `json:"dry_run"`

	Services []struct {
		Name        string `json:"name"`
		Type        string `json:"type"`
		Size        string `json:"size,omitempty"`
		Port        int    `json:"port,omitempty"`
		HealthCheck string `json:"health_check,omitempty"`
		Target      string `json:"target,omitempty"`
		Dist        string `json:"dist,omitempty"`
		SPA         bool   `json:"spa,omitempty"`
	} `json:"services"`
	Resources []struct {
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"resources"`
	Routes []struct {
		Domain  string   `json:"domain"`
		Service string   `json:"service"`
		Paths   []string `json:"paths,omitempty"`
	} `json:"routes"`
	// Layout is the canvas position of each node, keyed "service/api",
	// "resource/db", "route/0". It round-trips through goku.yaml so the
	// diagram an operator arranged is the one they see next time.
	Layout map[string][2]int `json:"layout"`
}

func (s *Server) handleSaveManifest(w http.ResponseWriter, r *http.Request) {
	org := orgFrom(r.Context())
	p, err := s.Store.GetProject(r.Context(), org, r.PathValue("ref"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	var in manifestInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	branch := strings.TrimSpace(in.Branch)
	if branch == "" {
		branch = "main"
	}
	if err := validateManifestInput(in); err != nil {
		httpError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}

	repo := s.RepoPath(org, p.Name)
	existing, _ := gitrepo.FileAt(repo, branch, "goku.yaml")
	rendered, err := renderManifest(existing, in)
	if err != nil {
		respond(w, nil, err)
		return
	}
	// The manifest the builder produced has to be one the deploy engine can
	// read back — catch that here, not at deploy time.
	if _, err := deploy.ParseManifest(rendered); err != nil {
		respond(w, nil, err)
		return
	}
	if in.DryRun {
		respond(w, map[string]any{"yaml": rendered, "branch": branch}, nil)
		return
	}
	if rendered == existing {
		respond(w, map[string]any{"yaml": rendered, "branch": branch, "committed": false}, nil)
		return
	}

	message := strings.TrimSpace(in.Message)
	if message == "" {
		message = "Update goku.yaml from the architecture builder"
	}
	sha, err := gitrepo.CommitToBranch(repo, branch, message, s.actorFrom(r),
		[]gitrepo.File{{Path: "goku.yaml", Content: rendered}})
	if err != nil {
		respond(w, nil, err)
		return
	}
	s.Store.Audit(r.Context(), org, s.actorFrom(r), "manifest.save", "project/"+p.Name,
		map[string]any{"branch": branch, "sha": sha})
	respond(w, map[string]any{"yaml": rendered, "branch": branch, "committed": true, "sha": sha}, nil)
}

func validateManifestInput(in manifestInput) error {
	if len(in.Services) == 0 {
		return fmt.Errorf("a manifest needs at least one service — add an api or web service to the canvas")
	}
	seen := map[string]bool{}
	services := map[string]bool{}
	for _, svc := range in.Services {
		if !nameRe.MatchString(svc.Name) {
			return fmt.Errorf("service name %q must start with a letter and use only lowercase letters, digits, - and _", svc.Name)
		}
		if seen["service/"+svc.Name] {
			return fmt.Errorf("two services are both named %q", svc.Name)
		}
		if svc.Type != "api" && svc.Type != "web" {
			return fmt.Errorf("service %q has type %q — services are api or web", svc.Name, svc.Type)
		}
		if svc.Port < 0 || svc.Port > 65535 {
			return fmt.Errorf("service %q has port %d, which is not a port", svc.Name, svc.Port)
		}
		seen["service/"+svc.Name] = true
		services[svc.Name] = true
	}
	for _, res := range in.Resources {
		if !nameRe.MatchString(res.Name) {
			return fmt.Errorf("resource name %q must start with a letter and use only lowercase letters, digits, - and _", res.Name)
		}
		if seen["resource/"+res.Name] {
			return fmt.Errorf("two resources are both named %q", res.Name)
		}
		if res.Type != "database" && res.Type != "storage" {
			return fmt.Errorf("resource %q has type %q — resources are database or storage", res.Name, res.Type)
		}
		seen["resource/"+res.Name] = true
	}
	for _, rt := range in.Routes {
		if strings.TrimSpace(rt.Domain) == "" {
			return fmt.Errorf("a route is missing its domain")
		}
		if !services[rt.Service] {
			return fmt.Errorf("route %s points at %q, which is not a service on the canvas", rt.Domain, rt.Service)
		}
	}
	return nil
}

// renderManifest merges the canvas over the existing document so keys the
// builder doesn't model survive the round trip.
func renderManifest(existing string, in manifestInput) (string, error) {
	doc := map[string]any{}
	if existing != "" {
		if err := yaml.Unmarshal([]byte(existing), &doc); err != nil {
			return "", fmt.Errorf("the goku.yaml already on this branch does not parse, so the builder won't overwrite it: %w", err)
		}
	}
	prevServices, _ := doc["services"].(map[string]any)
	prevResources, _ := doc["resources"].(map[string]any)

	services := map[string]any{}
	for _, svc := range in.Services {
		spec := carryOver(prevServices, svc.Name, "type", "size", "port", "health_check", "target", "dist", "spa")
		spec["type"] = svc.Type
		setOrDrop(spec, "size", svc.Size)
		setOrDrop(spec, "health_check", svc.HealthCheck)
		if svc.Port > 0 {
			spec["port"] = svc.Port
		} else {
			delete(spec, "port")
		}
		// Web-only keys: dropped when a node is switched to an api service.
		if svc.Type == "web" {
			setOrDrop(spec, "target", svc.Target)
			setOrDrop(spec, "dist", svc.Dist)
			if svc.SPA {
				spec["spa"] = true
			} else {
				delete(spec, "spa")
			}
		} else {
			delete(spec, "target")
			delete(spec, "dist")
			delete(spec, "spa")
		}
		services[svc.Name] = spec
	}

	resources := map[string]any{}
	for _, res := range in.Resources {
		spec := carryOver(prevResources, res.Name, "type")
		spec["type"] = res.Type
		resources[res.Name] = spec
	}

	routes := []any{}
	for _, rt := range in.Routes {
		entry := map[string]any{"domain": rt.Domain, "service": rt.Service}
		if len(rt.Paths) > 0 {
			entry["paths"] = rt.Paths
		}
		routes = append(routes, entry)
	}

	// [x, y] rather than a mapping: it stays on one line, and avoids YAML
	// 1.1's habit of quoting a bare "y" key as a boolean.
	layout := map[string]any{}
	for key, pos := range in.Layout {
		node := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		node.Content = []*yaml.Node{
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(pos[0])},
			{Kind: yaml.ScalarNode, Tag: "!!int", Value: fmt.Sprint(pos[1])},
		}
		layout[key] = node
	}

	// A struct fixes the order of the top-level sections; maps inside them
	// marshal with sorted keys, which is stable across saves either way.
	out := struct {
		Services  map[string]any `yaml:"services"`
		Resources map[string]any `yaml:"resources,omitempty"`
		Routes    []any          `yaml:"routes,omitempty"`
		Layout    map[string]any `yaml:"layout,omitempty"`
		Extra     map[string]any `yaml:",inline"`
	}{
		Services:  services,
		Resources: resources,
		Routes:    routes,
		Layout:    layout,
		Extra:     extras(doc),
	}

	var b strings.Builder
	b.WriteString("# goku.yaml — edited with the architecture builder.\n")
	b.WriteString("# layout holds canvas positions only; the deploy engine ignores it.\n\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(out); err != nil {
		return "", err
	}
	if err := enc.Close(); err != nil {
		return "", err
	}
	return b.String(), nil
}

// extras are the document's other top-level keys — anything a hand-edited
// manifest declares that the builder has no opinion about.
func extras(doc map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range doc {
		switch k {
		case "services", "resources", "routes", "layout":
		default:
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// carryOver copies an existing spec, keeping keys the builder does not model
// (env, host_mounts, anything hand-written) and clearing the ones it owns.
func carryOver(prev map[string]any, name string, owned ...string) map[string]any {
	spec := map[string]any{}
	if prev != nil {
		if m, ok := prev[name].(map[string]any); ok {
			for k, v := range m {
				spec[k] = v
			}
		}
	}
	for _, k := range owned {
		delete(spec, k)
	}
	return spec
}

func setOrDrop(spec map[string]any, key, value string) {
	if v := strings.TrimSpace(value); v != "" {
		spec[key] = v
		return
	}
	delete(spec, key)
}
