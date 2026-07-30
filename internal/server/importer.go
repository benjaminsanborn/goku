package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

var githubRepoRe = regexp.MustCompile(`^(?:https?://)?github\.com/([\w.-]+)/([\w.-]+?)(?:\.git)?/?$|^([\w.-]+)/([\w.-]+)$`)

// handleImport creates a goku project from an existing GitHub repository.
// The import is faithful: full history, branches, and tags, with main
// normalized as the protected default. Adopting the goku standard (adding
// goku.yaml etc.) happens afterwards through ordinary changesets.
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL  string `json:"url"`
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	owner, repo, ok := parseGitHubRef(in.URL)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "expected a GitHub repo like github.com/owner/name or owner/name")
		return
	}
	name := strings.ToLower(in.Name)
	if name == "" {
		name = strings.ToLower(repo)
	}
	org := orgFrom(r.Context())
	actor := s.actorFrom(r)

	// Private repos: prefer the acting user's GitHub token; token-authed
	// agents fall back to any org member who signed in with GitHub.
	ghToken := ""
	if user := userFrom(r.Context()); user != nil {
		ghToken = user.GitHubToken
	}
	if ghToken == "" {
		ghToken = s.Store.GitHubTokenForOrg(r.Context(), org)
	}

	p, err := s.Store.CreateProject(r.Context(), org, name, actor)
	if err != nil {
		respond(w, nil, err)
		return
	}
	cloneURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
	if ghToken != "" {
		cloneURL = fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", url.QueryEscape(ghToken), owner, repo)
	}
	repoPath := s.RepoPath(org, p.Name)
	if err := gitrepo.CloneBareFrom(cloneURL, repoPath); err != nil {
		// Don't leak the tokenized URL in errors.
		msg := strings.ReplaceAll(err.Error(), ghToken, "***")
		httpError(w, http.StatusBadGateway, "clone failed (is the repo accessible?): "+msg)
		return
	}
	// GitHub stays the source of truth for imported projects: goku mirrors it.
	// The clone left a (possibly tokenized) origin in git config — remove it;
	// the upstream link lives in the DB and tokens are injected per-fetch.
	gitrepo.RemoveOrigin(repoPath)
	p.Upstream = owner + "/" + repo
	if err := s.Store.SetProjectUpstream(r.Context(), p.ID, p.Upstream); err != nil {
		respond(w, nil, err)
		return
	}

	respond(w, map[string]any{
		"project":    p,
		"git_remote": s.gitRemoteURL(p.Name),
		"imported":   fmt.Sprintf("github.com/%s/%s", owner, repo),
		"adopted":    gitrepo.HasFile(repoPath, "main", "goku.yaml"),
	}, nil)
}

func parseGitHubRef(ref string) (owner, repo string, ok bool) {
	m := githubRepoRe.FindStringSubmatch(strings.TrimSpace(ref))
	if m == nil {
		return "", "", false
	}
	if m[1] != "" {
		return m[1], m[2], true
	}
	return m[3], m[4], true
}
