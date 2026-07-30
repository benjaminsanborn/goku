import { useState } from 'react'
import { Link } from 'react-router-dom'
import { api, timeAgo, usePoll, type AuditEvent, type Project } from './api'
import { ActorPill } from './App'

export default function ProjectsPage() {
  const data = usePoll<{ projects: Project[] }>('/projects')
  const events = usePoll<{ events: AuditEvent[] }>('/events', 5000)
  const [name, setName] = useState('')
  const [error, setError] = useState('')

  const isImport = /github\.com\//.test(name) || /^[\w.-]+\/[\w.-]+$/.test(name.trim())

  const create = async () => {
    if (!name.trim()) return
    try {
      if (isImport) {
        await api('/projects/import', { method: 'POST', body: JSON.stringify({ url: name.trim() }) })
      } else {
        await api('/projects', { method: 'POST', body: JSON.stringify({ name }) })
      }
      setName('')
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }

  return (
    <>
      <div className="row">
        <div>
          <h1 className="page-title">Projects</h1>
          <p className="page-sub">Isolated deployment targets with curated AWS resources.</p>
        </div>
        <div className="spacer" />
        <input
          className="input"
          style={{ width: 260 }}
          placeholder="new-project or github.com/owner/repo"
          value={name}
          onChange={(e) => setName(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && create()}
        />
        <button className="btn" onClick={create}>
          {isImport ? 'Import' : 'Create'}
        </button>
      </div>
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}

      {data && data.projects.length === 0 ? (
        <div className="empty">
          <p>No projects yet.</p>
          <p>
            Create one here, or ask your Claude: <code>create a project called hello-world</code>
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
                {p.region} · {p.changeset_count} changeset{p.changeset_count === 1 ? '' : 's'} · created{' '}
                {timeAgo(p.created_at)}
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
