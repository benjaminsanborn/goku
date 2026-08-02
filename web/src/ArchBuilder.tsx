import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { api } from './api'
import type { Manifest } from './ArchDiagram'

// ArchBuilder is the visual editor behind the architecture diagram: drag
// service, resource and route nodes around a 2d canvas and save the result as
// the branch's goku.yaml.
//
// The YAML is rendered by the server (PUT .../manifest with dry_run), not
// here — so the preview pane is byte-for-byte what a save would commit, and
// keys the canvas can't draw (env, host_mounts) survive the round trip.

type ServiceNode = {
  kind: 'service'
  name: string
  type: 'api' | 'web'
  size?: string
  port?: number
  health_check?: string
  target?: string
  dist?: string
  spa?: boolean
}

type ResourceNode = { kind: 'resource'; name: string; type: 'database' | 'storage' }
type RouteNode = { kind: 'route'; domain: string; service: string; paths?: string[] }

type Node = ServiceNode | ResourceNode | RouteNode
type Positioned = { node: Node; x: number; y: number }

// NodePatch is a partial edit to one node. It stays a union rather than an
// intersection because the variants disagree about `type`.
type NodePatch =
  | Partial<Omit<ServiceNode, 'kind'>>
  | Partial<Omit<ResourceNode, 'kind'>>
  | Partial<Omit<RouteNode, 'kind'>>

const GRID = 20
const NODE_W = 190
const NODE_H = 74

// layoutKey identifies a node in the saved layout map. Routes are keyed by
// index because a domain may appear on several routes.
const layoutKey = (n: Node, i: number) =>
  n.kind === 'route' ? `route/${i}` : `${n.kind}/${n.name}`

const snap = (v: number) => Math.max(0, Math.round(v / GRID) * GRID)

const PALETTE: { label: string; hint: string; make: (n: number) => Node }[] = [
  { label: '+ API service', hint: 'container built from your Dockerfile', make: (n) => ({ kind: 'service', name: n ? `api${n}` : 'api', type: 'api', port: 8080 }) },
  { label: '+ Web service', hint: 'static assets served by caddy', make: (n) => ({ kind: 'service', name: n ? `web${n}` : 'web', type: 'web', spa: true }) },
  { label: '+ Database', hint: 'postgres container with a volume', make: (n) => ({ kind: 'resource', name: n ? `db${n}` : 'db', type: 'database' }) },
  { label: '+ Storage', hint: 'object storage bucket', make: (n) => ({ kind: 'resource', name: n ? `files${n}` : 'files', type: 'storage' }) },
  { label: '+ Route', hint: 'domain routed to a service', make: () => ({ kind: 'route', domain: 'example.com', service: '' }) },
]

// fromManifest seeds the canvas from goku.yaml, falling back to a tidy
// column layout for nodes that have never been placed.
function fromManifest(m: Manifest): Positioned[] {
  const saved = m.layout ?? {}
  const out: Positioned[] = []
  const place = (node: Node, i: number, col: number, row: number) => {
    const pos = saved[layoutKey(node, i)]
    out.push({ node, x: pos?.[0] ?? 40 + col * 260, y: pos?.[1] ?? 40 + row * 110 })
  }
  ;(m.routes ?? []).forEach((r, i) =>
    place({ kind: 'route', domain: r.domain, service: r.service, paths: r.paths }, i, 0, i),
  )
  ;(m.services ?? []).forEach((s, i) =>
    place(
      {
        kind: 'service',
        name: s.name,
        type: s.type === 'web' ? 'web' : 'api',
        size: s.size,
        port: s.port,
        health_check: s.health_check,
        target: s.target,
        dist: s.dist,
        spa: s.spa,
      },
      i,
      1,
      i,
    ),
  )
  ;(m.resources ?? []).forEach((r, i) =>
    place({ kind: 'resource', name: r.name, type: r.type === 'storage' ? 'storage' : 'database' }, i, 2, i),
  )
  return out
}

