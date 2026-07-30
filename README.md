# goku

Compliant CI/CD into AWS, built for AI agents.

goku is a control plane your agents ship through: every project gets a git repository with a protected `main`, **branch-based review** (conventional branches, diffs, ff merges — like GitHub renders branches), and **deployments that materialize `goku.yaml` literally**: one container per service, a postgres container per database resource, path-routed HTTPS domains with automatic TLS. Branches deploy as their own live environments alongside main. Your Claude connects over MCP and proposes; you approve. Locally, the same manifest runs as **cognates** (postgres, MinIO in docker) with the identical env contract — and goku itself is deployed by goku.

> Deployment is **kamal-style containers on the goku host** today; AWS materialization of the same manifest (Fargate/Aurora) is design-complete and next ([docs/design](docs/design)). Current state + roadmap: [docs/design/11](docs/design/11-progress-and-next.md).

## Get started

You need: a control plane URL (see [internal/README](internal/README.md) to run your own), git, and Docker (for local cognates).

### 1. Install the CLI

```sh
brew install benjaminsanborn/goku/goku
```

(or grab a binary from [releases](https://github.com/benjaminsanborn/goku/releases))

### 2. Log in

```sh
goku login
```

Paste the organization token your operator issued you (organizations are created by the operator — see [internal/README](internal/README.md)). The token is saved to `~/.config/goku/config`, and everything you create (projects, repos, branches, audit log) is scoped to your org.

In the web UI you can instead **sign in with GitHub or Google** (when the operator has configured the providers) — on first sign-in you redeem the same org token once to join, and from then on every action you take is attributed to your own identity (`user:you@example.com`) in the audit feed rather than the shared token.

**That's the whole Claude integration too**: `goku login` registers goku with Claude Code automatically (a stdio MCP server, `goku mcp`, that reads your saved config — your token never enters Claude's own configuration). Every goku workspace also carries a committed `.mcp.json`, so any Claude opened inside one picks up the tools on its own. Manual fallback: `claude mcp add -s user goku -- goku mcp`.

Your Claude then has intent-shaped tools: `setup_project` ("set up a new goku project called demo"), `start_change` / `propose_change` ("in my goku project hello-world, fix the formatting" → branch, edit, submit for review), `add_resource`, `project_status`, `list_projects`, and `merge_change` (which it may only use when you explicitly ask).

### 3. Create a project — or import one from GitHub

```sh
goku new demo && cd demo                  # fresh project
goku import github.com/you/existing-app   # or bring an existing repo
```

`goku new` creates the project, clones its repo, scaffolds `goku.yaml`, and pushes the initial commit. `goku import` brings a GitHub repo over **with full history, branches, and tags** (private repos work once someone in your org has signed in with GitHub), untouched. Imported projects stay **linked to GitHub as the source of truth**: goku auto-syncs when you view them (or `goku sync` / the UI's Sync button), and merging happens on GitHub — goku follows. Adopting the goku standard (goku.yaml, Dockerfile) happens on a branch afterwards, which is exactly the kind of work your Claude is good at. Both are also available in the UI (the create field accepts `owner/repo`) and as MCP tools. From here `main` is protected — it only moves by merging a branch.

### 4. Declare what you need; develop against cognates

```sh
goku add database main        # edits goku.yaml AND starts postgres:16 in docker
goku add storage assets       # MinIO with an S3-compatible API
goku run -- psql "$DATABASE_URL" -c 'select 1'
```

`goku.yaml` is the single source of truth: it declares *what the project needs*, and both planes materialize it — docker cognates locally, AWS on merge (soon). Your code reads `DATABASE_URL`, `STORAGE_ENDPOINT`, etc. and never knows which plane it's on.

### 5. Propose, review, merge

```sh
git checkout -b feature/my-feature     # conventional branches: feature/ bugfix/ hotfix/ chore/ release/
# ...write code (or let your Claude do it)...
git commit -am "feat: add my feature"
goku push
```

The branch appears on the project page with its diff against `main` — including any `goku.yaml` changes, and the architecture diagram for that branch. Review and hit **Merge** in the UI (fast-forward + branch cleanup), or tell your Claude to merge. Direct pushes to `main` are rejected by the server.

### 6. Deploy — environments

Merging `main` auto-deploys it (native projects; linked projects deploy on GitHub pushes via webhook). Any branch can also become its **own live environment** — the **+** button in the UI (or `goku deploy <branch>`, or your Claude) ties a branch to a fleet instance and deploys it with its own containers and database at `https://<branch>--<project>.goku.host`, alongside main. Each environment shows its deploy history with live logs; every service container's logs live-tail from the UI, `goku logs [service] [-f]`, or the MCP `service_logs` tool. goku itself ships this way: pushing this repo to GitHub makes the production goku build and replace itself.

## Command reference

| Command | Does |
|---|---|
| `goku login` | authenticate with your org token + connect Claude Code |
| `goku whoami` | show which org you're authenticated as |
| `goku mcp` | serve MCP over stdio for Claude (registered automatically) |
| `goku new <name>` | create project + clone + scaffold + first push |
| `goku clone <name>` | existing project → local workspace |
| `goku sync [name]` | pull latest from a linked GitHub repo |
| `goku add <database\|storage> <name>` | add resource to manifest + start its cognate |
| `goku dev` | start cognates for everything in `goku.yaml` |
| `goku env` | print the injected env contract |
| `goku run -- <cmd>` | run anything with the env injected |
| `goku push` | push the current branch for review |
| `goku deploy [branch] [--on inst]` | deploy a branch as a live environment |
| `goku logs [service] [-f]` | tail a deployed container's logs |
| `goku secrets set\|list\|rm` | write-only env injected into deployments |
| `goku status` | project + branches from the CLI |

Config: `~/.config/goku/config` (`GOKU_URL`, `GOKU_TOKEN`); env vars override.

## Learn more

- [internal/README](internal/README.md) — running and operating the control plane (`gokud`)
- [docs/design](docs/design) — the full design: architecture, data model, AWS provisioning, pipeline, security & compliance, roadmap
