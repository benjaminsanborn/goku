import { Link, useParams } from 'react-router-dom'
import { timeAgo, usePoll, type Changeset, type Project } from './api'
import { ActorPill } from './App'

export default function ProjectPage() {
  const { ref } = useParams()
  const project = usePoll<Project>(`/projects/${ref}`)
  const data = usePoll<{ changesets: Changeset[] }>(`/projects/${ref}/changesets`)

  if (!project) return null
  return (
    <>
      <Link className="crumb" to="/">
        ← projects
      </Link>
      <div className="row">
        <h1 className="page-title" style={{ fontFamily: 'var(--mono)' }}>
          {project.name}
        </h1>
        <span className={`pill ${project.status}`}>{project.status.replaceAll('_', ' ')}</span>
      </div>
      <p className="page-sub">
        {project.region} · created {timeAgo(project.created_at)}
      </p>

      <h2 className="section-h">Changelog</h2>
      {data && data.changesets.length === 0 && (
        <div className="empty">
          <p>No changesets yet.</p>
          <p>
            Ask your Claude to propose one: <code>propose a hello world app for {project.name}</code>
          </p>
        </div>
      )}
      <div className="list">
        {data?.changesets.map((cs) => (
          <Link className="cs-item" key={cs.id} to={`/projects/${ref}/changesets/${cs.id}`}>
            <div className="row">
              <span className="title">
                #{cs.number} {cs.title}
              </span>
              <span className={`pill ${cs.status}`}>{cs.status}</span>
              <div className="spacer" />
              <ActorPill actor={cs.opened_by} />
              <span className="meta" style={{ color: 'var(--text-faint)', fontSize: 12 }}>
                {timeAgo(cs.created_at)}
              </span>
            </div>
            <div className="branch">
              {cs.branch} · {cs.files.length} file{cs.files.length === 1 ? '' : 's'}
            </div>
          </Link>
        ))}
      </div>
    </>
  )
}
