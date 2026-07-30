# Platform (working title)

Compliant CI/CD into AWS, built for AI agents.

A control plane that lets an organization connect an AWS account, bootstrap a curated set of production-grade resources (database, API service, load balancer, storage, web frontend) inside an isolated VPC, and deploy to them via an opinionated push-to-deploy pipeline from a built-in git server. Agents — not humans — are the primary contributors: the platform ships an MCP interface so a Claude instance can be pointed at a project and safely ship changes, while humans review and approve in the UI.

Think **Aptible, but the customer is an AI agent** and the code host is part of the platform instead of GitHub.

## What works today

The control plane vertical slice: projects, a real git server with protected `main`, changesets (the platform's PR/approval unit), local workspaces with manifest-driven **local cognates** (a declared database runs Aurora in prod, `postgres:16` on your laptop, same env vars), an MCP interface for agents, and a UI for humans. **AWS provisioning and deployment are not built yet** — merging a changeset advances `main` and records the manifest, but nothing reaches AWS.

## Bootstrap walkthrough

### 0. Prereqs

Go 1.25+, Node 20+, PostgreSQL running locally, Docker (for local cognates), `git`.

### 1. Run the control plane

```sh
createdb platform_development          # once
cd web && npm install && npm run build && cd ..
go build -o bin/platformd ./cmd/platformd
go build -o bin/platform  ./cmd/platform    # the CLI — put bin/ on your PATH
./bin/platformd
```

One process serves the UI (http://localhost:8080), REST API (`/v1`), git (`/git/<project>.git`), and MCP (`/mcp`). Config via env: `DATABASE_URL`, `PORT`, `PLATFORM_TOKEN` (default `dev-token`), `PLATFORM_DATA` (bare repos, default `./data`), `WEB_DIST`.

### 2. Install into your Claude

```sh
claude mcp add --transport http platform http://localhost:8080/mcp \
  --header "Authorization: Bearer dev-token"
```

MCP tools: `list_projects`, `create_project`, `get_project`, `open_changeset`, `list_changesets`, `merge_changeset`. Every MCP action is attributed to `agent:claude` in the audit trail.

### 3. Create a project and workspace

```sh
platform new demo && cd demo
```

Creates the project (visible in the UI immediately), its bare repo on the platform, clones it into `./demo`, scaffolds `platform.yaml` + `.gitignore` + README, and pushes the initial commit. From here `main` is protected — it only moves by merging a changeset.

### 4. Declare a resource; get its local cognate

```sh
platform add database main     # edits platform.yaml AND starts postgres:16 in docker
platform add storage assets    # MinIO with an S3-compatible API
platform run -- psql "$DATABASE_URL" -c 'select 1'   # env injected automatically
```

The manifest is the single source of truth: it declares *what the project needs*, and both planes materialize it — AWS on merge (future), docker cognates locally (`platform dev` restarts them; `platform env` prints the contract). Application code reads `DATABASE_URL`, `STORAGE_ENDPOINT`, etc. and never knows which plane it's on.

### 5. Propose, review, merge

```sh
git checkout -b claude/hello-world
# ... write code (your Claude does this part) ...
git commit -am "Add hello world API"
platform push -d "What this change does and why"
```

`platform push` pushes the branch and opens a **changeset** — the platform's PR. It appears in the project's changelog in the UI showing the full diff versus `main`, including any `platform.yaml` changes, attributed to whoever pushed. A human clicks **Merge** (fast-forward) in the UI — or asks their Claude to call `merge_changeset`. Agents without a local workspace can propose via MCP `open_changeset` with file contents, and the platform commits the branch for them.

`platform status` shows the project and its changesets from the CLI; the UI shows the same plus the org-wide audit feed (`agent:*` actions badged amber, humans blue).

## Deploying the control plane to a server

The repo stays one unit; deployment splits the planes. The server runs `platformd` (API + git + MCP + UI); your workstation keeps only the `platform` CLI and an MCP registration pointing at the server.

```sh
# one-time host setup (the only sudo step; installs postgres, service user, systemd unit):
ssh -t ubuntu 'sudo bash -s' < scripts/server-bootstrap.sh

# every deploy after that (cross-compiles, rsyncs binary + UI, restarts, health-checks):
scripts/deploy.sh                       # PLATFORM_HOST=<ssh-alias> to override
```

Bootstrap generates a random `PLATFORM_TOKEN` (left in `~/.platform-token` on the server) and writes `/etc/platform/platformd.env`. Point your workstation at the server via `~/.config/platform/config`:

```
PLATFORM_URL=http://<server-ip>:8080
PLATFORM_TOKEN=<token>
```

and re-register MCP: `claude mcp add --transport http platform http://<server-ip>:8080/mcp --header "Authorization: Bearer <token>"`.

### What's deliberately not here yet

AWS connection and CloudFormation provisioning, the build/deploy pipeline, preview deploys, approval policies, and real token/identity management (the single dev token maps to `agent:claude`). The design for all of it is in `docs/design/`.

## Design docs

| Doc | Contents |
|---|---|
| [00-overview](docs/design/00-overview.md) | Vision, personas, product principles, non-goals |
| [01-architecture](docs/design/01-architecture.md) | Control plane / data plane split, component diagrams |
| [02-data-model](docs/design/02-data-model.md) | ERD and core schema |
| [03-provisioning](docs/design/03-provisioning.md) | AWS connection, resource catalog, VPC layout, provisioning engine |
| [04-git-and-cicd](docs/design/04-git-and-cicd.md) | Built-in git server, build & deploy pipeline |
| [05-interfaces](docs/design/05-interfaces.md) | REST API, MCP server, UI |
| [06-security-compliance](docs/design/06-security-compliance.md) | Identity, encryption, audit, agent guardrails, control mapping |
| [07-roadmap](docs/design/07-roadmap.md) | MVP cut, phases, open questions, risks |
| [08-ux-helloworld](docs/design/08-ux-helloworld.md) | The golden path: describe → changeset → preview → merge → live |
| [09-local-dev](docs/design/09-local-dev.md) | Git structure, local workspaces, local cognates, the CLI |

## Layout

```
cmd/platformd/       control plane server (REST + git + MCP + UI)
cmd/platform/        workspace CLI (new, clone, add, dev, env, run, push, status)
internal/store/      PostgreSQL persistence + audit
internal/server/     HTTP handlers: REST, MCP tools, git smart-HTTP, SPA
internal/gitrepo/    bare-repo plumbing: hooks, diffs, ff-merge, commit-from-files
web/                 React UI (Vite)
docs/design/         design docs (architecture diagrams in Mermaid)
```
