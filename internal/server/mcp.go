package server

import (
	"context"
	"fmt"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/benjaminsanborn/goku/internal/store"
)

// The MCP actor for this dev slice; real deployments resolve token → agent identity.
const agentActor = "agent:claude"

// mcpHandler builds an MCP server per session, bound to the organization the
// session's token resolved to (requireToken put it in the request context).
func (s *Server) mcpHandler() http.Handler {
	// Caddy proxies to loopback with the public Host header; the SDK's DNS-
	// rebinding protection would 403 that. Safe to disable: /mcp requires a
	// bearer token, which a rebinding attacker cannot attach.
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		org := orgFrom(r.Context())
		if org == "" {
			return nil // 400; requireToken should have rejected already
		}
		return s.buildMCPServer(org)
	}, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
}

func (s *Server) buildMCPServer(org string) *mcp.Server {
	srv := mcp.NewServer(&mcp.Implementation{Name: "goku", Title: "Goku", Version: "0.1.0"}, nil)

	type projectIn struct {
		Project string `json:"project" jsonschema:"project name or id"`
	}

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_projects",
		Description: "List all projects in your organization with status and changeset counts.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ any) (*mcp.CallToolResult, map[string]any, error) {
		projects, err := s.Store.ListProjects(ctx, org)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"projects": projects}, nil
	})

	type createProjectIn struct {
		Name string `json:"name" jsonschema:"project name (lowercase letters, digits, dashes)"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "create_project",
		Description: "Create a new project: an isolated deployment target with its own git repository, which will hold curated AWS resources (API, database, load balancer, storage, web). Returns the project and its git remote URL — clone it to start a local workspace (username: claude, password: your goku token). Prefer the 'goku new' CLI when working locally: it creates, clones, and scaffolds in one step.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in createProjectIn) (*mcp.CallToolResult, map[string]any, error) {
		p, err := s.createProject(ctx, org, in.Name, agentActor)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"project": p, "git_remote": s.gitRemoteURL(p.Name)}, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_project",
		Description: "Get a project's detail: status, region, git remote URL, and recent changesets.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, map[string]any, error) {
		p, err := s.Store.GetProject(ctx, org, in.Project)
		if err != nil {
			return nil, nil, err
		}
		changesets, err := s.Store.ListChangesets(ctx, org, p.ID)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"project": p, "git_remote": s.gitRemoteURL(p.Name), "changesets": changesets}, nil
	})

	type fileIn struct {
		Path    string `json:"path" jsonschema:"repo-relative file path, e.g. main.go or goku.yaml"`
		Content string `json:"content" jsonschema:"full file content"`
	}
	type openChangesetIn struct {
		Project     string   `json:"project" jsonschema:"project name or id"`
		Title       string   `json:"title" jsonschema:"short human-readable title for the change"`
		Description string   `json:"description,omitempty" jsonschema:"what this change does and why"`
		Branch      string   `json:"branch,omitempty" jsonschema:"a branch you already pushed to the project's git remote; omit when providing files"`
		Files       []fileIn `json:"files,omitempty" jsonschema:"only when you have no local workspace: files (path + full content) the platform will commit onto a new branch for you"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "open_changeset",
		Description: "Propose a change to a project for human review in the changelog. Preferred flow: work in a local clone, push a branch to the goku remote, then open a changeset referencing that branch. Fallback (no local workspace): provide files and the platform commits them onto a new branch. Nothing deploys until a changeset is merged.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in openChangesetIn) (*mcp.CallToolResult, *store.Changeset, error) {
		files := make([]store.File, len(in.Files))
		for i, f := range in.Files {
			files[i] = store.File{Path: f.Path, Content: f.Content}
		}
		cs, err := s.openChangeset(ctx, org, in.Project, in.Title, in.Description, in.Branch, agentActor, files)
		if err != nil {
			return nil, nil, err
		}
		return nil, cs, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_changesets",
		Description: "List the changesets (proposed changes) for a project, newest first.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in projectIn) (*mcp.CallToolResult, map[string]any, error) {
		changesets, err := s.Store.ListChangesets(ctx, org, in.Project)
		if err != nil {
			return nil, nil, err
		}
		return nil, map[string]any{"changesets": changesets}, nil
	})

	type mergeIn struct {
		Project string `json:"project" jsonschema:"project name or id"`
		Number  int    `json:"number" jsonschema:"changeset number to merge"`
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "merge_changeset",
		Description: "Merge an open changeset: fast-forwards main to the changeset branch. This is the approval action — normally a human clicks Merge in the UI; only call this when the human has explicitly asked you to merge.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in mergeIn) (*mcp.CallToolResult, *store.Changeset, error) {
		changesets, err := s.Store.ListChangesets(ctx, org, in.Project)
		if err != nil {
			return nil, nil, err
		}
		for _, cs := range changesets {
			if cs.Number == in.Number {
				merged, err := s.mergeChangeset(ctx, org, cs.ID, agentActor)
				if err != nil {
					return nil, nil, err
				}
				return nil, merged, nil
			}
		}
		return nil, nil, fmt.Errorf("changeset #%d not found in project %q", in.Number, in.Project)
	})

	return srv
}
