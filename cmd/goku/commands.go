package main

import (
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type projectResp struct {
	Project struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Region string `json:"region"`
		Status string `json:"status"`
	} `json:"project"`
	GitRemote string `json:"git_remote"`
}

func cmdNew(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goku new <name>")
	}
	name := args[0]

	var created projectResp
	if err := apiCall("POST", "/v1/projects", map[string]string{"name": name}, &created); err != nil {
		return err
	}
	fmt.Printf("created project %s\n", created.Project.Name)

	if _, err := runGit("clone", "--quiet", authedRemote(name), name); err != nil {
		return err
	}
	if err := os.Chdir(name); err != nil {
		return err
	}

	writeIfMissing(manifestFile, scaffoldManifest())
	writeIfMissing(".gitignore", ".goku/\n")
	// Committed so every clone of this workspace gives Claude the goku tools.
	writeIfMissing(".mcp.json", "{\n  \"mcpServers\": {\n    \"goku\": { \"command\": \"goku\", \"args\": [\"mcp\"] }\n  }\n}\n")
	writeIfMissing("README.md", "# "+name+"\n\nA goku project. `goku dev` starts local cognates; push a conventional branch (feature/…, bugfix/…) to propose changes.\n")

	for _, gitArgs := range [][]string{
		{"add", "-A"},
		{"commit", "--quiet", "-m", "scaffold project"},
		{"push", "--quiet", "origin", "main"},
	} {
		if _, err := runGit(gitArgs...); err != nil {
			return err
		}
	}

	fmt.Printf("workspace ready: ./%s\n", name)
	fmt.Printf("  ui:     %s/projects/%s\n", gokuURL(), name)
	fmt.Printf("  next:   cd %s && goku add database main && goku dev\n", name)
	return nil
}

func cmdImport(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("usage: goku import <github.com/owner/repo> [name]")
	}
	body := map[string]string{"url": args[0]}
	if len(args) > 1 {
		body["name"] = args[1]
	}
	var resp struct {
		Project struct {
			Name string `json:"name"`
		} `json:"project"`
		Imported string `json:"imported"`
		Adopted  bool   `json:"adopted"`
	}
	if err := apiCall("POST", "/v1/projects/import", body, &resp); err != nil {
		return err
	}
	fmt.Printf("imported %s → project %s (full history preserved)\n", resp.Imported, resp.Project.Name)
	fmt.Printf("  %s/projects/%s\n", gokuURL(), resp.Project.Name)
	if !resp.Adopted {
		fmt.Println("no goku.yaml yet — adopt via a changeset when ready (goku clone, add goku.yaml, goku push)")
	}
	fmt.Printf("next: goku clone %s\n", resp.Project.Name)
	return nil
}

type deployment struct {
	ID     string `json:"id"`
	Branch string `json:"branch"`
	SHA    string `json:"sha"`
	Status string `json:"status"`
	URL    string `json:"url"`
	Log    string `json:"log"`
}

// cmdDeploy kicks a container deployment and follows its log to completion.
func cmdDeploy(args []string) error {
	project, err := projectName()
	if err != nil {
		return fmt.Errorf("run inside a goku workspace: %w", err)
	}
	branch := "main"
	if len(args) > 0 {
		branch = args[0]
	}
	var d deployment
	if err := apiCall("POST", "/v1/projects/"+project+"/deploy", map[string]string{"branch": branch}, &d); err != nil {
		return err
	}
	fmt.Printf("deploying %s @ %s (%s)\n", branch, d.SHA[:8], project)

	seen := 0
	for {
		time.Sleep(2 * time.Second)
		var list struct {
			Deployments []deployment `json:"deployments"`
		}
		if err := apiCall("GET", "/v1/projects/"+project+"/deployments", nil, &list); err != nil {
			return err
		}
		var cur *deployment
		for i := range list.Deployments {
			if list.Deployments[i].ID == d.ID {
				cur = &list.Deployments[i]
			}
		}
		if cur == nil {
			continue
		}
		lines := strings.Split(strings.TrimRight(cur.Log, "\n"), "\n")
		for ; seen < len(lines); seen++ {
			if lines[seen] != "" {
				fmt.Println("  " + lines[seen])
			}
		}
		switch cur.Status {
		case "healthy":
			fmt.Printf("deployed: %s\n", cur.URL)
			return nil
		case "failed":
			return fmt.Errorf("deployment failed")
		}
	}
}

