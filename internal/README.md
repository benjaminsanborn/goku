# Operating the goku control plane

`gokud` is a single binary serving three surfaces on one port: the REST API (`/v1`), the git server (`/git/<project>.git`, smart HTTP), and the web UI (everything else). The MCP surface lives in the CLI (`goku mcp`, stdio) and talks to the REST API. State lives in PostgreSQL plus bare git repos on disk.

## Package layout

```
cmd/gokud/           server entrypoint
cmd/goku/            workspace CLI (thin client over the REST API + docker)
internal/store/      PostgreSQL persistence + append-only audit events
internal/server/     HTTP handlers: REST, git smart-HTTP, OAuth, SPA serving
internal/gitrepo/    bare-repo plumbing: hooks, diffs, ff-merge, commit-from-files
web/                 React UI (Vite); built assets served by gokud
scripts/             server bootstrap + deploy
```

## Configuration (env)

| Var | Default | Meaning |
|---|---|---|
| `DATABASE_URL` | `postgres://localhost:5432/goku_development` | PostgreSQL DSN |
| `PORT` | `8080` | listen port |
| `GOKU_TOKEN` | `dev-token` | the bearer token for API/MCP/git (single-token dev auth) |
| `GOKU_DATA` | `./data` | bare repos live in `$GOKU_DATA/repos` |
| `WEB_DIST` | `web/dist` | built UI assets |
| `GOKU_BASE_URL` | `http://localhost:$PORT` | public URL used in git remotes and links |

Auth model: organizations are provisioned **by the operator, on the server host only** — there is deliberately no network signup:

```sh
ssh <host> 'sudo -n -u goku /opt/goku/bin/gokud create-org <name>'
```

This mints an org-scoped `gk_*` bearer token (sha256-hashed at rest in the `tokens` table; plaintext shown once) — hand it to the user, who runs `goku login`. Every API route requires auth; all data — projects, repos on disk (`repos/<org-id>/`), audit events — is org-scoped. The root `GOKU_TOKEN` from the env maps to the built-in `default` org and is meant for the operator.

**Human sign-in (SSO)**: the UI supports GitHub and Google OAuth once configured in `/etc/goku/gokud.env` (`GOKU_GITHUB_CLIENT_ID/SECRET`, `GOKU_GOOGLE_CLIENT_ID/SECRET`; callback URLs `https://<domain>/auth/{github,google}/callback`; GitHub scope includes `repo` to power private-repo import). SSO is **login, not signup**: a new user lands on a join screen and redeems an org token once; membership recorded, and their writes audit as `user:<email>`. Sessions are 30-day HttpOnly cookies (hashed in the `sessions` table). Agents and the CLI keep using bearer tokens (`agent:*` actors).

**GitHub import**: `POST /v1/projects/import` bare-clones the repo into the org's repo dir (all branches/tags preserved, `main` normalized as default + protected); no changes are made to the code. Private repos use the importing user's GitHub OAuth token, falling back to any org member's.

Releases: GoReleaser runs on `v*` tags ([.github/workflows/release.yml](../.github/workflows/release.yml)) — CLI binaries for darwin/linux, `gokud` for linux, plus a Homebrew formula pushed to [benjaminsanborn/homebrew-goku](https://github.com/benjaminsanborn/homebrew-goku). Requires the `GH_TOKEN` repo secret (a PAT that can push to the tap).

## Run locally

```sh
createdb goku_development       # schema auto-migrates on boot
cd web && npm install && npm run build && cd ..
go build -o bin/gokud ./cmd/gokud && ./bin/gokud
```

## The control plane is self-hosted

goku.host is served by a container that goku built from its own repo
(`goku-app/goku:<sha>`): push to main on GitHub → webhook → the running
container builds, health-checks, routes, and replaces itself (blue-green via
per-deployment ports; the old container is stopped last). Its config lives in
the goku project's own **secrets** (token, DATABASE_URL with the goku role's
password, OAuth, webhook secret, mounts paths); `host_mounts` in goku.yaml
(operator-org only) give it the machine: `/var/lib/goku` repos, docker.sock,
`/etc/goku` (apps.caddy), `/etc/caddy` (ro).

Environments are per-branch: each gets its own service containers, database
containers, and hostname (`<branch>--<project>.<domain>`); `main` keeps the
manifest domains. Fleet instances enroll over SSH and are verified, with
placement recorded per deployment (ssh deploy driver pending).

