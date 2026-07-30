# 11 — Where we are, and what's next

_Last updated: v0.8.0, July 2026._

## Built and running (goku.host)

**Self-hosted control plane.** gokud runs as a container that goku built from
its own repo; pushes to main on GitHub → webhook → the running container
builds, health-checks, routes, and replaces itself (blue-green via
per-deployment ports, old container stopped last). Its postgres is a
container it provisioned; its config lives in its own secrets.

**Identity & orgs.** Operator-created organizations (`gokud create-org`),
org-scoped `gk_*` tokens, GitHub SSO (login-not-signup; join by redeeming a
token), 30-day sessions, and an audit trail with real actors
(`user:email`, `agent:claude`, `system:github`, `system:merge`).

**Git.** Hosted bare repos over smart HTTP with protected `main`
(pre-receive), conventional-branch review (diff vs main, ahead/behind,
ff merge + branch cleanup), and GitHub-linked projects: full-history import,
auto-sync on view, push webhooks, merges happen on GitHub and goku follows.

**Manifest & deploys.** `goku.yaml` declares services (api, web),
database resources, and path-matched routes. Deploys materialize it
literally: one container per service (web as a synthesized static-caddy
image from a Dockerfile stage), one postgres:18 container per database
resource (persistent volume), Caddy site blocks with automatic TLS.
Secrets are write-only and injected at deploy. Deploy-on-merge for native
projects; push-to-deploy via webhook for linked ones.

**Environments.** Each branch can be a live environment: own containers, own
database, own hostname (`<branch>--<project>.goku.host`), independent
supersede, stop/teardown. The UI's Deployments section is environment-first
with expandable per-env history.

**Fleet.** SSH instances enroll with a .pem (write-only), get verified
(reachability, docker, facts) with remediation hints, and show live
assignments. Placement is recorded per deployment and capacity-1 is enforced
for ssh members. **The ssh deploy driver is not yet implemented** — picking a
remote instance returns an honest error.

**Surfaces.** CLI (brew-released): login/whoami/mcp/new/import/clone/sync/
add/dev/env/run/push/deploy [--on]/logs [-f]/secrets/status. MCP over stdio
with intent-shaped tools (auto-registered by `goku login`, plus committed
`.mcp.json` per workspace). Responsive web UI with live log tailing per
container. Local dev cognates (postgres/minio) off the same manifest.

## Next improvements, in rough priority order

1. ~~**Data durability.**~~ **Done (v0.9.0)**: nightly encrypted bundles
   (db dumps + repos) with local retention and off-box push to a private
   GitHub repo; restore tested.
2. ~~**SSH deploy driver.**~~ **Done (v0.9.0)**: remote builds from piped
   archives, remote db/service containers, ssh health checks, central
   routing to instance ports; verified against a loopback-enrolled instance.
   Remaining gaps: web services and host_mounts are local-only; remote log
   tailing routes through the local docker only.
3. **Runtime health monitoring.** A deployment that passes its health check
   and crashes an hour later still shows "healthy". A monitor loop should
   re-check containers, flip status, surface restarts in the UI, and alert.
4. **Rollback & redeploy.** Deploy accepts branch head only; add deploy-by-sha
   so any historical row gets a "redeploy this" button (rollback = redeploy
   old sha).
5. **Token & permission hardening.** Per-person/per-agent tokens with labels,
   last-used, and revocation UI (schema already supports it); scoped
   permissions; secrets encrypted at rest; CSRF tokens for session writes;
   rate limits on auth endpoints.
6. **Linked-repo default branches.** Upstreams whose default branch isn't
   `main` (e.g. master) leave goku's `main` frozen at import time. Sync
   should map the upstream default branch onto `main`.
7. **Build pipeline ergonomics.** Stream `docker build` output into the
   deploy log live (drop `-q`), build timeouts, image/volume GC policy,
   and a deploy queue instead of 409 on concurrent requests.
8. **More service/resource types.** `worker` (no route, no health port),
   `cron`, and `storage` materialized in deploys (minio container server-side,
   mirroring the local cognate).
9. **Merge ergonomics.** ff-only forces local rebases; add server-side
   merge commits or rebase-and-merge.
10. **Observability.** Deploy duration metrics, per-env uptime checks, log
    persistence beyond container lifetime, and a project activity feed.
11. **AWS materialization** (docs 01–03, the original thesis). The fleet
    model is the bridge: first EC2 provisioning that self-enrolls instances,
    then the same manifest as Fargate + Aurora + ALB/CloudFront via
    CloudFormation, with the env contract unchanged.
12. **Claude distribution.** A goku plugin (bundling the MCP config + a
    workflow skill), and eventually OAuth on a remote MCP endpoint for
    claude.ai users.

## Known quirks

- Local instance facts report the control-plane *container's* OS (Alpine),
  not the host's.
- The `main` instance field is empty on pre-instance-column deployments.
- Host postgres still runs with a stale pre-migration copy (cold fallback);
  decommission after backups exist.
- Break-glass: pre-cutover gokud binary + systemd unit remain on the host
  (see internal/README).
