package cloud

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
)

// EgressIP is the control plane's public address as the internet sees it —
// the only source that should reach an unmeshed instance. It is discovered
// per launch rather than configured, because the control plane's address can
// change (a restart on a new host, a NAT gateway swap).
func EgressIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", "https://checkip.amazonaws.com", nil)
	if err != nil {
		return "", err
	}
	raw, err := doRaw(req)
	if err != nil {
		return "", fmt.Errorf("egress ip lookup: %w", err)
	}
	ip := strings.TrimSpace(string(raw))
	if net.ParseIP(ip) == nil {
		return "", fmt.Errorf("egress ip lookup returned %q, which is not an address", firstLine(ip))
	}
	return ip, nil
}
