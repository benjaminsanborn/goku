export type Manifest = {
  adopted: boolean
  branch: string
  error?: string
  services?: { name: string; type: string; size?: string; port?: number; health_check?: string }[]
  resources?: { name: string; type: string }[]
  routes?: { domain: string; service: string }[]
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

const SERVICE_INFO: Record<string, { cloud: string; local: string }> = {
  api: { cloud: 'Fargate service', local: 'Dockerfile locally' },
  web: { cloud: 'CloudFront + S3', local: 'static server locally' },
}

const RESOURCE_INFO: Record<string, { cloud: string; local: string }> = {
  database: { cloud: 'Aurora PostgreSQL', local: 'postgres:18 locally' },
  storage: { cloud: 'S3 bucket', local: 'MinIO locally' },
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

  const serviceNodes = services.map((s) =>
    node(
      s.name,
      'service',
      'service',
      s.name,
      SERVICE_INFO[s.type]?.cloud ?? s.type,
      [s.size, s.port ? `:${s.port}` : '', s.health_check ? `health ${s.health_check}` : ''].filter(Boolean).join(' · '),
    ),
  )
  const resourceNodes = resources.map((r) =>
    node(r.name, 'database', 'resource', r.name, RESOURCE_INFO[r.type]?.cloud ?? r.type, RESOURCE_INFO[r.type]?.local ?? ''),
  )

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
