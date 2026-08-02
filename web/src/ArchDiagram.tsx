export type Manifest = {
  adopted: boolean
  branch: string
  error?: string
  services?: {
    name: string
    type: string
    size?: string
    port?: number
    health_check?: string
    target?: string
    dist?: string
    spa?: boolean
  }[]
  resources?: { name: string; type: string }[]
  routes?: { domain: string; service: string; paths?: string[] }[]
  // layout holds the architecture builder's canvas positions as [x, y],
  // keyed "service/api", "resource/db", "route/0".
  layout?: Record<string, [number, number]>
}

export type Unit = {
  name: string
  kind: 'service' | 'database'
  type: string
  container?: string
  instance?: string
  status: 'running' | 'stopped' | 'not_deployed'
  uptime?: string
  image?: string
  port?: number
}

// shortImage keeps image labels readable: goku-app/goku:47084b616879 → goku:47084b61
const shortImage = (img: string) => {
  const [name, tag] = img.split(':')
  const base = name.split('/').pop() ?? name
  return tag && tag.length > 8 ? `${base}:${tag.slice(0, 8)}` : img === name ? base : `${base}:${tag}`
}

const TYPE_LABEL: Record<string, string> = {
  api: 'docker service',
  web: 'static server',
  database: 'postgres',
  storage: 'object storage',
}

// ArchDiagram renders the architecture a branch's goku.yaml declares:
// edge (routes) → services → resources. When units are deployed, service and
// resource nodes are grouped inside the instance that runs them and click
// through to live logs.
export default function ArchDiagram({
  manifest,
  units,
  onLogs,
}: {
  manifest?: Manifest
  units?: Unit[]
  onLogs?: (u: Unit) => void
}) {
  if (!manifest) return null
  if (!manifest.adopted) {
    return (
      <div className="empty">
        <p>
          No <code>goku.yaml</code> on <b>{manifest.branch}</b> — this branch hasn't adopted goku yet.
        </p>
        <p>
          Ask your Claude to adopt it, or add <code>goku.yaml</code> in a branch.
        </p>
      </div>
    )
  }
  if (manifest.error) {
    return (
      <div className="empty">
        <p>{manifest.error}</p>
      </div>
    )
  }
  const services = manifest.services ?? []
  const resources = manifest.resources ?? []
  const routes = manifest.routes ?? []
  if (services.length === 0 && resources.length === 0) {
    return (
      <div className="empty">
        <p>
          <code>goku.yaml</code> declares nothing yet — add services and resources via branches or <code>goku add</code>.
        </p>
      </div>
    )
  }

  const unitFor = (name: string, kind: string) => units?.find((u) => u.name === name && u.kind === kind)
  const deployedUnits = (units ?? []).filter((u) => u.container)
  const instanceName = deployedUnits[0]?.instance

  const node = (
    key: string,
    kind: 'service' | 'database',
    cls: string,
    title: string,
    sub: string,
    meta: string,
  ) => {
    const u = unitFor(key, kind)
    const clickable = !!(u?.container && onLogs)
    return (
      <div
        key={kind + key}
        className={`arch-node ${cls} ${clickable ? 'clickable' : ''}`}
        onClick={() => clickable && onLogs!(u!)}
        title={clickable ? `logs: ${u!.container}` : undefined}
      >
        <div className="arch-name">
          {u && <span className={`unit-dot ${u.status}`}>●</span>} {title}
        </div>
        <div className="arch-sub">{sub}</div>
        <div className="arch-meta">{meta}</div>
      </div>
    )
  }

  const serviceNodes = services.map((s) => {
    const u = unitFor(s.name, 'service')
    const sub = u?.image ? shortImage(u.image) : TYPE_LABEL[s.type] ?? s.type
    const meta = u?.port ? `:${u.port}` : s.port ? `:${s.port} declared` : ''
    return node(s.name, 'service', 'service', s.name, sub, meta)
  })
  const resourceNodes = resources.map((r) => {
    const u = unitFor(r.name, 'database')
    const sub = u?.image ? shortImage(u.image) : TYPE_LABEL[r.type] ?? r.type
    const meta = u?.port ? `:${u.port}` : ''
    return node(r.name, 'database', 'resource', r.name, sub, meta)
  })

  const inner = (
    <>
      <div className="arch-col">
        <div className="arch-col-label">services</div>
        {serviceNodes.length === 0 && <div className="arch-node ghost">none declared</div>}
        {serviceNodes}
      </div>
      {resourceNodes.length > 0 && (
        <>
          <div className="arch-arrow">→</div>
          <div className="arch-col">
            <div className="arch-col-label">resources</div>
            {resourceNodes}
          </div>
        </>
      )}
    </>
  )

  return (
    <div className="arch">
      {routes.length > 0 && (
        <>
          <div className="arch-col">
            <div className="arch-col-label">edge</div>
            {routes.map((r, i) => (
              <div className="arch-node" key={r.domain + r.service + i}>
                <div className="arch-name">{r.domain === 'default' ? 'default domain' : r.domain}</div>
                <div className="arch-sub">HTTPS → {r.service}</div>
              </div>
            ))}
          </div>
          <div className="arch-arrow">→</div>
        </>
      )}
      {instanceName ? (
        <div className="arch-instance">
          <div className="arch-instance-label">instance · {instanceName}</div>
          <div className="arch-instance-inner">{inner}</div>
        </div>
      ) : (
        inner
      )}
    </div>
  )
}
