package main

import (
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
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
		return fmt.Errorf("usage: platform new <name>")
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
	writeIfMissing(".gitignore", ".platform/\n")
	writeIfMissing("README.md", "# "+name+"\n\nA platform project. `platform dev` starts local cognates; push a branch and open a changeset to propose changes.\n")

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
	fmt.Printf("  ui:     %s/projects/%s\n", platformURL(), name)
	fmt.Printf("  next:   cd %s && platform add database main && platform dev\n", name)
	return nil
}

func cmdClone(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: platform clone <name>")
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
		return fmt.Errorf("usage: platform add <database|storage> <name>")
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
		return fmt.Errorf("usage: platform run -- <command> [args…]")
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

func cmdPush(args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	title := fs.String("t", "", "changeset title (default: last commit subject)")
	desc := fs.String("d", "", "changeset description")
	if err := fs.Parse(args); err != nil {
		return err
	}

	project, err := projectName()
	if err != nil {
		return err
	}
	branch, err := runGit("rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch == "main" {
		return fmt.Errorf("main is protected — create a branch first: git checkout -b claude/my-change")
	}
	if _, err := runGit("push", "--quiet", "-u", "origin", branch); err != nil {
		return err
	}
	if *title == "" {
		*title, _ = runGit("log", "-1", "--format=%s")
	}

	var cs struct {
		ID     string `json:"id"`
		Number int    `json:"number"`
	}
	err = apiCall("POST", "/v1/projects/"+project+"/changesets",
		map[string]string{"title": *title, "description": *desc, "branch": branch}, &cs)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			fmt.Printf("pushed %s — its open changeset was updated\n", branch)
			return nil
		}
		return err
	}
	fmt.Printf("changeset #%d opened: %s\n", cs.Number, *title)
	fmt.Printf("  review: %s/projects/%s/changesets/%s\n", platformURL(), project, cs.ID)
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
		Changesets []struct {
			Number   int    `json:"number"`
			Title    string `json:"title"`
			Status   string `json:"status"`
			Branch   string `json:"branch"`
			OpenedBy string `json:"opened_by"`
		} `json:"changesets"`
	}
	if err := apiCall("GET", "/v1/projects/"+project+"/changesets", nil, &list); err != nil {
		return err
	}

	fmt.Printf("%s — %s (%s)\n", p.Name, p.Status, p.Region)
	if len(list.Changesets) == 0 {
		fmt.Println("no changesets yet")
		return nil
	}
	for _, cs := range list.Changesets {
		fmt.Printf("  #%-3d %-8s %-40s %s (%s)\n", cs.Number, cs.Status, truncate(cs.Title, 40), cs.Branch, cs.OpenedBy)
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
