import { Link, useParams, useSearchParams } from 'react-router-dom'
import { timeAgo, usePoll, type Changeset, type Project } from './api'
import { ActorPill } from './App'
import ArchDiagram, { type Manifest } from './ArchDiagram'

type Branch = { name: string; sha: string; subject: string; committed_at: string }
type Deployment = { id: string; status: string }

export default function ProjectPage() {
  const { ref } = useParams()
  const [params, setParams] = useSearchParams()
  const branch = params.get('branch') ?? 'main'

  const project = usePoll<Project>(`/projects/${ref}`)
  const branches = usePoll<{ branches: Branch[] }>(`/projects/${ref}/branches`, 5000)
  const manifest = usePoll<Manifest>(`/projects/${ref}/manifest?branch=${encodeURIComponent(branch)}`, 10000)
  const deployments = usePoll<{ deployments: Deployment[] }>(`/projects/${ref}/deployments`, 15000)
  const changesets = usePoll<{ changesets: Changeset[] }>(`/projects/${ref}/changesets`)

  if (!project) return null
  const openChangesetFor = (b: string) => changesets?.changesets.find((c) => c.branch === b && c.status === 'open')

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
        {branch !== 'main' && <span className="pill open">viewing {branch}</span>}
      </div>
      <p className="page-sub">
        {project.region} · created {timeAgo(project.created_at)}
      </p>

      <h2 className="section-h">
        Architecture <span className="section-note">{branch}</span>
      </h2>
      <ArchDiagram manifest={manifest} />

      <h2 className="section-h">
        Deployments <span className="section-note">{branch}</span>
      </h2>
      {deployments?.deployments.length === 0 ? (
        <p className="page-sub">No deployments yet — the deploy pipeline lands here when it ships.</p>
      ) : (
        <div className="list">{/* deployment rows once the pipeline exists */}</div>
      )}

      <h2 className="section-h">Branches</h2>
      <div className="list">
        {branches?.branches.map((b) => {
          const cs = openChangesetFor(b.name)
          return (
            <button
              key={b.name}
              className={`branch-item ${b.name === branch ? 'selected' : ''}`}
              onClick={() => (b.name === 'main' ? setParams({}) : setParams({ branch: b.name }))}
            >
              <span className="branch-name">{b.name}</span>
              <span className="branch-subject">{b.subject}</span>
              <span className="spacer" />
              {cs && (
                <Link
                  className="pill open"
                  to={`/projects/${ref}/changesets/${cs.id}`}
                  onClick={(e) => e.stopPropagation()}
                >
                  changeset #{cs.number}
                </Link>
              )}
              <span className="branch-sha">{b.sha.slice(0, 8)}</span>
              <span className="branch-when">{timeAgo(b.committed_at)}</span>
            </button>
          )
        })}
      </div>

      <h2 className="section-h">Changelog</h2>
      {changesets && changesets.changesets.length === 0 && (
        <p className="page-sub">
          No changesets yet — push a branch and <code>goku push</code>, or ask your Claude to propose a change.
        </p>
      )}
      <div className="list">
        {changesets?.changesets.map((cs) => (
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
