package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// Azure and DigitalOcean credentials are verified so an operator knows the
// account is wired up correctly, but provisioning for them is not implemented
// yet — providers of these kinds settle at status "pending" and are not
// offered as deploy targets. AWS (ec2.go) is the implemented path.

// VerifyAzure exchanges a service principal for a management-plane token.
func VerifyAzure(ctx context.Context, creds map[string]string) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {creds["client_id"]},
		"client_secret": {creds["client_secret"]},
		"scope":         {"https://management.azure.com/.default"},
	}
	endpoint := "https://login.microsoftonline.com/" + url.PathEscape(creds["tenant_id"]) + "/oauth2/v2.0/token"
	req, err := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := doRaw(req)
	if err != nil {
		var e struct {
			Description string `json:"error_description"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Description != "" {
			return "", fmt.Errorf("azure: %s", firstLine(e.Description))
		}
		return "", fmt.Errorf("azure: %w", err)
	}
	if sub := creds["subscription_id"]; sub != "" {
		return "subscription " + sub, nil
	}
	return "tenant " + creds["tenant_id"], nil
}

// VerifyDigitalOcean reads the account behind the API token.
func VerifyDigitalOcean(ctx context.Context, creds map[string]string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://api.digitalocean.com/v2/account", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+creds["api_token"])

	raw, err := doRaw(req)
	if err != nil {
		return "", fmt.Errorf("digitalocean: %s", firstLine(string(raw)))
	}
	var body struct {
		Account struct {
			Email string `json:"email"`
			UUID  string `json:"uuid"`
		} `json:"account"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", err
	}
	if body.Account.Email != "" {
		return body.Account.Email, nil
	}
	return body.Account.UUID, nil
}

func doRaw(req *http.Request) ([]byte, error) {
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return raw, fmt.Errorf("%s", resp.Status)
	}
	return raw, nil
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}
