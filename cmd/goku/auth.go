package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type meResp struct {
	Organization struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"organization"`
}

// cmdSignup creates an organization on the control plane; the returned token
// is saved to ~/.config/goku/config and shown once.
func cmdSignup(args []string) error {
	fs := flag.NewFlagSet("signup", flag.ContinueOnError)
	url := fs.String("url", gokuURL(), "control plane URL")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: goku signup <org-name> [--url https://goku.host]")
	}
	orgName := fs.Arg(0)

	var resp struct {
		Organization struct {
			Name string `json:"name"`
		} `json:"organization"`
		Token string `json:"token"`
	}
	if err := apiCallAt(*url, "", "POST", "/v1/signup", map[string]string{"organization": orgName}, &resp); err != nil {
		return err
	}
	if err := writeConfig(*url, resp.Token); err != nil {
		return err
	}
	fmt.Printf("organization %q created on %s\n", resp.Organization.Name, *url)
	fmt.Printf("\n  token: %s\n\n", resp.Token)
	fmt.Println("saved to ~/.config/goku/config — this token is shown only once; store it somewhere safe.")
	fmt.Println("connect your Claude:")
	fmt.Printf("  claude mcp add --transport http goku %s/mcp --header \"Authorization: Bearer %s\"\n", *url, resp.Token)
	return nil
}

// cmdLogin points this machine at an existing organization using its token.
func cmdLogin(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	url := fs.String("url", gokuURL(), "control plane URL")
	tokenFlag := fs.String("token", "", "org token (prompted if omitted)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token := *tokenFlag
	if token == "" {
		fmt.Fprintf(os.Stderr, "token for %s: ", *url)
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil {
			return err
		}
		token = strings.TrimSpace(line)
	}
	var me meResp
	if err := apiCallAt(*url, token, "GET", "/v1/me", nil, &me); err != nil {
		return fmt.Errorf("token rejected by %s: %w", *url, err)
	}
	if err := writeConfig(*url, token); err != nil {
		return err
	}
	fmt.Printf("logged in to %s as organization %q\n", *url, me.Organization.Name)
	return nil
}

func cmdWhoami() error {
	var me meResp
	if err := apiCallAt(gokuURL(), gokuToken(), "GET", "/v1/me", nil, &me); err != nil {
		return err
	}
	fmt.Printf("%s → organization %q (%s)\n", gokuURL(), me.Organization.Name, me.Organization.ID)
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
