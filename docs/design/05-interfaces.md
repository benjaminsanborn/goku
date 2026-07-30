# 05 — Interfaces: REST API, MCP, UI

All three interfaces are adapters over one domain layer — identical authz, identical audit. MCP is not a bolt-on; it's a peer of REST.

## REST API (Go)

Conventional resource-oriented JSON API; the UI and MCP server consume the same domain services (MCP in-process, UI via HTTP).

```
POST   /v1/orgs                                  # bootstrap
POST   /v1/orgs/{org}/agents                     # register agent identity
POST   /v1/orgs/{org}/tokens                     # mint scoped token (user or agent actor)
POST   /v1/orgs/{org}/projects
GET    /v1/projects/{id}                         # includes resource + deploy status rollup
POST   /v1/projects/{id}/aws-connection          # start connect flow, returns quick-create URL
POST   /v1/projects/{id}/resources               # {type, name, config} → revision 1
PATCH  /v1/projects/{id}/resources/{rid}         # new revision (may create approval instead)
GET    /v1/projects/{id}/resources/{rid}/revisions
POST   /v1/projects/{id}/deployments             # deploy specific sha or redeploy (rollback)
GET    /v1/projects/{id}/deployments/{n}
GET    /v1/projects/{id}/deployments/{n}/logs    # build + release logs
GET    /v1/projects/{id}/logs?resource=api&since=...   # runtime logs (streamed)
GET    /v1/projects/{id}/approvals               # pending gates
POST   /v1/approvals/{id}/decision               # human only
GET    /v1/orgs/{org}/audit?since=...            # audit export
```

Auth: session cookies (UI humans) or `Authorization: Bearer` scoped tokens (agents, CLI). RBAC: `owner | member | auditor` at org level; token scopes narrow further.

## MCP server

Streamable HTTP endpoint (`/mcp`) speaking the current MCP spec; auth via OAuth 2.1 (platform is the authorization server) or pre-provisioned scoped tokens for headless agents. Installation story: *"Add `https://platform.example/mcp` to your Claude — your agent can now see and ship to your projects."*

### Tools (initial set)

| Tool | Scope needed | Notes |
|---|---|---|
| `list_projects` | `project:read` | id, name, status rollup |
| `get_project` | `project:read` | resources, endpoints (ALB/CloudFront URLs), git remote URL, recent deploys |
| `get_deployment` / `list_deployments` | `deploy:read` | structured status incl. failure reason |
| `get_logs` | `logs:read` | build, release, or runtime; bounded tail |
| `open_changeset` | `repo:push` | branch + description → changeset in the changelog |
| `deploy_preview` | `deploy:preview` | ephemeral branch deploy, returns preview URL ([08](08-ux-helloworld.md)) |
| `merge_changeset` | `deploy:trigger` | policy-gated; human approval by default when manifest changes |
| `trigger_deploy` | `deploy:trigger` | redeploy HEAD of main or named sha |
| `rollback` | `deploy:trigger` | to previous or numbered deployment |
| `get_manifest_plan` | `project:read` | rendered CFN change-set preview + cost delta for a changeset |
| `get_pending_approvals` | `project:read` | so an agent can tell its human "waiting on you" |
| `create_project` | `project:admin` | usually operator-gated |
| `get_git_credentials` | `repo:push` | short-lived push credential for the project remote |

Deliberately absent as tools: delete anything, mint tokens, change policy — those are human/UI-only.

### Resources & prompts

- MCP resources expose read-only docs: project status doc, "how to deploy here" instructions, catalog reference — so a fresh Claude session self-orients without the human pasting context.
- A `deploy_and_verify` prompt template encodes the golden path (push → watch → check health → report), nudging agent behavior into the audited flow.

Code push itself stays on **git over HTTPS** (agents run `git push` via their shell tools with the short-lived credential) — MCP carries everything *around* the push. Rationale: git preserves real history/diffs and works with every agent harness; an MCP `write_files` pseudo-commit tool would fork the source of truth. Revisit if demand appears for shell-less agents.

## UI (React SPA)

Read-heavy, policy-write-only surface for operators and auditors:

1. **Project overview** — resource cards with live status (active / provisioning / drifted / failed), endpoints, cost estimate
2. **Deployments** — timeline: sha, actor (human/agent badge), build + deploy status, one-click rollback, streamed logs
3. **Resources** — config revision history, pending change sets w/ rendered diff, drift flags
4. **Approvals inbox** — the human-in-the-loop queue: agent-proposed changes with diffs, approve/deny
5. **Agents & tokens** — registry, last-seen, scopes, revoke
6. **Audit log** — filterable, exportable
7. **Settings** — AWS connection status, policies (approval gates, spend ceiling), members

Live updates via SSE from the API (deploy status, provisioning progress). No infra mutations from the UI except approvals and (human-confirmed) deletes — the UI is primarily *the place humans watch what agents do*.
