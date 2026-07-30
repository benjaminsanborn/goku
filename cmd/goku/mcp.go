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
		Description: "List the user's goku projects with status and changeset counts.",
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
			"next":      "Use start_change before editing; main is protected and only moves via merged changesets.",
		}, nil
	})

	type startIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Slug    string `json:"slug" jsonschema:"short kebab-case name for this change, e.g. fix-formatting"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "start_change",
		Description: "Begin a change in a goku project (e.g. the user says: in my goku project X, change the formatting). Finds or clones the local workspace, updates main, and creates a work branch. Returns the workspace directory — edit files there with your normal file tools, then call propose_change to submit for review.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in startIn) (*mcp.CallToolResult, map[string]any, error) {
		ws, err := workspaceFor(in.Project)
		if err != nil {
			return nil, nil, err
		}
		branch := "claude/" + slugify(in.Slug)
		if branch == "claude/" {
			return nil, nil, fmt.Errorf("slug is required")
		}
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
		Project     string `json:"project" jsonschema:"goku project name"`
		Title       string `json:"title" jsonschema:"short human-readable title for the change"`
		Description string `json:"description,omitempty" jsonschema:"what this change does and why"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "propose_change",
		Description: "Submit the current change in a goku project for human review: commits everything in the workspace, pushes the work branch, and opens a changeset in the project changelog. Nothing deploys until a human merges it.",
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
		out, err := selfIn(ws, "push", "-t", in.Title, "-d", in.Description)
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
		Description: "Add an infrastructure resource (database or storage) to a goku project: declares it in the goku.yaml manifest and starts its local docker cognate with the environment contract (DATABASE_URL etc.) injected for local development. The manifest change rides in the next changeset.",
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
		Description: "Show a goku project's status: its resources, changesets (open and merged), and review URLs. Use to check whether a proposed change has been merged.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, map[string]any, error) {
		var project, changesets map[string]any
		if err := apiCall("GET", "/v1/projects/"+in.Project, nil, &project); err != nil {
			return nil, nil, err
		}
		if err := apiCall("GET", "/v1/projects/"+in.Project+"/changesets", nil, &changesets); err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"project": project, "changesets": changesets["changesets"], "ui": gokuURL() + "/projects/" + in.Project}, nil
	})

	type mergeIn struct {
		Project string `json:"project" jsonschema:"goku project name"`
		Number  int    `json:"number" jsonschema:"changeset number to merge"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "merge_change",
		Description: "Merge an open changeset (fast-forwards the project's main). This is the human approval action — only call it when the user has explicitly asked you to merge.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mergeIn) (*mcp.CallToolResult, map[string]any, error) {
		var list struct {
			Changesets []struct {
				ID     string `json:"id"`
				Number int    `json:"number"`
			} `json:"changesets"`
		}
		if err := apiCall("GET", "/v1/projects/"+in.Project+"/changesets", nil, &list); err != nil {
			return nil, nil, err
		}
		for _, cs := range list.Changesets {
			if cs.Number == in.Number {
				var merged map[string]any
				if err := apiCall("POST", "/v1/changesets/"+cs.ID+"/merge", nil, &merged); err != nil {
					return nil, nil, err
				}
				return nil, merged, nil
			}
		}
		return nil, nil, fmt.Errorf("changeset #%d not found in project %q", in.Number, in.Project)
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
