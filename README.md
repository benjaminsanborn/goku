# goku

Compliant CI/CD into AWS, built for AI agents.

goku is a control plane your agents ship through: every project gets an isolated deployment target with curated resources (database, API, load balancer, storage, web), a git repository with a protected `main`, and a changelog of **changesets** — proposed changes that humans review and merge. Your Claude connects over MCP and proposes; you approve in the UI. Locally, declared resources run as **cognates** (postgres, MinIO in docker) with the same env vars they'll have in the cloud.

> AWS provisioning and deployment are design-complete but not built yet ([docs/design](docs/design)) — today merging a changeset advances `main` and records the manifest.

## Get started

You need: a control plane URL (see [internal/README](internal/README.md) to run your own), git, and Docker (for local cognates).

### 1. Install the CLI

```sh
brew install benjaminsanborn/goku/goku
```

> Note: `brew install` works once this repo is public — Homebrew can't download release assets from a private repo. Until then: `gh release download v0.1.0 -p "goku_$(uname -s)_$(uname -m).tar.gz" -R benjaminsanborn/goku` and untar to your PATH, or `go build -o /usr/local/bin/goku ./cmd/goku` from a clone.

### 2. Log in

```sh
goku login
```

Paste the organization token your operator issued you (organizations are created by the operator — see [internal/README](internal/README.md)). The token is saved to `~/.config/goku/config`, and everything you create (projects, repos, changesets, audit log) is scoped to your org.

**That's the whole Claude integration too**: `goku login` registers goku with Claude Code automatically (a stdio MCP server, `goku mcp`, that reads your saved config — your token never enters Claude's own configuration). Every goku workspace also carries a committed `.mcp.json`, so any Claude opened inside one picks up the tools on its own. Manual fallback: `claude mcp add -s user goku -- goku mcp`.

Your Claude then has intent-shaped tools: `setup_project` ("set up a new goku project called demo"), `start_change` / `propose_change` ("in my goku project hello-world, fix the formatting" → branch, edit, submit for review), `add_resource`, `project_status`, `list_projects`, and `merge_change` (which it may only use when you explicitly ask).

Your Claude can now `list_projects`, `create_project`, `open_changeset`, `list_changesets`, and (when you ask it to) `merge_changeset` — every action attributed to `agent:claude` in the audit feed.

### 4. Create a project

```sh
goku new demo && cd demo
```

One command: creates the project, clones its repo, scaffolds `goku.yaml`, pushes the initial commit. It's immediately visible in the UI. From here `main` is protected — it only moves by merging a changeset.

### 5. Declare what you need; develop against cognates

```sh
goku add database main        # edits goku.yaml AND starts postgres:16 in docker
goku add storage assets       # MinIO with an S3-compatible API
goku run -- psql "$DATABASE_URL" -c 'select 1'
```

`goku.yaml` is the single source of truth: it declares *what the project needs*, and both planes materialize it — docker cognates locally, AWS on merge (soon). Your code reads `DATABASE_URL`, `STORAGE_ENDPOINT`, etc. and never knows which plane it's on.

### 6. Propose, review, merge

```sh
git checkout -b claude/my-feature
# ...write code (or let your Claude do it)...
git commit -am "Add my feature"
goku push -d "What this change does and why"
```

The changeset appears in the project changelog with the full diff — including any `goku.yaml` changes — attributed to whoever pushed. Review and hit **Merge** in the UI (or tell your Claude to merge). Direct pushes to `main` are rejected by the server.

## Command reference

| Command | Does |
|---|---|
| `goku login` | authenticate with your org token + connect Claude Code |
| `goku whoami` | show which org you're authenticated as |
| `goku mcp` | serve MCP over stdio for Claude (registered automatically) |
| `goku new <name>` | create project + clone + scaffold + first push |
| `goku clone <name>` | existing project → local workspace |
| `goku add <database\|storage> <name>` | add resource to manifest + start its cognate |
| `goku dev` | start cognates for everything in `goku.yaml` |
| `goku env` | print the injected env contract |
| `goku run -- <cmd>` | run anything with the env injected |
| `goku push [-t title] [-d desc]` | push branch + open changeset |
| `goku status` | project + changesets from the CLI |

Config: `~/.config/goku/config` (`GOKU_URL`, `GOKU_TOKEN`); env vars override.

## Learn more

- [internal/README](internal/README.md) — running and operating the control plane (`gokud`)
- [docs/design](docs/design) — the full design: architecture, data model, AWS provisioning, pipeline, security & compliance, roadmap