func cmdSync(args []string) error {
	project := ""
	if len(args) > 0 {
		project = args[0]
	} else {
		var err error
		if project, err = projectName(); err != nil {
			return err
		}
	}
	var resp struct {
		Synced string `json:"synced"`
	}
	if err := apiCall("POST", "/v1/projects/"+project+"/sync", map[string]string{}, &resp); err != nil {
		return err
	}
	fmt.Printf("synced from github.com/%s\n", resp.Synced)
	return nil
}

func cmdClone(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: goku clone <name>")
	}
	name := args[0]
	if err := apiCall("GET", "/v1/projects/"+name, nil, &struct{}{}); err != nil {
		return err
	}
	if _, err := runGit("clone", "--quiet", authedRemote(name), name); err != nil {
		return err
	}
	fmt.Printf("cloned into ./%s\n", name)
	return nil
}

func cmdAdd(args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: goku add <database|storage> <name>")
	}
	kind, name := args[0], args[1]
	m, err := loadManifest()
	if err != nil {
		return err
	}
	if err := m.addResource(kind, name); err != nil {
		return err
	}
	if err := m.save(); err != nil {
		return err
	}
	fmt.Printf("added %s %q to %s\n", kind, name, manifestFile)
	fmt.Println("starting local cognate…")
	return devUp()
}

func cmdDev() error { return devUp() }

func cmdEnv() error {
	lines, err := loadEnvFile()
	if err != nil {
		return err
	}
	for _, l := range lines {
		fmt.Println("export " + l)
	}
	return nil
}

func cmdRun(args []string) error {
	if len(args) > 0 && args[0] == "--" {
		args = args[1:]
	}
	if len(args) == 0 {
		return fmt.Errorf("usage: goku run -- <command> [args…]")
	}
	lines, err := loadEnvFile()
	if err != nil {
		return err
	}
	bin, err := exec.LookPath(args[0])
	if err != nil {
		return err
	}
	return syscall.Exec(bin, args, append(os.Environ(), lines...))
}

// cmdPush pushes the current branch for review. Branch names follow
// conventionalbranch.org by default (feature/…, bugfix/…, hotfix/…, chore/…).
func cmdPush(args []string) error {
	project, err := projectName()
	if err != nil {
		return err
	}
	branch, err := runGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch == "main" {
		return fmt.Errorf("main is protected — create a branch first: git checkout -b feature/my-change")
	}
	if _, err := runGit("push", "--quiet", "-u", "origin", branch); err != nil {
		return err
	}
	fmt.Printf("pushed %s\n", branch)
	fmt.Printf("  review: %s/projects/%s?branch=%s\n", gokuURL(), project, url.QueryEscape(branch))
	return nil
}

func cmdStatus() error {
	project, err := projectName()
	if err != nil {
		return err
	}
	var p struct {
		Name   string `json:"name"`
		Region string `json:"region"`
		Status string `json:"status"`
	}
	if err := apiCall("GET", "/v1/projects/"+project, nil, &p); err != nil {
		return err
	}
	var list struct {
		Branches []struct {
			Name    string `json:"name"`
			SHA     string `json:"sha"`
			Subject string `json:"subject"`
			Merged  bool   `json:"merged"`
		} `json:"branches"`
	}
	if err := apiCall("GET", "/v1/projects/"+project+"/branches", nil, &list); err != nil {
		return err
	}

	fmt.Printf("%s — %s (%s)\n", p.Name, p.Status, p.Region)
	for _, b := range list.Branches {
		state := "open"
		switch {
		case b.Name == "main":
			state = "default"
		case b.Merged:
			state = "merged"
		}
		fmt.Printf("  %-8s %-32s %s  %s\n", state, truncate(b.Name, 32), b.SHA[:8], truncate(b.Subject, 40))
	}
	return nil
}

func writeIfMissing(path, content string) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		os.WriteFile(path, []byte(content), 0o644)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