export default function ArchBuilder({
  projectRef,
  branch,
  manifest,
  onClose,
}: {
  projectRef: string
  branch: string
  manifest: Manifest
  onClose: () => void
}) {
  const [nodes, setNodes] = useState<Positioned[]>(() => fromManifest(manifest))
  const [selected, setSelected] = useState<number | null>(null)
  const [preview, setPreview] = useState('')
  const [error, setError] = useState('')
  const [saving, setSaving] = useState(false)
  const [saved, setSaved] = useState('')
  const [target, setTarget] = useState(branch)
  const canvasRef = useRef<HTMLDivElement>(null)
  const drag = useRef<{ index: number; dx: number; dy: number } | null>(null)

  const payload = useMemo(() => {
    const layout: Record<string, [number, number]> = {}
    let routeIndex = 0
    const services: unknown[] = []
    const resources: unknown[] = []
    const routes: unknown[] = []
    for (const { node, x, y } of nodes) {
      if (node.kind === 'service') {
        layout[layoutKey(node, 0)] = [x, y]
        services.push({
          name: node.name,
          type: node.type,
          size: node.size ?? '',
          port: node.port ?? 0,
          health_check: node.health_check ?? '',
          target: node.target ?? '',
          dist: node.dist ?? '',
          spa: !!node.spa,
        })
      } else if (node.kind === 'resource') {
        layout[layoutKey(node, 0)] = [x, y]
        resources.push({ name: node.name, type: node.type })
      } else {
        layout[`route/${routeIndex++}`] = [x, y]
        routes.push({ domain: node.domain, service: node.service, paths: node.paths ?? [] })
      }
    }
    return { services, resources, routes, layout }
  }, [nodes])

  // The preview is the server's rendering of the current canvas, debounced so
  // dragging a node doesn't hammer the endpoint.
  useEffect(() => {
    const id = setTimeout(() => {
      api<{ yaml: string }>(`/projects/${projectRef}/manifest`, {
        method: 'PUT',
        body: JSON.stringify({ ...payload, branch: target, dry_run: true }),
      })
        .then((r) => {
          setPreview(r.yaml)
          setError('')
        })
        .catch((e) => setError((e as Error).message))
    }, 350)
    return () => clearTimeout(id)
  }, [payload, projectRef, target])

  const addNode = (make: (n: number) => Node) => {
    setNodes((prev) => {
      // Name collisions are the common case when clicking a palette button
      // twice, so suffix until the name is free.
      let n = 0
      let node = make(0)
      const taken = (name: string) =>
        prev.some((p) => p.node.kind !== 'route' && p.node.name === name)
      while (node.kind !== 'route' && taken(node.name)) node = make(++n)
      const x = snap(60 + ((prev.length * 40) % 320))
      const y = snap(60 + prev.length * 30)
      setSelected(prev.length)
      return [...prev, { node, x, y }]
    })
  }

  const update = (index: number, patch: NodePatch) =>
    setNodes((prev) =>
      prev.map((p, i) => (i === index ? { ...p, node: { ...p.node, ...patch } as Node } : p)),
    )

  const remove = (index: number) => {
    setNodes((prev) => {
      const gone = prev[index].node
      const next = prev.filter((_, i) => i !== index)
      // A route pointing at a deleted service would fail validation, so drop
      // those with it rather than saving something that can't deploy.
      if (gone.kind === 'service') {
        const name = gone.name
        return next.filter((p) => !(p.node.kind === 'route' && p.node.service === name))
      }
      return next
    })
    setSelected(null)
  }

  // Pointer coordinates are canvas-relative, and the canvas scrolls, so the
  // scroll offset has to come back in or nodes jump when dragged in a
  // scrolled view.
  const canvasPoint = (e: React.PointerEvent) => {
    const el = canvasRef.current
    if (!el) return null
    const rect = el.getBoundingClientRect()
    return { x: e.clientX - rect.left + el.scrollLeft, y: e.clientY - rect.top + el.scrollTop }
  }

  const onPointerDown = (e: React.PointerEvent, index: number) => {
    const pt = canvasPoint(e)
    if (!pt) return
    const { x, y } = nodes[index]
    drag.current = { index, dx: pt.x - x, dy: pt.y - y }
    ;(e.target as HTMLElement).setPointerCapture(e.pointerId)
    setSelected(index)
  }

  const onPointerMove = (e: React.PointerEvent) => {
    const d = drag.current
    const pt = canvasPoint(e)
    if (!d || !pt) return
    const x = snap(pt.x - d.dx)
    const y = snap(pt.y - d.dy)
    setNodes((prev) => prev.map((p, i) => (i === d.index ? { ...p, x, y } : p)))
  }

  const onPointerUp = () => {
    drag.current = null
  }

  const save = useCallback(async () => {
    setSaving(true)
    setError('')
    setSaved('')
    try {
      const r = await api<{ committed: boolean; sha?: string; branch: string }>(
        `/projects/${projectRef}/manifest`,
        {
          method: 'PUT',
          body: JSON.stringify({ ...payload, branch: target, message: 'Update goku.yaml from the architecture builder' }),
        },
      )
      setSaved(r.committed ? `committed to ${r.branch} (${r.sha?.slice(0, 8)})` : 'no changes to commit')
    } catch (e) {
      setError((e as Error).message)
    }
    setSaving(false)
  }, [payload, projectRef, target])

  const services = nodes.filter((p) => p.node.kind === 'service')
  const current = selected !== null ? nodes[selected] : null

  return (
    <div className="builder">
      <div className="row" style={{ marginBottom: 10, flexWrap: 'wrap', gap: 6 }}>
        {PALETTE.map((p) => (
          <button key={p.label} className="btn ghost" title={p.hint} onClick={() => addNode(p.make)}>
            {p.label}
          </button>
        ))}
        <div className="spacer" />
        <input
          className="input"
          style={{ width: 200 }}
          value={target}
          onChange={(e) => setTarget(e.target.value)}
          title="branch this saves to"
        />
        <button className="btn" onClick={save} disabled={saving}>
          {saving ? 'Saving…' : 'Save goku.yaml'}
        </button>
        <button className="btn ghost" onClick={onClose}>
          Done
        </button>
      </div>

      <div className="builder-body">
        <div
          className="canvas"
          ref={canvasRef}
          onPointerMove={onPointerMove}
          onPointerUp={onPointerUp}
          onPointerDown={(e) => e.target === canvasRef.current && setSelected(null)}
        >
          <Wires nodes={nodes} />
          {nodes.map((p, i) => (
            <div
              key={i}
              className={`canvas-node ${p.node.kind} ${selected === i ? 'selected' : ''}`}
              style={{ left: p.x, top: p.y, width: NODE_W }}
              onPointerDown={(e) => onPointerDown(e, i)}
            >
              <div className="arch-name">{p.node.kind === 'route' ? p.node.domain : p.node.name}</div>
              <div className="arch-sub">{describe(p.node)}</div>
              <button
                className="canvas-x"
                onPointerDown={(e) => e.stopPropagation()}
                onClick={() => remove(i)}
                title="remove"
              >
                ×
              </button>
            </div>
          ))}
          {nodes.length === 0 && (
            <div className="canvas-hint">Add a service to start — drag nodes to arrange them.</div>
          )}
        </div>

        <div className="builder-side">
          {current ? (
            <Inspector
              node={current.node}
              services={services.map((s) => (s.node as ServiceNode).name)}
              onChange={(patch) => update(selected!, patch)}
            />
          ) : (
            <p className="page-sub">Select a node to edit it.</p>
          )}
          <div className="filebox" style={{ marginTop: 12 }}>
            <div className="path">goku.yaml · {target}</div>
            <pre>{preview || '…'}</pre>
          </div>
        </div>
      </div>

      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
      {saved && <p style={{ color: 'var(--green)' }}>{saved}</p>}
    </div>
  )
}

