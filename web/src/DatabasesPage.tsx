import { useEffect, useState } from 'react'
import { Link } from 'react-router-dom'
import { api, timeAgo, usePoll } from './api'

export type Database = {
  id: string
  name: string
  engine_version: string
  instance_type: string
  storage_gb: number
  status: string
  endpoint: string
  config_overrides: Record<string, string>
  event_log: string
  created_at: string
}

export type Size = { type: string; vcpu: number; mem_mb: number; label: string; purpose: string }

type ProviderRow = { provider: { id: string; name: string; kind: string; status: string; region: string } }

export const statusClass = (status: string) =>
  status === 'available' ? 'merged' : status === 'failed' ? 'agent' : 'open'

export default function DatabasesPage() {
  const data = usePoll<{ databases: Database[]; sizes: Size[] }>('/databases', 5000)
  const [creating, setCreating] = useState(false)

  return (
    <>
      <div className="row">
        <div>
          <h1 className="page-title">Databases</h1>
          <p className="page-sub">
            Managed Postgres 18 on your own EC2 instances — a tuned server per database, with its data on a volume
            that outlives the machine.
          </p>
        </div>
        <div className="spacer" />
        <button className="btn" onClick={() => setCreating(true)}>
          Create database
        </button>
      </div>

      {creating && <CreateDatabaseModal sizes={data?.sizes ?? []} onClose={() => setCreating(false)} />}

      <div className="list" style={{ marginTop: 16 }}>
        {data?.databases.map((d) => (
          <Link key={d.id} className="cs-item" to={`/databases/${d.id}`} style={{ display: 'block' }}>
            <div className="row">
              <span className="branch-name" style={{ fontSize: 15 }}>
                {d.name}
              </span>
              <span className={`pill ${statusClass(d.status)}`}>{d.status}</span>
              <span className="pill human">postgres {d.engine_version}</span>
              <span className="branch-sha">{d.instance_type}</span>
              <span className="branch-sha">{d.storage_gb} GB</span>
              <div className="spacer" />
              <span className="branch-when">created {timeAgo(d.created_at)}</span>
            </div>
            <div className="meta" style={{ marginTop: 6, color: 'var(--text-dim)', fontSize: 12, fontFamily: 'var(--mono)' }}>
              {d.endpoint ? `${d.endpoint}:5432` : lastEvent(d.event_log) || 'provisioning…'}
            </div>
          </Link>
        ))}
        {data && data.databases.length === 0 && (
          <div className="empty">
            <p>No databases yet — create one and goku launches a tuned Postgres 18 server in your AWS account.</p>
          </div>
        )}
      </div>
    </>
  )
}

export function lastEvent(log: string): string {
  const lines = log.trim().split('\n').filter(Boolean)
  return lines[lines.length - 1] ?? ''
}

