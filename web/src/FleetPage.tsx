import { useState } from 'react'
import { api, timeAgo, usePoll } from './api'

type Instance = {
  id: string
  name: string
  driver: string
  address: string
  status: string
  facts: Record<string, string>
  check_log: string
  created_at: string
  last_checked_at: string | null
}

type Row = { instance: Instance; assignments: string[] }

export default function FleetPage() {
  const data = usePoll<{ instances: Row[] }>('/instances', 5000)
  const [modal, setModal] = useState(false)

  return (
    <>
      <div className="row">
        <div>
          <h1 className="page-title">Fleet</h1>
          <p className="page-sub">Machines that run your deployments. An instance runs one branch of one project.</p>
        </div>
        <div className="spacer" />
        <button className="btn" onClick={() => setModal(true)}>
          Add instance
        </button>
      </div>

      {modal && <AddInstanceModal onClose={() => setModal(false)} />}

      <div className="list" style={{ marginTop: 16 }}>
        {data?.instances.map(({ instance: i, assignments }) => (
          <InstanceCard key={i.id} i={i} assignments={assignments} />
        ))}
        {data && data.instances.length === 0 && (
          <div className="empty">
            <p>No instances yet — add a machine goku can deploy to (it needs SSH access and docker).</p>
          </div>
        )}
      </div>
    </>
  )
}

function InstanceCard({ i, assignments }: { i: Instance; assignments: string[] }) {
  const [showLog, setShowLog] = useState(false)
  const statusClass =
    i.status === 'ready' ? 'merged' : i.status === 'verifying' ? 'open' : 'agent'

  return (
    <div className="cs-item" style={{ cursor: 'default' }}>
      <div className="row">
        <span className="branch-name" style={{ fontSize: 15 }}>
          {i.name}
        </span>
        <span className={`pill ${statusClass}`}>{i.status}</span>
        <span className="pill human">{i.driver}</span>
        {i.address && <span className="branch-sha">{i.address}</span>}
        <div className="spacer" />
        <button
          className="btn ghost"
          style={{ padding: '2px 10px', fontSize: 12 }}
          onClick={() => api(`/instances/${i.id}/verify`, { method: 'POST', body: '{}' })}
        >
          re-check
        </button>
        <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={() => setShowLog(!showLog)}>
          {showLog ? 'hide checks' : 'checks'}
        </button>
        {i.driver !== 'local' && (
          <button
            className="btn ghost"
            style={{ padding: '2px 10px', fontSize: 12 }}
            onClick={() => api(`/instances/${i.id}`, { method: 'DELETE' })}
          >
            remove
          </button>
        )}
      </div>
      <div className="meta" style={{ marginTop: 6, color: 'var(--text-dim)', fontSize: 12, fontFamily: 'var(--mono)' }}>
        {[i.facts.os, i.facts.arch, i.facts.docker && `docker ${i.facts.docker}`, i.facts.resources]
          .filter(Boolean)
          .join(' · ') || 'no facts yet'}
        {i.last_checked_at && ` · checked ${timeAgo(i.last_checked_at)}`}
      </div>
      <div className="row" style={{ marginTop: 8, flexWrap: 'wrap', gap: 6 }}>
        {assignments.length === 0 && <span className="pill human">idle</span>}
        {assignments.map((a) => (
          <span key={a} className="pill open" style={{ fontFamily: 'var(--mono)' }}>
            {a}
          </span>
        ))}
      </div>
      {showLog && (
        <div className="filebox" style={{ marginTop: 10 }}>
          <div className="path">verification</div>
          <pre>{i.check_log || 'not checked yet'}</pre>
        </div>
      )}
    </div>
  )
}

function AddInstanceModal({ onClose }: { onClose: () => void }) {
  const [name, setName] = useState('')
  const [address, setAddress] = useState('')
  const [key, setKey] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const add = async () => {
    setBusy(true)
    setError('')
    try {
      await api('/instances', {
        method: 'POST',
        body: JSON.stringify({ name: name.trim(), address: address.trim(), ssh_key: key }),
      })
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 520 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">Add instance</h2>
        <p className="page-sub">
          A machine goku can reach over SSH. Verification checks connectivity, docker, and capacity; the key is
          stored write-only.
        </p>
        <div className="row" style={{ marginBottom: 10 }}>
          <input className="input" placeholder="name (e.g. ec2-dev-1)" style={{ width: 180 }} value={name} onChange={(e) => setName(e.target.value)} />
          <input
            className="input"
            placeholder="ubuntu@203.0.113.7 or user@host:2222"
            style={{ flex: 1 }}
            value={address}
            onChange={(e) => setAddress(e.target.value)}
          />
        </div>
        <textarea
          className="input"
          placeholder="-----BEGIN ... PRIVATE KEY----- (paste .pem contents)"
          style={{ width: '100%', height: 140, fontFamily: 'var(--mono)', fontSize: 11, resize: 'vertical' }}
          value={key}
          onChange={(e) => setKey(e.target.value)}
        />
        {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
        <div className="row" style={{ marginTop: 14 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" onClick={add} disabled={busy}>
            {busy ? 'Adding…' : 'Add & verify'}
          </button>
        </div>
      </div>
    </div>
  )
}
