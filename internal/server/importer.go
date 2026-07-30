package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
	"github.com/benjaminsanborn/goku/internal/store"
)

var githubRepoRe = regexp.MustCompile(`^(?:https?://)?github\.com/([\w.-]+)/([\w.-]+?)(?:\.git)?/?$|^([\w.-]+)/([\w.-]+)$`)

// handleImport creates a goku project from an existing GitHub repository:
// full history becomes the project repo, and an "Adopt goku standard"
// changeset proposes the goku scaffolding for human review.
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

	cs, err := s.openChangeset(r.Context(), org, p.ID,
		"Adopt goku standard",
		adoptionDescription(repoPath),
		"goku/adopt", actor, adoptionFiles(repoPath, p.Name))
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{
		"project":    p,
		"changeset":  cs,
		"git_remote": s.gitRemoteURL(p.Name),
		"imported":   fmt.Sprintf("github.com/%s/%s", owner, repo),
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

func adoptionFiles(repoPath, project string) []store.File {
	manifest := "# goku.yaml — declares what this project needs.\n" +
		"# Goku materializes these as AWS resources on merge; locally,\n" +
		"# 'goku dev' runs cognates (postgres, minio) with the same env contract.\n\nservices:\n"
	if gitrepo.HasFile(repoPath, "main", "Dockerfile") {
		manifest += "  api:\n    type: api            # built from the existing Dockerfile\n    size: small\n    port: 8080           # adjust to the port this app listens on\n    health_check: /\n"
	} else {
		manifest += "  # api:                 # add once a Dockerfile exists (see changeset notes)\n  #   type: api\n  #   size: small\n  #   port: 8080\n  #   health_check: /\n"
	}
	manifest += "\nresources: {}\n  # 'goku add database main' / 'goku add storage assets'\n\nroutes:\n  - domain: default\n    service: api\n"

	return []store.File{
		{Path: "goku.yaml", Content: manifest},
		{Path: ".mcp.json", Content: "{\n  \"mcpServers\": {\n    \"goku\": { \"command\": \"goku\", \"args\": [\"mcp\"] }\n  }\n}\n"},
	}
}

func adoptionDescription(repoPath string) string {
	d := "Imported from GitHub with full history. This changeset adds the goku scaffolding:\n" +
		"- goku.yaml — the resource manifest (single source of truth for infra)\n" +
		"- .mcp.json — gives any Claude opened in this workspace the goku tools\n\nTo finish adoption:\n"
	if gitrepo.HasFile(repoPath, "main", "Dockerfile") {
		d += "- Verify the existing Dockerfile builds and the port in goku.yaml matches the app\n"
	} else {
		d += "- Add a Dockerfile (goku builds services from it) and uncomment the api service in goku.yaml\n"
	}
	d += "- Declare resources the app needs (goku add database main, goku add storage assets)\n" +
		"- Wire the app to the env contract (DATABASE_URL, STORAGE_ENDPOINT, …)"
	return d
}