function describe(n: Node): string {
  if (n.kind === 'route') return n.service ? `HTTPS → ${n.service}` : 'no service selected'
  if (n.kind === 'resource') return n.type === 'database' ? 'postgres' : 'object storage'
  return n.type === 'api' ? `docker service${n.port ? ` :${n.port}` : ''}` : 'static server'
}

// Wires draws the edges the manifest implies: routes into their service, and
// each service to each resource (every service gets the database env
// contract, so the dependency is real even though goku.yaml doesn't spell it
// out per service).
function Wires({ nodes }: { nodes: Positioned[] }) {
  const center = (p: Positioned) => ({ x: p.x + NODE_W / 2, y: p.y + NODE_H / 2 })
  const lines: { from: Positioned; to: Positioned; dashed: boolean }[] = []
  for (const p of nodes) {
    if (p.node.kind === 'route') {
      const svc = nodes.find((n) => n.node.kind === 'service' && n.node.name === (p.node as RouteNode).service)
      if (svc) lines.push({ from: p, to: svc, dashed: false })
    }
    if (p.node.kind === 'service') {
      for (const r of nodes) {
        if (r.node.kind === 'resource') lines.push({ from: p, to: r, dashed: true })
      }
    }
  }
  return (
    <svg className="canvas-wires">
      {lines.map((l, i) => {
        const a = center(l.from)
        const b = center(l.to)
        return (
          <line
            key={i}
            x1={a.x}
            y1={a.y}
            x2={b.x}
            y2={b.y}
            stroke="var(--border)"
            strokeWidth={1.5}
            strokeDasharray={l.dashed ? '4 4' : undefined}
          />
        )
      })}
    </svg>
  )
}

