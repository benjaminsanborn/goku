# Operating the goku control plane

`gokud` is a single binary serving four surfaces on one port: the REST API (`/v1`), the git server (`/git/<project>.git`, smart HTTP), the MCP endpoint (`/mcp`), and the web UI (everything else). State lives in PostgreSQL plus bare git repos on disk.

## Package layout

```
cmd/gokud/           server entrypoint
cmd/goku/            workspace CLI (thin client over the REST API + docker)
internal/store/      PostgreSQL persistence + append-only audit events
internal/server/     HTTP handlers: REST, MCP tools, git smart-HTTP, SPA serving
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

This mints an org-scoped `gk_*` bearer token (sha256-hashed at rest in the `tokens` table; plaintext shown once) — hand it to the user, who runs `goku login`. Every API route requires a token; all data — projects, repos on disk (`repos/<org-id>/`), changesets, audit events — is scoped to the token's org. The root `GOKU_TOKEN` from the env maps to the built-in `default` org and is meant for the operator. `X-Goku-Actor: operator` distinguishes the UI from agents in the audit trail; per-agent identities are designed in [docs/design/06](../docs/design/06-security-compliance.md).

Releases: GoReleaser runs on `v*` tags ([.github/workflows/release.yml](../.github/workflows/release.yml)) — CLI binaries for darwin/linux, `gokud` for linux, plus a Homebrew formula pushed to [benjaminsanborn/homebrew-goku](https://github.com/benjaminsanborn/homebrew-goku). Requires the `GH_TOKEN` repo secret (a PAT that can push to the tap).

## Run locally

```sh
createdb goku_development       # schema auto-migrates on boot
cd web && npm install && npm run build && cd ..
go build -o bin/gokud ./cmd/gokud && ./bin/gokud
```

## Deploy to a server

Target: any Ubuntu 24.04 host reachable over ssh with passwordless sudo for your user.

```sh
# one-time host setup (the only sudo step):
ssh <host> 'sudo -n bash -s' < scripts/server-bootstrap.sh

# every deploy (cross-compile + rsync binary/UI + restart + health check):
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

Notes from the field:

- The MCP endpoint disables the Go SDK's localhost/DNS-rebinding protection because Caddy proxies to loopback with a public `Host` header; the bearer-token requirement is the actual guard.
- `GIT_PROJECT_ROOT` must be absolute — gokud resolves `GOKU_DATA` at startup.
- Deleting a project's rows requires deleting its changesets first (FK); test cleanup is `truncate changesets, audit_events cascade; delete from projects`.
