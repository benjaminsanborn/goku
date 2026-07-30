package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type meResp struct {
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
}

// cmdLogin points this machine at the control plane using an organization
// token (issued by the operator). The --url flag exists for development but
// is undocumented: the hosted control plane is the default.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	url := fs.String("url", gokuURL(), "control plane URL (development only)")
	tokenFlag := fs.String("token", "", "org token (prompted if omitted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := *tokenFlag
	if token == "" {
		fmt.Fprintf(os.Stderr, "token: ")
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		token = strings.TrimSpace(line)
	}
	var me meResp
	if err := apiCallAt(*url, token, "GET", "/v1/me", nil, &me); err != nil {
		return fmt.Errorf("token rejected: %w", err)
	}
	if err := writeConfig(*url, token); err != nil {
		return err
	}
	fmt.Printf("logged in as organization %q\n", me.Organization.Name)
	registerClaude()
	return nil
}

// registerClaude wires goku into Claude Code (user scope) as a stdio MCP
// server. The token stays in goku's config — Claude only learns the command.
func registerClaude() {
	if _, err := exec.LookPath("claude"); err != nil {
		fmt.Println("Claude Code not found — to connect it later, run: claude mcp add -s user goku -- goku mcp")
		return
	}
	exec.Command("claude", "mcp", "remove", "-s", "user", "goku").Run()
	if out, err := exec.Command("claude", "mcp", "add", "-s", "user", "goku", "--", "goku", "mcp").CombinedOutput(); err != nil {
		fmt.Printf("could not register with Claude Code (%s) — run manually: claude mcp add -s user goku -- goku mcp\n", strings.TrimSpace(string(out)))
		return
	}
	fmt.Println("Claude Code connected: your Claude now has the goku tools in every session.")
}

func cmdWhoami() error {
	var me meResp
	if err := apiCallAt(gokuURL(), gokuToken(), "GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("organization %q (%s)\n", me.Organization.Name, me.Organization.ID)
	return nil
}

func writeConfig(url, token string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dir := filepath.Join(home, ".config", "goku")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	content := fmt.Sprintf("GOKU_URL=%s\nGOKU_TOKEN=%s\n", url, token)
	return os.WriteFile(filepath.Join(dir, "config"), []byte(content), 0o600)
}