function Inspector({
  node,
  services,
  onChange,
}: {
  node: Node
  services: string[]
  onChange: (patch: NodePatch) => void
}) {
  const field = (label: string, el: React.ReactNode) => (
    <label className="field">
      <span>{label}</span>
      {el}
    </label>
  )
  const text = (value: string, onInput: (v: string) => void, placeholder?: string) => (
    <input className="input" value={value} placeholder={placeholder} onChange={(e) => onInput(e.target.value)} />
  )

  if (node.kind === 'route') {
    return (
      <div className="inspector">
        <h3 className="section-h" style={{ marginTop: 0 }}>Route</h3>
        {field('domain', text(node.domain, (v) => onChange({ domain: v }), 'app.example.com'))}
        {field(
          'service',
          <select className="input" value={node.service} onChange={(e) => onChange({ service: e.target.value })}>
            <option value="">— pick a service —</option>
            {services.map((s) => (
              <option key={s} value={s}>
                {s}
              </option>
            ))}
          </select>,
        )}
        {field(
          'paths',
          text((node.paths ?? []).join(', '), (v) =>
            onChange({ paths: v.split(',').map((p) => p.trim()).filter(Boolean) }), '/api/*, /v1/* (blank = all)'),
        )}
      </div>
    )
  }

  if (node.kind === 'resource') {
    return (
      <div className="inspector">
        <h3 className="section-h" style={{ marginTop: 0 }}>Resource</h3>
        {field('name', text(node.name, (v) => onChange({ name: v })))}
        {field(
          'type',
          <select className="input" value={node.type} onChange={(e) => onChange({ type: e.target.value as 'database' | 'storage' })}>
            <option value="database">database (postgres)</option>
            <option value="storage">storage (objects)</option>
          </select>,
        )}
      </div>
    )
  }

  return (
    <div className="inspector">
      <h3 className="section-h" style={{ marginTop: 0 }}>Service</h3>
      {field('name', text(node.name, (v) => onChange({ name: v })))}
      {field(
        'type',
        <select className="input" value={node.type} onChange={(e) => onChange({ type: e.target.value as 'api' | 'web' })}>
          <option value="api">api (container)</option>
          <option value="web">web (static)</option>
        </select>,
      )}
      {field('size', text(node.size ?? '', (v) => onChange({ size: v }), 'small'))}
      {node.type === 'api' && (
        <>
          {field(
            'port',
            <input
              className="input"
              type="number"
              value={node.port ?? ''}
              placeholder="8080"
              onChange={(e) => onChange({ port: e.target.value ? Number(e.target.value) : undefined })}
            />,
          )}
          {field('health check', text(node.health_check ?? '', (v) => onChange({ health_check: v }), '/healthz'))}
        </>
      )}
      {node.type === 'web' && (
        <>
          {field('dockerfile stage', text(node.target ?? '', (v) => onChange({ target: v }), 'web'))}
          {field('dist directory', text(node.dist ?? '', (v) => onChange({ dist: v }), '/src/web/dist'))}
          <label className="field row" style={{ gap: 8 }}>
            <input type="checkbox" checked={!!node.spa} onChange={(e) => onChange({ spa: e.target.checked })} />
            <span>SPA fallback to index.html</span>
          </label>
        </>
      )}
    </div>
  )
}
