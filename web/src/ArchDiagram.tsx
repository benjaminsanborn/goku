export type Manifest = {
  adopted: boolean
  branch: string
  error?: string
  services?: { name: string; type: string; size?: string; port?: number; health_check?: string }[]
  resources?: { name: string; type: string }[]
  routes?: { domain: string; service: string }[]
}

const SERVICE_INFO: Record<string, { cloud: string; local: string }> = {
  api: { cloud: 'Fargate service', local: 'Dockerfile locally' },
  web: { cloud: 'CloudFront + S3', local: 'static server locally' },
}

const RESOURCE_INFO: Record<string, { cloud: string; local: string }> = {
  database: { cloud: 'Aurora PostgreSQL', local: 'postgres:16 locally' },
  storage: { cloud: 'S3 bucket', local: 'MinIO locally' },
}

// ArchDiagram renders the architecture a branch's goku.yaml declares:
// edge (routes) → services → resources.
export default function ArchDiagram({ manifest }: { manifest?: Manifest }) {
  if (!manifest) return null
  if (!manifest.adopted) {
    return (
      <div className="empty">
        <p>
          No <code>goku.yaml</code> on <b>{manifest.branch}</b> — this branch hasn't adopted goku yet.
        </p>
        <p>
          Ask your Claude to adopt it, or add <code>goku.yaml</code> in a changeset.
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
          <code>goku.yaml</code> declares nothing yet — add services and resources via changesets or{' '}
          <code>goku add</code>.
        </p>
      </div>
    )
  }

  return (
    <div className="arch">
      {routes.length > 0 && (
        <>
          <div className="arch-col">
            <div className="arch-col-label">edge</div>
            {routes.map((r) => (
              <div className="arch-node" key={r.domain + r.service}>
                <div className="arch-name">{r.domain === 'default' ? 'default domain' : r.domain}</div>
                <div className="arch-sub">HTTPS · ALB → {r.service}</div>
              </div>
            ))}
          </div>
          <div className="arch-arrow">→</div>
        </>
      )}
      <div className="arch-col">
        <div className="arch-col-label">services</div>
        {services.length === 0 && <div className="arch-node ghost">none declared</div>}
        {services.map((s) => (
          <div className="arch-node service" key={s.name}>
            <div className="arch-name">{s.name}</div>
            <div className="arch-sub">{SERVICE_INFO[s.type]?.cloud ?? s.type}</div>
            <div className="arch-meta">
              {[s.size, s.port ? `:${s.port}` : '', s.health_check ? `health ${s.health_check}` : '']
                .filter(Boolean)
                .join(' · ')}
            </div>
          </div>
        ))}
      </div>
      {resources.length > 0 && (
        <>
          <div className="arch-arrow">→</div>
          <div className="arch-col">
            <div className="arch-col-label">resources</div>
            {resources.map((r) => (
              <div className="arch-node resource" key={r.name}>
                <div className="arch-name">{r.name}</div>
                <div className="arch-sub">{RESOURCE_INFO[r.type]?.cloud ?? r.type}</div>
                <div className="arch-meta">{RESOURCE_INFO[r.type]?.local}</div>
              </div>
            ))}
          </div>
        </>
      )}
    </div>
  )
}
