import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { api, timeAgo, usePoll, type Changeset } from './api'
import { ActorPill } from './App'

export default function ChangesetPage() {
  const { ref, id } = useParams()
  const cs = usePoll<Changeset>(`/changesets/${id}`, 5000)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')

  if (!cs) return null

  const merge = async () => {
    setBusy(true)
    setError('')
    try {
      await api(`/changesets/${id}/merge`, { method: 'POST' })
    } catch (e) {
      setError((e as Error).message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <>
      <Link className="crumb" to={`/projects/${ref}`}>
        ← {ref}
      </Link>
      <div className="row">
        <h1 className="page-title">
          #{cs.number} {cs.title}
        </h1>
        <span className={`pill ${cs.status}`}>{cs.status}</span>
        <div className="spacer" />
        {cs.status === 'open' && (
          <button className="btn" onClick={merge} disabled={busy}>
            {busy ? 'Merging…' : 'Merge'}
          </button>
        )}
      </div>
      <p className="page-sub">
        <ActorPill actor={cs.opened_by} /> opened {timeAgo(cs.created_at)} on{' '}
        <span style={{ fontFamily: 'var(--mono)' }}>
          {cs.branch}
          {cs.head_sha && ` @ ${cs.head_sha.slice(0, 8)}`}
        </span>
      </p>
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}

      {cs.description && <p className="desc">{cs.description}</p>}

      <h2 className="section-h">Files ({cs.files.length})</h2>
      {cs.files.map((f) => (
        <div className="filebox" key={f.path}>
          <div className="path">{f.path}</div>
          <pre>{f.content}</pre>
        </div>
      ))}
    </>
  )
}
