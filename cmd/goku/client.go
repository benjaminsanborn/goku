package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"strings"
)

// Config precedence: env var, then ~/.config/goku/config (KEY=VALUE
// lines), then dev defaults.
func gokuURL() string {
	return strings.TrimSuffix(configValue("GOKU_URL", "https://goku.host"), "/")
}

func gokuToken() string {
	return configValue("GOKU_TOKEN", "dev-token")
}

func configValue(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err == nil {
		if b, err := os.ReadFile(home + "/.config/goku/config"); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok && k == key {
					return v
				}
			}
		}
	}
	return fallback
}

// apiCall hits the configured control plane with the configured token.
func apiCall(method, p string, body any, out any) error {
	return apiCallAt(gokuURL(), gokuToken(), method, p, body, out)
}

func apiCallAt(base, token, method, p string, body any, out any) error {
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			return err
		}
	}
	req, err := http.NewRequest(method, strings.TrimSuffix(base, "/")+p, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s unreachable — is gokud running? (%w)", base, err)
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		var e struct {
			Error string `json:"error"`
		}
		json.NewDecoder(res.Body).Decode(&e)
		if e.Error == "" {
			e.Error = res.Status
		}
		return fmt.Errorf("%s", e.Error)
	}
	if out != nil {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

// apiStream GETs a text/stream endpoint and copies it to out until EOF or
// interrupt (used for live log tails).
func apiStream(p string, out io.Writer) error {
	req, err := http.NewRequest("GET", gokuURL()+p, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+gokuToken())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return fmt.Errorf("%s", strings.TrimSpace(string(body)))
	}
	_, err = io.Copy(out, res.Body)
	return err
}

// authedRemote embeds credentials so git push works without a credential helper.
func authedRemote(project string) string {
	u, _ := url.Parse(gokuURL())
	u.User = url.UserPassword("claude", gokuToken())
	u.Path = "/git/" + project + ".git"
	return u.String()
}

// projectName derives the project from the workspace's origin remote.
func projectName() (string, error) {
	out, err := runGit("remote", "get-url", "origin")
	if err != nil {
		return "", fmt.Errorf("not inside a goku workspace (no origin remote)")
	}
	base := path.Base(strings.TrimSpace(out))
	name := strings.TrimSuffix(base, ".git")
	if name == "" {
		return "", fmt.Errorf("could not derive project name from remote %q", out)
	}
	return name, nil
}

func runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}
