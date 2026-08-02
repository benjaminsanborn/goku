import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, timeAgo, usePoll } from './api'
import { ConfigEditor, statusClass, type Database, type Size } from './DatabasesPage'

type Detail = {
  database: Database
  size: Size
  config: string
  restart_params: string[]
}

type Credentials = { host: string; port: number; user: string; password: string; url: string }

export default function DatabasePage() {
  const { id } = useParams()
  const detail = usePoll<Detail>(`/databases/${id}`, 4000)

  if (!detail) return null
  const d = detail.database
  const settling = d.status !== 'available' && d.status !== 'failed'

  return (
    <>
      <Link className="crumb" to="/databases">
        ← databases
      </Link>
      <div className="row">
        <h1 className="page-title" style={{ fontFamily: 'var(--mono)' }}>
          {d.name}
        </h1>
        <span className={`pill ${statusClass(d.status)}`}>{d.status}</span>
        <span className="pill human">postgres {d.engine_version}</span>
        <div className="spacer" />
        <RebootButton id={d.id} disabled={settling} />
        <DeleteButton database={d} />
      </div>
      <p className="page-sub">
        {detail.size.label} · {d.storage_gb} GB data volume · created {timeAgo(d.created_at)}
      </p>

      {settling && (
        <div className="filebox">
          <div className="path">activity</div>
          <pre>{d.event_log || 'starting…'}</pre>
        </div>
      )}

      <h2 className="section-h">Connection</h2>
      <ConnectionPanel database={d} />

      <h2 className="section-h">Databases</h2>
      <LogicalDatabases id={d.id} status={d.status} />

      <h2 className="section-h">Configuration</h2>
      <ConfigPanel detail={detail} />

      <h2 className="section-h">Logs</h2>
      <LogPanel id={d.id} />

      <h2 className="section-h">Activity</h2>
      <div className="filebox">
        <div className="path">events</div>
        <pre>{d.event_log || 'nothing yet'}</pre>
      </div>

      <h2 className="section-h">Coming next</h2>
      <div className="list">
        <Planned title="Read replicas" note="Streaming replication to a second instance, with a promote action. Enabling it restarts the primary once, to raise wal_level." />
        <Planned title="Connection pooling" note="pgbouncer in front of the endpoint, so the connection count stops being a ceiling." />
        <Planned title="Backups" note="Deferred deliberately — snapshots, logical dumps and PITR are a design of their own. Until then, deleting a database destroys its data." />
        <Planned title="Cloning" note="Snapshot the data volume into a new database, for staging copies of production." />
        <Planned title="Metrics" note="The stats daemon plugs in here — instance and Postgres metrics alongside the logs." />
      </div>
    </>
  )
}

function Planned({ title, note }: { title: string; note: string }) {
  return (
    <div className="branch-item" style={{ cursor: 'default', alignItems: 'flex-start' }}>
      <span className="branch-name">{title}</span>
      <span className="pill human">planned</span>
      <span className="branch-subject" style={{ whiteSpace: 'normal' }}>
        {note}
      </span>
    </div>
  )
}

function ConnectionPanel({ database }: { database: Database }) {
  const [creds, setCreds] = useState<Credentials>()
  const [error, setError] = useState('')

  if (!database.endpoint) {
    return <p className="page-sub">The endpoint appears once the instance is up.</p>
  }

  const reveal = async () => {
    try {
      setCreds(await api<Credentials>(`/databases/${database.id}/credentials`))
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <div className="cs-item" style={{ cursor: 'default' }}>
      <Row label="host" value={database.endpoint} />
      <Row label="port" value="5432" />
      <Row label="user" value="goku" />
      <Row label="password" value={creds?.password ?? '••••••••••••'} />
      <Row label="url" value={creds?.url ?? `postgres://goku:••••@${database.endpoint}:5432/postgres`} />
      <div className="row" style={{ marginTop: 8 }}>
        <span className="page-sub" style={{ margin: 0 }}>
          Revealing the password is recorded in the audit log.
        </span>
        <div className="spacer" />
        {creds && (
          <button className="btn ghost" onClick={() => navigator.clipboard?.writeText(creds.url)}>
            Copy URL
          </button>
        )}
        <button className="btn ghost" onClick={reveal}>
          {creds ? 'Refresh' : 'Reveal password'}
        </button>
      </div>
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
    </div>
  )
}

function Row({ label, value }: { label: string; value: string }) {
  return (
    <div className="row" style={{ gap: 10 }}>
      <span style={{ width: 70, color: 'var(--text-faint)', fontSize: 12 }}>{label}</span>
      <span style={{ fontFamily: 'var(--mono)', fontSize: 12, wordBreak: 'break-all' }}>{value}</span>
    </div>
  )
}

function LogicalDatabases({ id, status }: { id: string; status: string }) {
  const list = usePoll<{ databases: { name: string; size: string }[] }>(`/databases/${id}/databases`, 10000)
  const [name, setName] = useState('')
  const [created, setCreated] = useState<{ name: string; url: string }>()
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const add = async () => {
    setBusy(true)
    setError('')
    try {
      setCreated(await api(`/databases/${id}/databases`, { method: 'POST', body: JSON.stringify({ name: name.trim() }) }))
      setName('')
    } catch (e) {
      setError((e as Error).message)
    }
    setBusy(false)
  }

  return (
    <>
      <div className="list">
        {list?.databases.map((db) => (
          <div className="branch-item" key={db.name} style={{ cursor: 'default' }}>
            <span className="branch-name">{db.name}</span>
            <span className="spacer" />
            <span className="branch-when">{db.size}</span>
          </div>
        ))}
        {list?.databases.length === 0 && <p className="page-sub">Only the default postgres database so far.</p>}
      </div>
      <div className="row" style={{ marginTop: 10 }}>
        <input
          className="input"
          placeholder="new database (creates an owner role too)"
          style={{ flex: 1 }}
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && add()}
          disabled={status !== 'available'}
        />
        <button className="btn" onClick={add} disabled={busy || status !== 'available' || !name.trim()}>
          Add database
        </button>
      </div>
      {created && (
        <div className="filebox" style={{ marginTop: 10 }}>
          <div className="path">{created.name} — copy this now, the password isn't shown again</div>
          <pre>{created.url}</pre>
        </div>
      )}
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
    </>
  )
}

