package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// cmdMCP serves MCP over stdio for Claude. It runs on the user's machine with
// their goku config, so tools can do local workspace work (clone, branch,
// commit) as well as talk to the control plane — no token ever reaches
// Claude's own configuration.
func cmdMCP() error {
	srv := mcp.NewServer(&mcp.Implementation{Name: "goku", Title: "Goku", Version: version}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_projects",
		Description: "List the user's goku projects with status.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, map[string]any, error) {
		var out map[string]any
		if err := apiCall("GET", "/v1/projects", nil, &out); err != nil {
			return nil, nil, err
		}
		return nil, out, nil
	})

	type setupIn struct {
		Name string `json:"name" jsonschema:"project name (lowercase letters, digits, dashes)"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "setup_project",
		Description: "Set up a new goku project: creates it on the control plane, clones a local workspace into ./<name>, scaffolds the goku.yaml manifest, and pushes the initial commit. Use when the user asks to create, start, or set up a goku project. Afterwards, use start_change to begin working in it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in setupIn) (*mcp.CallToolResult, map[string]any, error) {
		cwd, err := os.Getwd()
		if err != nil {
			return nil, nil, err
		}
		out, err := selfIn(cwd, "new", in.Name)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{
			"workspace": filepath.Join(cwd, in.Name),
			"output":    out,
			"next":      "Use start_change before editing; main is protected and only moves when a branch is merged.",
		}, nil
	})

	type importIn struct {
		URL  string `json:"url" jsonschema:"GitHub repository, e.g. github.com/owner/repo or owner/repo"`
		Name string `json:"name,omitempty" jsonschema:"goku project name; defaults to the repo name"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "import_project",
		Description: "Import an existing GitHub repository as a goku project: full git history, branches, and tags are preserved and main becomes the protected default branch. No changes are made to the code. Use when the user asks to bring an existing repo into goku. If the result has adopted=false there is no goku.yaml yet — when the user wants to adopt goku, use start_change to add goku.yaml (+ .mcp.json, Dockerfile if missing) through a normal changeset.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in importIn) (*mcp.CallToolResult, map[string]any, error) {
		body := map[string]string{"url": in.URL}
		if in.Name != "" {
			body["name"] = in.Name
		}
		var out map[string]any
		if err := apiCall("POST", "/v1/projects/import", body, &out); err != nil {
			return nil, nil, err
		}
		out["next"] = "If adopted=false, use start_change to add goku.yaml and a Dockerfile when the user wants to adopt goku."
		return nil, out, nil
	})

	type startIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Kind    string `json:"kind,omitempty" jsonschema:"conventional branch type: feature (default), bugfix, hotfix, chore, or release"`
		Slug    string `json:"slug" jsonschema:"short kebab-case name for this change, e.g. fix-formatting"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "start_change",
		Description: "Begin a change in a goku project (e.g. the user says: in my goku project X, change the formatting). Finds or clones the local workspace, updates main, and creates a conventional branch (conventionalbranch.org): feature/<slug> by default, or bugfix/hotfix/chore/release via kind. Returns the workspace directory — edit files there with your normal file tools, then call propose_change to push for review.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in startIn) (*mcp.CallToolResult, map[string]any, error) {
		ws, err := workspaceFor(in.Project)
		if err != nil {
			return nil, nil, err
		}
		kind := in.Kind
		switch kind {
		case "":
			kind = "feature"
		case "feature", "bugfix", "hotfix", "chore", "release":
		default:
			return nil, nil, fmt.Errorf("kind must be feature, bugfix, hotfix, chore, or release")
		}
		slug := slugify(in.Slug)
		if slug == "" {
			return nil, nil, fmt.Errorf("slug is required")
		}
		branch := kind + "/" + slug
		if _, err := gitIn(ws, "checkout", "main"); err != nil {
			return nil, nil, err
		}
		if _, err := gitIn(ws, "pull", "--ff-only", "--quiet", "origin", "main"); err != nil {
			return nil, nil, err
		}
		if _, err := gitIn(ws, "checkout", "-b", branch); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{
			"workspace": ws,
			"branch":    branch,
			"next":      "Edit files in the workspace, then call propose_change. Use add_resource if the change needs a database or storage.",
		}, nil
	})

	type proposeIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Title   string `json:"title" jsonschema:"commit message subject for uncommitted work (conventional commit style encouraged)"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "propose_change",
		Description: "Submit the current change in a goku project for human review: commits everything in the workspace and pushes the branch. The branch appears in the project UI with its diff against main — like a GitHub branch awaiting merge. Nothing deploys until a human merges it.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in proposeIn) (*mcp.CallToolResult, map[string]any, error) {
		ws, err := workspaceFor(in.Project)
		if err != nil {
			return nil, nil, err
		}
		branch, err := gitIn(ws, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return nil, nil, err
		}
		if branch == "main" {
			return nil, nil, fmt.Errorf("workspace is on main — call start_change first")
		}
		if _, err := gitIn(ws, "add", "-A"); err != nil {
			return nil, nil, err
		}
		if status, _ := gitIn(ws, "status", "--porcelain"); status != "" {
			if _, err := gitIn(ws, "commit", "-m", in.Title); err != nil {
				return nil, nil, err
			}
		}
		out, err := selfIn(ws, "push")
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"output": out, "branch": branch}, nil
	})

	type resourceIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Type    string `json:"type" jsonschema:"resource type: database (postgres) or storage (S3-compatible)"`
		Name    string `json:"name" jsonschema:"resource name, e.g. main or assets"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_resource",
		Description: "Add an infrastructure resource (database or storage) to a goku project: declares it in the goku.yaml manifest and starts its local docker cognate with the environment contract (DATABASE_URL etc.) injected for local development. The manifest change rides in the branch you push.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in resourceIn) (*mcp.CallToolResult, map[string]any, error) {
		ws, err := workspaceFor(in.Project)
		if err != nil {
			return nil, nil, err
		}
		out, err := selfIn(ws, "add", in.Type, in.Name)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"output": out}, nil
	})

	type projectIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "project_status",
		Description: "Show a goku project's status: its branches (with merged state), architecture manifest, and review URLs. A branch that has disappeared or shows merged=true was accepted into main.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, map[string]any, error) {
		var project, branches, manifest map[string]any
		if err := apiCall("GET", "/v1/projects/"+in.Project, nil, &project); err != nil {
			return nil, nil, err
		}
		if err := apiCall("GET", "/v1/projects/"+in.Project+"/branches", nil, &branches); err != nil {
			return nil, nil, err
		}
		if err := apiCall("GET", "/v1/projects/"+in.Project+"/manifest", nil, &manifest); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"project": project, "branches": branches["branches"], "manifest": manifest, "ui": gokuURL() + "/projects/" + in.Project}, nil
	})

	type mergeIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Branch  string `json:"branch" jsonschema:"branch to merge into main, e.g. feature/add-todos"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "merge_change",
		Description: "Merge a branch into the project's main (fast-forward) and delete the branch. This is the human approval action — only call it when the user has explicitly asked you to merge.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mergeIn) (*mcp.CallToolResult, map[string]any, error) {
		var merged map[string]any
		if err := apiCall("POST", "/v1/projects/"+in.Project+"/merge", map[string]string{"branch": in.Branch}, &merged); err != nil {
			return nil, nil, err
		}
		return nil, merged, nil
	})

	return srv.Run(context.Background(), &mcp.StdioTransport{})
}

// workspaceFor locates the project's local workspace: the current directory,
// a child directory, or a fresh clone into the current directory.
func workspaceFor(project string) (string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	if filepath.Base(cwd) == project {
		if _, err := os.Stat(filepath.Join(cwd, ".git")); err == nil {
			return cwd, nil
		}
	}
	child := filepath.Join(cwd, project)
	if _, err := os.Stat(filepath.Join(child, ".git")); err == nil {
		return child, nil
	}
	if _, err := selfIn(cwd, "clone", project); err != nil {
		return "", fmt.Errorf("no local workspace and clone failed: %w", err)
	}
	return child, nil
}

func gitIn(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// selfIn re-invokes this goku binary as a subprocess in dir, reusing the
// CLI's workspace-relative commands (new, clone, add, push) verbatim.
func selfIn(dir string, args ...string) (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("goku %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func slugify(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
		case b.Len() > 0 && !strings.HasSuffix(b.String(), "-"):
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}
