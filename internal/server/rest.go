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
	"sync"
	"time"

	"github.com/benjaminsanborn/goku/internal/deploy"
	"github.com/benjaminsanborn/goku/internal/store"
)

type ctxKey int

const (
	orgKey ctxKey = iota
	userKey
)

func orgFrom(ctx context.Context) string {
	org, _ := ctx.Value(orgKey).(string)
	return org
}

func userFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}

type Server struct {
	Store *store.Store
	// Token authenticates MCP, git, and write REST requests.
	Token   string
	WebDist string
	DataDir string
	BaseURL string
	OAuth   OAuthConfig
	Deploy  deploy.Target
	// WebhookSecret verifies GitHub push webhooks (deploy-on-merge for linked repos).
	WebhookSecret string

	syncMu    sync.Mutex
	lastSync  map[string]time.Time // project id → last upstream fetch
	deploying map[string]bool      // project id → deployment in flight
}

func (s *Server) Handler() http.Handler {
	// The whole /v1 surface requires the token: this server faces the
	// internet, and project code + audit data are as sensitive as writes.
	api := http.NewServeMux()
	api.HandleFunc("GET /v1/projects", s.listProjects)
	api.HandleFunc("POST /v1/projects", s.handleCreateProject)
	api.HandleFunc("GET /v1/projects/{ref}", s.getProject)
	api.HandleFunc("GET /v1/projects/{ref}/branches", s.handleBranches)
	api.HandleFunc("GET /v1/projects/{ref}/branch", s.handleBranchDetail)
	api.HandleFunc("POST /v1/projects/{ref}/merge", s.handleMergeBranch)
	api.HandleFunc("POST /v1/projects/{ref}/sync", s.handleSync)
	api.HandleFunc("POST /v1/projects/{ref}/deploy", s.handleDeploy)
	api.HandleFunc("POST /v1/projects/{ref}/environments/stop", s.handleStopEnv)
	api.HandleFunc("PUT /v1/projects/{ref}/secrets", s.handleSetSecret)
	api.HandleFunc("GET /v1/projects/{ref}/secrets", s.handleListSecrets)
	api.HandleFunc("DELETE /v1/projects/{ref}/secrets/{key}", s.handleDeleteSecret)
	api.HandleFunc("GET /v1/projects/{ref}/manifest", s.handleManifest)
	api.HandleFunc("GET /v1/projects/{ref}/deployments", s.handleDeployments)
	api.HandleFunc("GET /v1/projects/{ref}/services", s.handleServices)
	api.HandleFunc("GET /v1/projects/{ref}/logs", s.handleServiceLogs)
	api.HandleFunc("GET /v1/instances", s.handleListInstances)
	api.HandleFunc("POST /v1/instances", s.handleAddInstance)
	api.HandleFunc("POST /v1/instances/{id}/verify", s.handleVerifyInstance)
	api.HandleFunc("DELETE /v1/instances/{id}", s.handleDeleteInstance)
	api.HandleFunc("GET /v1/events", s.listEvents)
	api.HandleFunc("GET /v1/me", s.handleMe)
	api.HandleFunc("POST /v1/orgs/join", s.handleJoinOrg)
	api.HandleFunc("POST /v1/projects/import", s.handleImport)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /auth/providers", s.handleProviders)
	mux.HandleFunc("GET /auth/github", s.handleAuthStart(s.githubOAuth))
	mux.HandleFunc("GET /auth/github/callback", s.handleAuthCallback("github", s.githubOAuth))
	mux.HandleFunc("GET /auth/google", s.handleAuthStart(s.googleOAuth))
	mux.HandleFunc("GET /auth/google/callback", s.handleAuthCallback("google", s.googleOAuth))
	mux.HandleFunc("POST /v1/logout", s.handleLogout)
	mux.HandleFunc("POST /hooks/github", s.handleGitHubWebhook)
	mux.Handle("/v1/", s.requireAuth(api))
	mux.Handle("/git/", s.gitHandler())
	mux.Handle("/", s.spaHandler())

	return cors(mux)
}

// Organizations are provisioned by the operator on the server host
// (gokud create-org); there is deliberately no network signup surface.

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{}
	if user := userFrom(r.Context()); user != nil {
		out["user"] = user
		orgs, err := s.Store.UserOrgs(r.Context(), user.ID)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out["organizations"] = orgs
	}
	if orgID := orgFrom(r.Context()); orgID != "" {
		org, err := s.Store.GetOrg(r.Context(), orgID)
		if err != nil {
			respond(w, nil, err)
			return
		}
		out["organization"] = org
	} else {
		out["organization"] = nil
	}
	respond(w, out, nil)
}

// actorFrom attributes an authenticated write: session users act as
// themselves; token callers are agents (or the operator UI via header).
func (s *Server) actorFrom(r *http.Request) string {
	if user := userFrom(r.Context()); user != nil {
		return "user:" + user.Email
	}
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

func (s *Server) getProject(w http.ResponseWriter, r *http.Request) {
	p, err := s.Store.GetProject(r.Context(), orgFrom(r.Context()), r.PathValue("ref"))
	respond(w, p, err)
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

// requireAuth accepts a browser session cookie (human) or a bearer token
// (agent/CLI). Session users without an org membership may only reach /v1/me
// and /v1/orgs/join — enough to see who they are and redeem an invite.
func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		if c, err := r.Cookie(sessionCookie); err == nil {
			user, org, err := s.Store.ResolveSession(ctx, c.Value)
			if err == nil {
				if org == "" && r.URL.Path != "/v1/me" && r.URL.Path != "/v1/orgs/join" {
					httpError(w, http.StatusForbidden, "join an organization first")
					return
				}
				ctx = context.WithValue(ctx, userKey, user)
				ctx = context.WithValue(ctx, orgKey, org)
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
		}

		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			httpError(w, http.StatusUnauthorized, "sign in or provide a bearer token")
			return
		}
		org, ok := s.resolveOrg(ctx, token)
		if !ok {
			httpError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, orgKey, org)))
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
