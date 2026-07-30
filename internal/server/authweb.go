package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"

	"golang.org/x/oauth2"
	oagithub "golang.org/x/oauth2/github"
	oagoogle "golang.org/x/oauth2/google"
)

const sessionCookie = "goku_session"

// OAuthConfig carries provider credentials from the environment; a provider
// with no client ID is simply not offered.
type OAuthConfig struct {
	GitHubClientID     string
	GitHubClientSecret string
	GoogleClientID     string
	GoogleClientSecret string
}

func (s *Server) githubOAuth() *oauth2.Config {
	if s.OAuth.GitHubClientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     s.OAuth.GitHubClientID,
		ClientSecret: s.OAuth.GitHubClientSecret,
		Endpoint:     oagithub.Endpoint,
		RedirectURL:  s.BaseURL + "/auth/github/callback",
		// repo scope powers private-repo import; see docs/design 06 for the
		// planned narrower GitHub App integration.
		Scopes: []string{"read:user", "user:email", "repo"},
	}
}

func (s *Server) googleOAuth() *oauth2.Config {
	if s.OAuth.GoogleClientID == "" {
		return nil
	}
	return &oauth2.Config{
		ClientID:     s.OAuth.GoogleClientID,
		ClientSecret: s.OAuth.GoogleClientSecret,
		Endpoint:     oagoogle.Endpoint,
		RedirectURL:  s.BaseURL + "/auth/google/callback",
		Scopes:       []string{"openid", "email", "profile"},
	}
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	respond(w, map[string]bool{
		"github": s.OAuth.GitHubClientID != "",
		"google": s.OAuth.GoogleClientID != "",
	}, nil)
}

func (s *Server) handleAuthStart(conf func() *oauth2.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := conf()
		if c == nil {
			httpError(w, http.StatusNotFound, "provider not configured")
			return
		}
		raw := make([]byte, 16)
		rand.Read(raw)
		state := hex.EncodeToString(raw)
		http.SetCookie(w, &http.Cookie{
			Name: "goku_oauth_state", Value: state, Path: "/auth",
			MaxAge: 600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, c.AuthCodeURL(state), http.StatusFound)
	}
}

func (s *Server) handleAuthCallback(provider string, conf func() *oauth2.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c := conf()
		if c == nil {
			httpError(w, http.StatusNotFound, "provider not configured")
			return
		}
		stateCookie, err := r.Cookie("goku_oauth_state")
		if err != nil || r.URL.Query().Get("state") != stateCookie.Value {
			httpError(w, http.StatusBadRequest, "state mismatch — restart login")
			return
		}
		tok, err := c.Exchange(r.Context(), r.URL.Query().Get("code"))
		if err != nil {
			httpError(w, http.StatusBadGateway, "token exchange failed: "+err.Error())
			return
		}

		var id, email, name, avatar, ghToken string
		switch provider {
		case "github":
			id, email, name, avatar, err = githubProfile(r.Context(), c, tok)
			ghToken = tok.AccessToken
		case "google":
			id, email, name, avatar, err = googleProfile(r.Context(), c, tok)
		}
		if err != nil {
			httpError(w, http.StatusBadGateway, "profile fetch failed: "+err.Error())
			return
		}

		user, err := s.Store.UpsertUser(r.Context(), provider, id, email, name, avatar, ghToken)
		if err != nil {
			respond(w, nil, err)
			return
		}
		session, err := s.Store.CreateSession(r.Context(), user.ID)
		if err != nil {
			respond(w, nil, err)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: sessionCookie, Value: session, Path: "/",
			MaxAge: 30 * 24 * 3600, HttpOnly: true, Secure: true, SameSite: http.SameSiteLaxMode,
		})
		http.Redirect(w, r, "/", http.StatusFound)
	}
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		s.Store.DeleteSession(r.Context(), c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1})
	respond(w, map[string]bool{"ok": true}, nil)
}

// handleJoinOrg lets a logged-in user redeem an org token (issued by the
// operator) to become a member; afterwards their own identity is the actor.
func (s *Server) handleJoinOrg(w http.ResponseWriter, r *http.Request) {
	user := userFrom(r.Context())
	if user == nil {
		httpError(w, http.StatusUnauthorized, "sign in first")
		return
	}
	var in struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	org, ok := s.resolveOrg(r.Context(), in.Token)
	if !ok {
		httpError(w, http.StatusUnprocessableEntity, "invalid organization token")
		return
	}
	if err := s.Store.JoinOrg(r.Context(), user.ID, org, user.Email); err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"joined": true}, nil)
}

func githubProfile(ctx context.Context, c *oauth2.Config, tok *oauth2.Token) (id, email, name, avatar string, err error) {
	client := c.Client(ctx, tok)
	var u struct {
		ID        int64  `json:"id"`
		Login     string `json:"login"`
		Name      string `json:"name"`
		Email     string `json:"email"`
		AvatarURL string `json:"avatar_url"`
	}
	if err = getJSON(client, "https://api.github.com/user", &u); err != nil {
		return
	}
	email = u.Email
	if email == "" {
		var emails []struct {
			Email   string `json:"email"`
			Primary bool   `json:"primary"`
		}
		if err = getJSON(client, "https://api.github.com/user/emails", &emails); err != nil {
			return
		}
		for _, e := range emails {
			if e.Primary {
				email = e.Email
			}
		}
	}
	if u.Name == "" {
		u.Name = u.Login
	}
	return fmt.Sprint(u.ID), email, u.Name, u.AvatarURL, nil
}

func googleProfile(ctx context.Context, c *oauth2.Config, tok *oauth2.Token) (id, email, name, avatar string, err error) {
	client := c.Client(ctx, tok)
	var u struct {
		Sub     string `json:"sub"`
		Email   string `json:"email"`
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err = getJSON(client, "https://openidconnect.googleapis.com/v1/userinfo", &u); err != nil {
		return
	}
	return u.Sub, u.Email, u.Name, u.Picture, nil
}

func getJSON(client *http.Client, url string, out any) error {
	res, err := client.Get(url)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		return fmt.Errorf("%s: %s", url, res.Status)
	}
	return json.NewDecoder(res.Body).Decode(out)
}