function ConfigPanel({ detail }: { detail: Detail }) {
  const d = detail.database
  const [overrides, setOverrides] = useState<Record<string, string>>(d.config_overrides ?? {})
  const [preview, setPreview] = useState('')
  const [restart, setRestart] = useState(false)
  const [error, setError] = useState('')
  const [note, setNote] = useState('')
  const dirty = JSON.stringify(overrides) !== JSON.stringify(d.config_overrides ?? {})

  const check = async (next: Record<string, string>) => {
    setOverrides(next)
    try {
      const r = await api<{ config: string; restart: boolean }>(`/databases/${d.id}/config`, {
        method: 'PUT',
        body: JSON.stringify({ config_overrides: next, dry_run: true }),
      })
      setPreview(r.config)
      setRestart(r.restart)
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }

  const apply = async () => {
    setError('')
    setNote('')
    try {
      await api(`/databases/${d.id}/config`, {
        method: 'PUT',
        body: JSON.stringify({ config_overrides: overrides }),
      })
      setNote(restart ? 'Restarting to apply — connections will drop briefly.' : 'Reloading configuration.')
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <>
      <p className="page-sub">
        Values are computed from {d.instance_type} and recomputed if the size changes. Overrides below are layered on
        top, so tuning survives a resize.
      </p>
      <ConfigEditor overrides={overrides} onChange={check} restartParams={detail.restart_params} />
      {dirty && (
        <div className="row" style={{ marginBottom: 10 }}>
          {restart ? (
            <span className="pill open">restart required — connections drop</span>
          ) : (
            <span className="pill merged">reload only — no downtime</span>
          )}
          <div className="spacer" />
          <button className="btn ghost" onClick={() => check(d.config_overrides ?? {})}>
            Revert
          </button>
          <button className="btn" onClick={apply} disabled={d.status !== 'available'}>
            Apply
          </button>
        </div>
      )}
      {note && <p style={{ color: 'var(--green)' }}>{note}</p>}
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
      <div className="filebox" style={{ maxHeight: 300, overflow: 'auto' }}>
        <div className="path">postgresql.conf{dirty ? ' · pending' : ''}</div>
        <pre>{preview || detail.config}</pre>
      </div>
      <p className="page-sub">
        If Postgres doesn't come back after a change, goku restores the previous file automatically and reports why.
      </p>
    </>
  )
}

function LogPanel({ id }: { id: string }) {
  const [live, setLive] = useState(false)
  const data = usePoll<{ log: string }>(live ? `/databases/${id}/logs` : '', 5000)
  const [once, setOnce] = useState<string>()

  const load = async () => setOnce((await api<{ log: string }>(`/databases/${id}/logs`)).log)

  return (
    <>
      <div className="row" style={{ marginBottom: 8 }}>
        <div className="spacer" />
        <button className="btn ghost" onClick={load}>
          Refresh
        </button>
        <button className={`btn ${live ? '' : 'ghost'}`} onClick={() => setLive(!live)}>
          {live ? 'Following' : 'Follow'}
        </button>
      </div>
      <div className="filebox" style={{ maxHeight: 320, overflow: 'auto' }}>
        <div className="path">postgres</div>
        <pre>{data?.log ?? once ?? 'press refresh to read the server log'}</pre>
      </div>
    </>
  )
}

function RebootButton({ id, disabled }: { id: string; disabled: boolean }) {
  return (
    <button
      className="btn ghost"
      disabled={disabled}
      onClick={() => {
        if (confirm('Restart Postgres? Open connections will drop.')) {
          api(`/databases/${id}/reboot`, { method: 'POST', body: '{}' })
        }
      }}
    >
      Restart
    </button>
  )
}

// Deleting destroys the data volume, and backups don't exist yet — so the
// gate is typing the name, and the copy says exactly what is lost.
function DeleteButton({ database }: { database: Database }) {
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState('')
  const [error, setError] = useState('')

  const destroy = async () => {
    try {
      await api(`/databases/${database.id}?confirm=${encodeURIComponent(typed)}`, { method: 'DELETE' })
      window.location.href = '/databases'
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <>
      <button className="btn ghost" onClick={() => setOpen(true)}>
        Delete
      </button>
      {open && (
        <div className="overlay" onClick={() => setOpen(false)}>
          <div className="modal" style={{ width: 480 }} onClick={(e) => e.stopPropagation()}>
            <h2 className="page-title">Delete {database.name}</h2>
            <p className="page-sub">
              This terminates the instance and destroys its data volume. There are no backups yet — the data is gone.
              Type <b style={{ fontFamily: 'var(--mono)' }}>{database.name}</b> to confirm.
            </p>
            <input
              className="input"
              style={{ width: '100%' }}
              value={typed}
              onChange={(e) => setTyped(e.target.value)}
              autoFocus
            />
            {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
            <div className="row" style={{ marginTop: 14 }}>
              <div className="spacer" />
              <button className="btn ghost" onClick={() => setOpen(false)}>
                Cancel
              </button>
              <button className="btn" onClick={destroy} disabled={typed !== database.name}>
                Delete permanently
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}
