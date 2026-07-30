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

func (s *Server) RepoPath(project string) string {
	return filepath.Join(s.DataDir, "repos", project+".git")
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
		Env: []string{
			"GIT_PROJECT_ROOT=" + filepath.Join(s.DataDir, "repos"),
			"GIT_HTTP_EXPORT_ALL=1",
		},
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, ok := s.gitAuth(r)
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
		if _, err := s.Store.GetProject(r.Context(), project); err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		repo := s.RepoPath(project)
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
		h.Env = append(append([]string{}, backend.Env...), "REMOTE_USER="+actor)
		h.ServeHTTP(w, r)

		if isPush {
			after, _ := gitrepo.Refs(repo)
			s.recordPush(r.Context(), project, actor, before, after)
		}
	})
}

// gitAuth accepts HTTP basic auth where the password is the platform token;
// the username names the actor (e.g. "claude"). Bearer tokens also work.
func (s *Server) gitAuth(r *http.Request) (actor string, ok bool) {
	if user, pass, ok := r.BasicAuth(); ok && pass == s.Token {
		if user == "" {
			user = "claude"
		}
		return "agent:" + user, true
	}
	if r.Header.Get("Authorization") == "Bearer "+s.Token {
		return "agent:claude", true
	}
	return "", false
}

func gitURLProject(path string) string {
	rest := strings.TrimPrefix(path, "/git/")
	name, _, found := strings.Cut(rest, ".git")
	if !found || name == "" || strings.Contains(name, "/") || strings.Contains(name, "..") {
		return ""
	}
	return name
}

func (s *Server) recordPush(ctx context.Context, project, actor string, before, after map[string]string) {
	repo := s.RepoPath(project)
	for branch, sha := range after {
		if before[branch] == sha {
			continue
		}
		s.Store.RecordGitPush(ctx, project, actor, branch, sha)
		// Refresh any open changeset tracking this branch with the new head + diff.
		files, err := gitrepo.DiffFiles(repo, branch)
		if err != nil {
			continue
		}
		s.Store.RefreshChangesetForBranch(ctx, project, branch, sha, storeFiles(files))
	}
}
