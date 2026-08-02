# 13 — The architecture builder

The Architecture section of a project has an **Edit** button. It swaps the
read-only diagram (doc 08) for a 2d canvas: drag service, resource and route
nodes around, edit them in an inspector, and save the result as that branch's
`goku.yaml`.

## Shape of it

Five things go on the canvas, matching what the manifest can declare:

| node          | becomes                                             |
| ------------- | --------------------------------------------------- |
| API service   | `services.<name>.type: api` + port, health check     |
| Web service   | `services.<name>.type: web` + target, dist, spa      |
| Database      | `resources.<name>.type: database`                    |
| Storage       | `resources.<name>.type: storage`                     |
| Route         | a `routes[]` entry: domain → service, optional paths |

Wires are drawn, not dragged: a route connects to the service it names, and
every service connects to every resource — dashed, because the database env
contract (`DATABASE_URL`) reaches all of them whether or not the manifest says
so per service. Deleting a service also deletes routes that pointed at it,
since those wouldn't validate.

## The server renders the YAML

`PUT /v1/projects/{ref}/manifest` takes the canvas as JSON. With
`dry_run: true` it renders and returns the file without committing; that is
what the preview pane shows, so the preview is byte-for-byte what a save
writes.

Rendering server-side is what makes the builder safe to use on a manifest it
didn't write. The canvas models only what it can draw, so generating a whole
file from canvas state would silently drop `env` blocks, `host_mounts`, or
anything else hand-written. Instead each service spec is *merged* over the
existing one: builder-owned keys are replaced, everything else survives. Type
switches are the exception — moving a node from web to api drops `target`,
`dist` and `spa`, which no longer mean anything.

The result is parsed back through `deploy.ParseManifest` before it is
committed: a manifest the builder can produce but the deploy engine can't read
is a bug caught here rather than at deploy time.

## Layout lives in the manifest

Canvas positions round-trip through `goku.yaml` itself:

```yaml
layout:
  service/api: [40, 60]
  resource/db: [300, 60]
  route/0: [40, 200]
```

One file, no extra state to keep in sync with the repo, and the arrangement
travels with the branch. `ParseManifest` ignores the key, so it is inert to
everything downstream.

## Saving

Saves commit to the branch shown in the toolbar (the branch being viewed, by
default) via `gitrepo.CommitToBranch`, which — unlike `CommitFiles` — checks
out an existing branch instead of branching from main, and is a no-op when
nothing changed. Every save is audited as `manifest.save`.

## Open edges

- The canvas can't express `env` or `host_mounts`; those keys are preserved
  but only editable by hand.
- No undo, and no conflict detection: a save is a commit onto the branch tip
  as it stands at that moment.
- Saving to `main` is allowed. Whether the builder should force a branch
  (matching the propose-then-merge flow agents use) is worth deciding once
  there's a second author on a repo.
