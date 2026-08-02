# 14 — Databases

A **database** is a managed Postgres server goku runs on a dedicated EC2
instance: pick a size, get a tuned `postgresql.conf`, connect. RDS-shaped,
minus the parts of RDS nobody asked for.

This is a different object from the per-project `resources.<name>.type:
database` containers (doc 10), and deliberately unconnected to them for now —
project database UX is likely to be redesigned, and coupling the two before
that happens would constrain both.

## Model

`databases` is org-scoped, one row per server:

| column               | meaning                                                    |
| -------------------- | ---------------------------------------------------------- |
| `provider_id`        | the AWS account it runs in                                  |
| `engine_version`     | `18` — the only supported version today                     |
| `instance_type`      | EC2 type, from a curated list with known CPU/RAM            |
| `storage_gb`         | size of the **data volume**, separate from the root disk    |
| `status`             | `creating` → `available` \| `modifying` \| `rebooting` \| `failed` \| `deleting` |
| `endpoint`           | host clients connect to                                     |
| `config_overrides`   | jsonb: parameters the operator set, over the computed base  |
| `ssh_key`, `password`| write-only, like instance keys and project secrets          |
| `event_log`          | the activity feed shown on the database page                |

### Stateful by construction

Three decisions exist to keep the follow-on features (replicas, pgbouncer,
cloning) from requiring a rebuild:

1. **Data lives on its own EBS volume**, mounted at `/var/lib/goku-pgdata`,
   never on the root disk. Vertical resize, instance replacement, volume
   growth, and snapshot-based cloning all become operations on a volume rather
   than a migration.
2. **Clients get an endpoint, never a raw IP in an app's head.** On a tailnet
   the instance registers as `goku-db-<name>`, and promotion of a replica or
   the insertion of a pooler is a change on goku's side.
3. **Delete is gated** behind typing the database name, and says plainly that
   the volume goes with it. Backups are deferred (below), which makes the
   gate the only protection there is — so it is deliberately annoying.

## Postgres runs in a container

`postgres:18`, with the data volume bind-mounted and the config file mounted
read-only. Same shape as the deploy engine's database containers, version
pinning is trivial, and a failed config change is one `docker restart` away
from being diagnosed. The instance is otherwise an ordinary provisioned
machine — same `cloud.Provision` path as the fleet, same two ingress models
(tailnet with no public inbound; otherwise 22 and 5432 locked to the control
plane's egress `/32`).

## Configuration: computed base + overrides

`postgresql.conf` is generated from the instance type's CPU and memory —
`shared_buffers` at a quarter of RAM, `effective_cache_size` at three
quarters, `max_connections` and parallel workers scaled to the box, gp3-shaped
`random_page_cost` and `effective_io_concurrency`.

Overrides are stored as parameters, not as a file. The rendered file is shown
read-only; the operator edits individual values. This is what keeps "scales
with instance size" true after a resize: the base recomputes, the overrides
survive. A whole-file editor would silently pin a 2GB box's `shared_buffers`
onto a 64GB one.

`wal_level` stays at the stock default and `archive_mode` stays off: no
replication plumbing until replicas exist, at which point enabling it is a
restart, which is acceptable. The one preload set at creation is
`pg_stat_statements`, because adding it later is a restart and the stats
daemon will want it.

### Applying a change

Parameters divide into reload-safe and restart-required (`postmaster` context:
`shared_buffers`, `max_connections`, `shared_preload_libraries`, `wal_level`,
…). The UI says which before the operator commits, because "this will
interrupt connections" is the whole difference between the two.

Applying keeps the previous file. If Postgres doesn't come back healthy, goku
restores it, restarts, and surfaces the startup error — a database that won't
start because of a typo'd value is the worst failure this feature can have, so
it is the one path that is explicitly engineered.

## Connecting

The database page shows what a client needs: endpoint, port, superuser role,
and a password that is revealed on request (audited) rather than displayed by
default. Alongside it, the logical databases on the server, and an **add
database** action that creates the database plus an owner role and hands back
a ready connection string once.

That is the whole last mile for v1. Nothing wires these into projects yet.

## Logs

The Postgres container's log, tailed over ssh, in the same drawer shape the
deploy engine uses. `log_min_duration_statement` defaults to 1s so slow
queries actually appear — an empty log pane reads as broken.

## Deferred, and why

- **Stats** — an existing stats daemon will be plugged in; the page leaves a
  place for it rather than growing a second metrics path.
- **Backups** — deserves its own design (logical dumps vs. snapshots vs. PITR,
  and PITR implies `archive_mode`, which implies a restart). Until then the
  delete gate is the only guard, and the UI says so.
- **Replicas, pgbouncer, cloning** — the page has sections for them so the
  information architecture doesn't have to be rearranged when they land.

## Open edges

- No resize yet: changing instance type is a stop/start on the same volume,
  and is the next natural operation.
- No volume growth, which is the failure mode most likely to be hit first.
- Credentials are plaintext in Postgres, like every other secret goku holds
  (doc 06's KMS phase covers all of them).
- A tailnet endpoint is an address, not a name, until the control plane
  resolves MagicDNS; the stable-name promise is only half-kept.
