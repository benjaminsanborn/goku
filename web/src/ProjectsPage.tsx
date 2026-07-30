import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api, timeAgo, usePoll, type AuditEvent, type Project } from './api'
import { ActorPill } from './App'

export default function ProjectsPage() {
  const data = usePoll<{ projects: Project[] }>('/projects')
  const events = usePoll<{ events: AuditEvent[] }>('/events', 5000)
  const [modal, setModal] = useState<'create' | 'import' | null>(null)

  return (
    <>
      <div className="row">
        <div>
          <h1 className="page-title">Projects</h1>
          <p className="page-sub">Isolated deployment targets with curated AWS resources.</p>
        </div>
        <div className="spacer" />
        <button className="btn ghost" onClick={() => setModal('import')}>
          Import
        </button>
        <button className="btn" onClick={() => setModal('create')}>
          Create
        </button>
      </div>

      {modal && <ProjectModal kind={modal} onClose={() => setModal(null)} />}

      {data && data.projects.length === 0 ? (
        <div className="empty">
          <p>No projects yet.</p>
          <p>
            Create one, import from GitHub, or ask your Claude: <code>set up a goku project called hello-world</code>
          </p>
        </div>
      ) : (
        <div className="grid">
          {data?.projects.map((p) => (
            <Link className="card" key={p.id} to={`/projects/${p.name}`}>
              <h3>{p.name}</h3>
              <div className="row" style={{ marginBottom: 8 }}>
                <span className={`pill ${p.status}`}>{p.status.replaceAll('_', ' ')}</span>
              </div>
              <div className="meta">
                {p.region} · created {timeAgo(p.created_at)}
              </div>
            </Link>
          ))}
        </div>
      )}

      <h2 className="section-h">Activity</h2>
      <div className="feed">
        {events?.events.length === 0 && <p className="page-sub">Nothing yet — activity from agents and humans lands here.</p>}
        {events?.events.map((e) => (
          <div className="feed-item" key={e.seq}>
            <ActorPill actor={e.actor} />
            <span className="action">{e.action}</span>
            <span className="subject">{e.subject}</span>
            <span className="when">{timeAgo(e.at)}</span>
          </div>
        ))}
      </div>
    </>
  )
}

function ProjectModal({ kind, onClose }: { kind: 'create' | 'import'; onClose: () => void }) {
  const [value, setValue] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const isImport = kind === 'import'

  const submit = async () => {
    if (!value.trim() || busy) return
    setBusy(true)
    setError('')
    try {
      if (isImport) {
        await api('/projects/import', { method: 'POST', body: JSON.stringify({ url: value.trim() }) })
      } else {
        await api('/projects', { method: 'POST', body: JSON.stringify({ name: value.trim() }) })
      }
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 460 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">{isImport ? 'Import from GitHub' : 'New project'}</h2>
        <p className="page-sub">
          {isImport
            ? 'Full history, branches, and tags are preserved; main becomes the protected default branch.'
            : 'An isolated deployment target with its own git repository and protected main.'}
        </p>
        <input
          className="input"
          style={{ width: '100%' }}
          placeholder={isImport ? 'github.com/owner/repo' : 'my-project'}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && submit()}
          autoFocus
        />
        {error && <p style={{ color: 'var(--amber)', marginBottom: 0 }}>{error}</p>}
        <div className="row" style={{ marginTop: 16 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" onClick={submit} disabled={busy}>
            {busy ? (isImport ? 'Importing…' : 'Creating…') : isImport ? 'Import' : 'Create'}
          </button>
        </div>
      </div>
    </div>
  )
}
