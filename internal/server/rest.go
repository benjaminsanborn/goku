// Package server exposes the platform domain over REST, MCP, and static UI serving.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/benjaminsanborn/goku/internal/store"
)

type ctxKey int

const orgKey ctxKey = iota

func orgFrom(ctx context.Context) string {
	org, _ := ctx.Value(orgKey).(string)
	return org
}

type Server struct {
	Store *store.Store
	// Token authenticates MCP, git, and write REST requests.
	Token   string
	WebDist string
	DataDir string
	BaseURL string
}

func (s *Server) Handler() http.Handler {
	// The whole /v1 surface requires the token: this server faces the
	// internet, and project code + audit data are as sensitive as writes.
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/projects", s.listProjects)
	api.HandleFunc("POST /v1/projects", s.handleCreateProject)
	api.HandleFunc("GET /v1/projects/{ref}", s.getProject)
	api.HandleFunc("GET /v1/projects/{ref}/changesets", s.listChangesets)
	api.HandleFunc("POST /v1/projects/{ref}/changesets", s.handleOpenChangeset)
	api.HandleFunc("GET /v1/changesets/{id}", s.getChangeset)
	api.HandleFunc("POST /v1/changesets/{id}/merge", s.handleMerge)
	api.HandleFunc("GET /v1/events", s.listEvents)
	api.HandleFunc("GET /v1/me", s.handleMe)

	mux := http.NewServeMux()
	mux.Handle("/v1/", s.requireToken(api))
	mux.Handle("/git/", s.gitHandler())
	mux.Handle("/mcp", s.requireToken(s.mcpHandler()))
	mux.Handle("/", s.spaHandler())

	return cors(mux)
}

// Organizations are provisioned by the operator on the server host
// (gokud create-org); there is deliberately no network signup surface.

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	org, err := s.Store.GetOrg(r.Context(), orgFrom(r.Context()))
	respond(w, map[string]any{"organization": org}, err)
}

// actorFrom attributes an authenticated write. The single-token dev slice
// can't distinguish identities, so the UI self-identifies as the operator;
// real token→identity mapping is designed in docs/design/06.
func (s *Server) actorFrom(r *http.Request) string {
	if r.Header.Get("X-Goku-Actor") == "operator" {
		return "user:operator"
	}
	return "agent:claude"
}

func (s *Server) listProjects(w http.ResponseWriter, r *http.Request) {
	projects, err := s.Store.ListProjects(r.Context(), orgFrom(r.Context()))
	respond(w, map[string]any{"projects": projects}, err)
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	p, err := s.createProject(r.Context(), orgFrom(r.Context()), in.Name, s.actorFrom(r))
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"project": p, "git_remote": s.gitRemoteURL(p.Name)}, nil)
}

func (s *Server) handleOpenChangeset(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Title       string       `json:"title"`
		Description string       `json:"description"`
		Branch      string       `json:"branch"`
		Files       []store.File `json:"files"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	cs, err := s.openChangeset(r.Context(), orgFrom(r.Context()), r.PathValue("ref"), in.Title, in.Description, in.Branch, s.actorFrom(r), in.Files)
	respond(w, cs, err)
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	cs, err := s.mergeChangeset(r.Context(), orgFrom(r.Context()), r.PathValue("id"), s.actorFrom(r))
	respond(w, cs, err)
}

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	respond(w, p, err)
}

func (s *Server) listChangesets(w http.ResponseWriter, r *http.Request) {
	changesets, err := s.Store.ListChangesets(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	respond(w, map[string]any{"changesets": changesets}, err)
}

func (s *Server) getChangeset(w http.ResponseWriter, r *http.Request) {
	cs, err := s.Store.GetChangeset(r.Context(), orgFrom(r.Context()), r.PathValue("id"))
	respond(w, cs, err)
}

func (s *Server) listEvents(w http.ResponseWriter, r *http.Request) {
	events, err := s.Store.ListAuditEvents(r.Context(), orgFrom(r.Context()), 50)
	respond(w, map[string]any{"events": events}, err)
}

// resolveOrg maps a bearer token to an organization: the root GOKU_TOKEN owns
// the default org; signup-minted tokens resolve via the tokens table.
func (s *Server) resolveOrg(ctx context.Context, token string) (string, bool) {
	if token != "" && token == s.Token {
		return s.Store.DefaultOrgID, true
	}
	org, err := s.Store.ResolveToken(ctx, token)
	if err != nil {
		return "", false
	}
	return org, true
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			httpError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		org, ok := s.resolveOrg(r.Context(), token)
		if !ok {
			httpError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), orgKey, org)))
	})
}

// spaHandler serves the built UI with an index.html fallback for client-side routes.
func (s *Server) spaHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := filepath.Join(s.WebDist, filepath.Clean("/"+r.URL.Path))
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			http.ServeFile(w, r, path)
			return
		}
		http.ServeFile(w, r, filepath.Join(s.WebDist, "index.html"))
	})
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if strings.HasPrefix(origin, "http://localhost") || strings.HasPrefix(origin, "http://127.0.0.1") {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Goku-Actor, Mcp-Session-Id, Mcp-Protocol-Version")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func respond(w http.ResponseWriter, v any, err error) {
	if err != nil {
		status := http.StatusInternalServerError
		msg := err.Error()
		if errors.Is(err, store.ErrNotFound) {
			status = http.StatusNotFound
		} else if strings.Contains(msg, "required") || strings.Contains(msg, "already exists") ||
			strings.Contains(msg, "fast-forward") || strings.Contains(msg, "pushed") ||
			strings.Contains(msg, "not open") || strings.Contains(msg, "target main") {
			status = http.StatusUnprocessableEntity
		}
		httpError(w, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
