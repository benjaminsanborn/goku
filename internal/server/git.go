package server

import (
	"context"
	"log"
	"net/http"
	"net/http/cgi"
	"path/filepath"
	"strings"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

// RepoPath is org-scoped: project names are only unique within an org.
func (s *Server) RepoPath(org, project string) string {
	return filepath.Join(s.DataDir, "repos", org, project+".git")
}

// gitHandler serves git smart HTTP by delegating to git http-backend.
// Push requests (git-receive-pack) are wrapped with a ref snapshot so pushes
// become audit events and update open changesets — the in-process equivalent
// of a post-receive hook.
func (s *Server) gitHandler() http.Handler {
	execPath, err := gitrepo.ExecPath()
	if err != nil {
		log.Fatalf("git not found: %v", err)
	}
	backend := &cgi.Handler{
		Path: filepath.Join(execPath, "git-http-backend"),
		Root: "/git",
		Env:  []string{"GIT_HTTP_EXPORT_ALL=1"},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		org, actor, ok := s.gitAuth(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Basic realm="goku git"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
			return
		}

		project := gitURLProject(r.URL.Path)
		if project == "" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		// Scoped lookup: a token can only reach its own org's repos.
		if _, err := s.Store.GetProject(r.Context(), org, project); err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		repo := s.RepoPath(org, project)
		if err := gitrepo.EnsureBareRepo(repo); err != nil {
			http.Error(w, "repo unavailable", http.StatusInternalServerError)
			return
		}

		isPush := strings.HasSuffix(r.URL.Path, "/git-receive-pack")
		var before map[string]string
		if isPush {
			before, _ = gitrepo.Refs(repo)
		}

		h := *backend
		h.Env = append(append([]string{}, backend.Env...),
			"REMOTE_USER="+actor,
			"GIT_PROJECT_ROOT="+filepath.Join(s.DataDir, "repos", org))
		h.ServeHTTP(w, r)

		if isPush {
			after, _ := gitrepo.Refs(repo)
			s.recordPush(r.Context(), org, project, actor, before, after)
		}
	})
}

// gitAuth accepts HTTP basic auth where the password is a goku token (root or
// org-scoped); the username names the actor (e.g. "claude").
func (s *Server) gitAuth(r *http.Request) (org, actor string, ok bool) {
	user, pass, basicOK := r.BasicAuth()
	if !basicOK {
		if token, bearerOK := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); bearerOK {
			user, pass = "claude", token
		} else {
			return "", "", false
		}
	}
	org, ok = s.resolveOrg(r.Context(), pass)
	if !ok {
		return "", "", false
	}
	if user == "" {
		user = "claude"
	}
	return org, "agent:" + user, true
}

func gitURLProject(path string) string {
	rest := strings.TrimPrefix(path, "/git/")
	name, _, found := strings.Cut(rest, ".git")
	if !found || name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func (s *Server) recordPush(ctx context.Context, org, project, actor string, before, after map[string]string) {
	for branch, sha := range after {
		if before[branch] == sha {
			continue
		}
		s.Store.RecordGitPush(ctx, org, project, actor, branch, sha)
	}
}