Databases (the control plane's own included) run as per-project postgres:18
containers (`goku-db-<project>-<resource>`, named volume, 127.0.0.1-published
port); the host postgres is no longer used and holds a stale pre-migration
copy of the goku db as a cold fallback.

**Backups**: nightly (a loop in gokud runs when >20h stale; manually:
`sudo -u goku /opt/goku/bin/gokud backup`) — dumps every `goku-db-*`
container + the repos into `/var/lib/goku/backups/` (7 kept), encrypts with
`/etc/goku/backup.key` (KEEP A COPY IN A PASSWORD MANAGER), and force-pushes
the latest bundle to the private `GOKU_BACKUP_REPO` GitHub repo (RESTORE.md
included there; restore was tested end-to-end).

**SSH deploy driver**: `--on <instance>` builds on the instance from a piped
`git archive`, runs remote db + service containers, health-checks over ssh,
and routes the central Caddy at `instance:port`. Web services and
host_mounts are local-only for now; capacity-1 per ssh instance.

**Break-glass**: if the container is wedged, the pre-cutover binary and unit
still exist: `sudo systemctl start gokud` (serves :8080) and point goku.host
at it in `/etc/goku/apps.caddy` (`reverse_proxy localhost:8080`), then
`caddy reload --config /etc/caddy/Caddyfile`. A pre-cutover DB dump lives in
`/tmp/goku-precutover-*.sql.gz` (move it somewhere durable).

## Deploy to a server (fresh install)

Target: any Ubuntu 24.04 host reachable over ssh with passwordless sudo for your user.

```sh
# one-time host setup (the only sudo step):
ssh <host> 'sudo -n bash -s' < scripts/server-bootstrap.sh

# first deploy only (cross-compile + rsync binary/UI + restart + health check);
# after that the control plane self-deploys from GitHub pushes:
scripts/deploy.sh               # GOKU_HOST=<ssh-alias> (default: ubuntu)
```

Bootstrap is idempotent and provisions:

- **PostgreSQL 18** from the PGDG repo (refuses to touch a pre-existing cluster on 5432 — resolve manually)
- **Caddy** with automatic Let's Encrypt for `goku.host` (edit `DOMAIN`/`ACME_EMAIL` at the top of the script), terminating TLS on 80/443 and proxying to gokud on 8080
- service user `goku`, hardened systemd unit `gokud.service`
- a generated random `GOKU_TOKEN` → `/etc/goku/gokud.env`, copied to `~/.goku-token` for the deploy user
- a sudoers rule so deploys can restart the service without a password

Server filesystem:

| Path | Contents |
|---|---|
| `/opt/goku/bin/gokud`, `/opt/goku/web/dist` | deploy artifacts (owned by deploy user) |
| `/var/lib/goku/repos/*.git` | bare project repos (owned by service user) |
| `/etc/goku/gokud.env` | config incl. token |
| `/etc/caddy/Caddyfile` | TLS + reverse proxy |

### Exposing it publicly

- DNS: `A @` and `A *` records → your public IP
- Router: forward TCP 80 and 443 to the server; nothing else (8080 stays LAN-only)
- Certs issue automatically; if Caddy is stuck in ACME backoff after a DNS fix, `sudo systemctl restart caddy`

## Troubleshooting

```sh
ssh <host> sudo -n systemctl status gokud
ssh <host> sudo -n journalctl -u gokud -n 100
ssh <host> sudo -n journalctl -u caddy -n 50     # cert issuance
ssh <host> 'psql "postgres://goku@/goku?host=/var/run/postgresql"'   # as a sudoer via: sudo -u goku ...
```

**Deployments (kamal-style)**: `internal/deploy` builds images from the bare repo (`git archive` → `docker build`), provisions `goku_app_<project>` postgres roles/dbs, runs containers with host networking + `PORT`/`DATABASE_URL`/`DATA_DIR`, and writes per-app Caddy site blocks to `/etc/goku/apps.caddy` (reloaded via Caddy's admin API — no sudo). Config: `GOKU_APPS_CADDY`, `GOKU_APP_DOMAIN`. The unit needs docker group membership, `ReadWritePaths=/etc/goku/apps.caddy`, and the goku role needs `CREATEDB CREATEROLE` (all in bootstrap).

Notes from the field:

- `GIT_PROJECT_ROOT` must be absolute — gokud resolves `GOKU_DATA` at startup.
- Branch state (open/merged, diffs) lives in git, not the DB; test cleanup is `delete from audit_events; delete from projects` plus removing the repo dir.