function CreateDatabaseModal({ sizes, onClose }: { sizes: Size[]; onClose: () => void }) {
  const providers = usePoll<{ providers: ProviderRow[] }>('/providers', 10000)
  const aws = (providers?.providers ?? [])
    .map((p) => p.provider)
    .filter((p) => p.kind === 'aws' && p.status === 'ready')

  const [name, setName] = useState('')
  const [providerId, setProviderId] = useState('')
  const [type, setType] = useState('')
  const [storage, setStorage] = useState(50)
  const [showConfig, setShowConfig] = useState(false)
  const [overrides, setOverrides] = useState<Record<string, string>>({})
  const [config, setConfig] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  useEffect(() => {
    if (!providerId && aws.length) setProviderId(aws[0].id)
  }, [aws, providerId])
  useEffect(() => {
    if (!type && sizes.length) setType(sizes[0].type)
  }, [sizes, type])

  // The config preview is the server's own rendering, so what's shown here is
  // the file the database will boot with.
  useEffect(() => {
    if (!type) return
    const id = setTimeout(() => {
      api<{ config: string }>('/databases/preview', {
        method: 'POST',
        body: JSON.stringify({ instance_type: type, storage_gb: storage, config_overrides: overrides }),
      })
        .then((r) => setConfig(r.config))
        .catch(() => {})
    }, 250)
    return () => clearTimeout(id)
  }, [type, storage, overrides])

  const create = async () => {
    setBusy(true)
    setError('')
    try {
      await api('/databases', {
        method: 'POST',
        body: JSON.stringify({
          name: name.trim(),
          provider_id: providerId,
          instance_type: type,
          storage_gb: storage,
          config_overrides: overrides,
        }),
      })
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 640 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">Create database</h2>
        <p className="page-sub">
          Postgres 18 on a dedicated instance. The configuration is computed from the size you pick — you can adjust
          it now or any time after.
        </p>

        {aws.length === 0 ? (
          <div className="empty">
            <p>
              No verified AWS provider — add one on the <Link to="/providers">Providers</Link> tab first.
            </p>
          </div>
        ) : (
          <>
            <div className="row" style={{ marginBottom: 10 }}>
              <input
                className="input"
                placeholder="name (e.g. orders)"
                style={{ flex: 1 }}
                value={name}
                onChange={(e) => setName(e.target.value)}
                autoFocus
              />
              <select className="input" value={providerId} onChange={(e) => setProviderId(e.target.value)}>
                {aws.map((p) => (
                  <option key={p.id} value={p.id}>
                    {p.name} · {p.region || 'us-east-1'}
                  </option>
                ))}
              </select>
            </div>

            <div className="size-grid">
              {sizes.map((s) => (
                <button
                  key={s.type}
                  className={`size-card ${type === s.type ? 'selected' : ''}`}
                  onClick={() => setType(s.type)}
                >
                  <div className="arch-name">{s.type}</div>
                  <div className="arch-sub">
                    {s.vcpu} vCPU · {s.mem_mb / 1024} GB
                  </div>
                  <div className="arch-meta">{s.purpose}</div>
                </button>
              ))}
            </div>

            <label className="field">
              <span>storage — the data volume, separate from the instance</span>
              <div className="row">
                <input
                  type="range"
                  min={20}
                  max={1000}
                  step={10}
                  value={storage}
                  style={{ flex: 1 }}
                  onChange={(e) => setStorage(Number(e.target.value))}
                />
                <span style={{ fontFamily: 'var(--mono)', width: 70, textAlign: 'right' }}>{storage} GB</span>
              </div>
            </label>

            <button className="btn ghost" style={{ marginBottom: 8 }} onClick={() => setShowConfig(!showConfig)}>
              {showConfig ? 'Hide' : 'Review'} postgresql.conf
            </button>
            {showConfig && (
              <>
                <ConfigEditor overrides={overrides} onChange={setOverrides} />
                <div className="filebox" style={{ maxHeight: 220, overflow: 'auto' }}>
                  <div className="path">postgresql.conf · computed for {type}</div>
                  <pre>{config || '…'}</pre>
                </div>
              </>
            )}

            {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
            <div className="row" style={{ marginTop: 14 }}>
              <span className="page-sub" style={{ margin: 0 }}>
                Takes a few minutes — you can watch it come up.
              </span>
              <div className="spacer" />
              <button className="btn ghost" onClick={onClose}>
                Cancel
              </button>
              <button className="btn" onClick={create} disabled={busy || !name.trim()}>
                {busy ? 'Creating…' : 'Create'}
              </button>
            </div>
          </>
        )}
      </div>
    </div>
  )
}

// ConfigEditor edits parameters, not a file: the base is recomputed from the
// instance size, and these are the values layered over it.
export function ConfigEditor({
  overrides,
  onChange,
  restartParams = [],
}: {
  overrides: Record<string, string>
  onChange: (next: Record<string, string>) => void
  restartParams?: string[]
}) {
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')

  const add = () => {
    const k = key.trim()
    if (!k) return
    onChange({ ...overrides, [k]: value.trim() })
    setKey('')
    setValue('')
  }
  const drop = (k: string) => {
    const next = { ...overrides }
    delete next[k]
    onChange(next)
  }

  return (
    <div style={{ marginBottom: 10 }}>
      {Object.entries(overrides).map(([k, v]) => (
        <div className="row" key={k} style={{ marginBottom: 4 }}>
          <span style={{ fontFamily: 'var(--mono)', fontSize: 12, flex: 1 }}>
            {k} = {v}
          </span>
          {restartParams.includes(k) && <span className="pill open">restart</span>}
          <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={() => drop(k)}>
            remove
          </button>
        </div>
      ))}
      <div className="row">
        <input
          className="input"
          placeholder="parameter (e.g. work_mem)"
          style={{ flex: 1 }}
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
        <input
          className="input"
          placeholder="value"
          style={{ width: 140 }}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && add()}
        />
        <button className="btn ghost" onClick={add}>
          Set
        </button>
      </div>
    </div>
  )
}
