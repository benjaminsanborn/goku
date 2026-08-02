package cloud

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Tailscale is the network provider: instances join a tailnet at boot and the
// control plane reaches them over it, so nothing in the fleet needs public
// inbound. Credentials are an OAuth client rather than a long-lived auth key —
// goku mints a short-lived, single-use, pre-authorized key per instance.
type Tailscale struct {
	ClientID     string
	ClientSecret string
	Tailnet      string // "-" means the token's default tailnet
	Tag          string // ACL tag applied to provisioned instances
}

const (
	tailscaleAPI     = "https://api.tailscale.com/api/v2"
	defaultFleetTag  = "tag:goku-fleet"
	authKeyExpirySec = 900
)

// TailscaleFrom builds a client from stored credentials.
func TailscaleFrom(creds map[string]string) Tailscale {
	t := Tailscale{
		ClientID:     creds["client_id"],
		ClientSecret: creds["client_secret"],
		Tailnet:      creds["tailnet"],
		Tag:          creds["tag"],
	}
	if t.Tailnet == "" {
		t.Tailnet = "-"
	}
	if t.Tag == "" {
		t.Tag = defaultFleetTag
	}
	return t
}

// Identity verifies the OAuth client and reports the tailnet it opens.
func (t Tailscale) Identity(ctx context.Context) (string, error) {
	devices, err := t.devices(ctx)
	if err != nil {
		return "", err
	}
	name := t.Tailnet
	if name == "-" {
		name = "default tailnet"
	}
	return fmt.Sprintf("%s (%d devices)", name, len(devices)), nil
}

func (t Tailscale) token(ctx context.Context) (string, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {t.ClientID},
		"client_secret": {t.ClientSecret},
	}
	req, err := http.NewRequestWithContext(ctx, "POST", tailscaleAPI+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	raw, err := doRaw(req)
	if err != nil {
		return "", fmt.Errorf("tailscale oauth: %s", firstLine(string(raw)))
	}
	var out struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", fmt.Errorf("tailscale oauth: no access token returned")
	}
	return out.AccessToken, nil
}

func (t Tailscale) call(ctx context.Context, method, path string, body any, out any) error {
	token, err := t.token(ctx)
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := http.NewRequestWithContext(ctx, method, tailscaleAPI+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	raw, err := doRaw(req)
	if err != nil {
		return fmt.Errorf("tailscale %s: %s", path, firstLine(string(raw)))
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(raw, out)
}

// Device is a tailnet member.
type Device struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Hostname  string   `json:"hostname"`
	Addresses []string `json:"addresses"`
}

// TailnetIP is the device's 100.x address — what the control plane dials.
func (d Device) TailnetIP() string {
	for _, a := range d.Addresses {
		if strings.HasPrefix(a, "100.") {
			return a
		}
	}
	if len(d.Addresses) > 0 {
		return d.Addresses[0]
	}
	return ""
}

func (t Tailscale) devices(ctx context.Context) ([]Device, error) {
	var out struct {
		Devices []Device `json:"devices"`
	}
	if err := t.call(ctx, "GET", "/tailnet/"+url.PathEscape(t.Tailnet)+"/devices", nil, &out); err != nil {
		return nil, err
	}
	return out.Devices, nil
}

// MintAuthKey issues a single-use, pre-authorized, tagged key for one
// instance. Short expiry: it is consumed within a minute of being minted.
func (t Tailscale) MintAuthKey(ctx context.Context) (string, error) {
	body := map[string]any{
		"capabilities": map[string]any{
			"devices": map[string]any{
				"create": map[string]any{
					"reusable":      false,
					"ephemeral":     false,
					"preauthorized": true,
					"tags":          []string{t.Tag},
				},
			},
		},
		"expirySeconds": authKeyExpirySec,
		"description":   "goku fleet provisioning",
	}
	var out struct {
		Key string `json:"key"`
	}
	if err := t.call(ctx, "POST", "/tailnet/"+url.PathEscape(t.Tailnet)+"/keys", body, &out); err != nil {
		return "", err
	}
	if out.Key == "" {
		return "", fmt.Errorf("tailscale: no auth key returned")
	}
	return out.Key, nil
}

// WaitForDevice polls until the instance has joined the tailnet, returning
// the address the control plane should use to reach it.
func (t Tailscale) WaitForDevice(ctx context.Context, hostname string, logf Logf) (string, error) {
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
		devices, err := t.devices(ctx)
		if err != nil {
			logf("tailnet lookup failed, retrying: %v", err)
			continue
		}
		for _, d := range devices {
			if !matchesHostname(d, hostname) {
				continue
			}
			if ip := d.TailnetIP(); ip != "" {
				return ip, nil
			}
		}
	}
	return "", fmt.Errorf("tailscale: %s did not join the tailnet within 4m", hostname)
}

// RemoveDevice deletes the tailnet member for a hostname, so terminated
// instances don't linger in the device list.
func (t Tailscale) RemoveDevice(ctx context.Context, hostname string) error {
	devices, err := t.devices(ctx)
	if err != nil {
		return err
	}
	for _, d := range devices {
		if matchesHostname(d, hostname) {
			return t.call(ctx, "DELETE", "/device/"+url.PathEscape(d.ID), nil, nil)
		}
	}
	return nil
}

// matchesHostname compares against both the reported hostname and the
// MagicDNS name, whose first label is the hostname.
func matchesHostname(d Device, hostname string) bool {
	if strings.EqualFold(d.Hostname, hostname) {
		return true
	}
	first, _, _ := strings.Cut(d.Name, ".")
	return strings.EqualFold(first, hostname)
}
