package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/benjaminsanborn/goku/internal/gitrepo"
)

// handleGitHubWebhook receives push events from linked repos: sync the
// mirror, and if main moved, deploy it. This is what makes "merge on GitHub"
// mean "goku deploys".
func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	if s.WebhookSecret == "" {
		httpError(w, http.StatusNotFound, "webhooks not configured")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		httpError(w, http.StatusBadRequest, "unreadable body")
		return
	}
	sig := strings.TrimPrefix(r.Header.Get("X-Hub-Signature-256"), "sha256=")
	mac := hmac.New(sha256.New, []byte(s.WebhookSecret))
	mac.Write(body)
	if expected, err := hex.DecodeString(sig); err != nil || !hmac.Equal(expected, mac.Sum(nil)) {
		httpError(w, http.StatusUnauthorized, "bad signature")
		return
	}
	if r.Header.Get("X-GitHub-Event") != "push" {
		respond(w, map[string]any{"ignored": r.Header.Get("X-GitHub-Event")}, nil)
		return
	}

	var event struct {
		Ref        string `json:"ref"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &event); err != nil {
		httpError(w, http.StatusBadRequest, "unparseable payload")
		return
	}

	projects, err := s.Store.ProjectsByUpstream(r.Context(), event.Repository.FullName)
	if err != nil || len(projects) == 0 {
		respond(w, map[string]any{"matched": 0}, nil)
		return
	}
	deployed := 0
	for i := range projects {
		p := &projects[i]
		repo := s.RepoPath(p.OrgID, p.Name)
		if err := gitrepo.FetchUpstream(repo, s.upstreamFetchURL(r.Context(), p.OrgID, p.Upstream)); err != nil {
			log.Printf("webhook sync %s: %v", p.Name, err)
			continue
		}
		if event.Ref == "refs/heads/main" {
			if _, err := s.startDeploy(context.Background(), p.OrgID, p, "main", "system:github"); err != nil {
				log.Printf("webhook deploy %s: %v", p.Name, err)
			} else {
				deployed++
			}
		}
	}
	respond(w, map[string]any{"matched": len(projects), "deployed": deployed}, nil)
}

// ensureGitHubWebhook creates the push webhook on a linked repo (idempotent
// on GitHub's side: duplicate config URLs are rejected, which we ignore).
func (s *Server) ensureGitHubWebhook(ctx context.Context, ghToken, upstream string) {
	if s.WebhookSecret == "" || ghToken == "" {
		return
	}
	payload, _ := json.Marshal(map[string]any{
		"config": map[string]string{
			"url":          s.BaseURL + "/hooks/github",
			"content_type": "json",
			"secret":       s.WebhookSecret,
		},
		"events": []string{"push"},
		"active": true,
	})
	req, err := http.NewRequestWithContext(ctx, "POST",
		fmt.Sprintf("https://api.github.com/repos/%s/hooks", upstream), bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+ghToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("webhook create %s: %v", upstream, err)
		return
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 && res.StatusCode != 422 { // 422 = already exists
		log.Printf("webhook create %s: %s", upstream, res.Status)
	}
}
