import { useState } from 'react'
import { Link, useParams, useSearchParams } from 'react-router-dom'
import { api, timeAgo, usePoll, type Branch, type BranchDetail, type Project } from './api'
import ArchDiagram, { type Manifest, type Unit } from './ArchDiagram'
import ArchBuilder from './ArchBuilder'
import LogsDrawer from './LogsDrawer'

type Deployment = {
  id: string
  branch: string
  sha: string
  status: string
  actor: string
  instance: string
  url: string
  log: string
  created_at: string
}

const KIND_CLASS: Record<string, string> = {
  feature: 'open',
  release: 'merged',
  bugfix: 'agent',
  hotfix: 'agent',
  chore: 'human',
}

export default function ProjectPage() {
  const { ref } = useParams()
  const [params, setParams] = useSearchParams()
  const branch = params.get('branch') ?? 'main'

  const project = usePoll<Project>(`/projects/${ref}`)
  const branches = usePoll<{ branches: Branch[] }>(`/projects/${ref}/branches`, 5000)
  const manifest = usePoll<Manifest>(`/projects/${ref}/manifest?branch=${encodeURIComponent(branch)}`, 10000)
  const deployments = usePoll<{ deployments: Deployment[] }>(`/projects/${ref}/deployments`, 15000)
  const detail = usePoll<BranchDetail>(
    branch === 'main' ? '' : `/projects/${ref}/branch?name=${encodeURIComponent(branch)}`,
    5000,
  )
  const services = usePoll<{ units: Unit[] }>(`/projects/${ref}/services`, 8000)
  const [logsUnit, setLogsUnit] = useState<Unit | null>(null)
  const [building, setBuilding] = useState(false)

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
        {project.upstream && (
          <>
            {' · '}synced from{' '}
            <a href={`https://github.com/${project.upstream}`} target="_blank" rel="noreferrer">
              <span style={{ fontFamily: 'var(--mono)' }}>github.com/{project.upstream}</span>
            </a>{' '}
            <SyncButton projectRef={ref!} />
          </>
        )}
      </p>

      {branch !== 'main' && detail && (
        <BranchPanel projectRef={ref!} detail={detail} linked={!!project.upstream} onMerged={() => setParams({})} />
      )}

      <div className="row">
        <h2 className="section-h">
          Architecture <span className="section-note">{branch}</span>
        </h2>
        <div className="spacer" />
        {manifest && !building && (
          <button className="btn ghost" onClick={() => setBuilding(true)}>
            {manifest.adopted ? 'Edit' : 'Build goku.yaml'}
          </button>
        )}
      </div>
      {building && manifest ? (
        <ArchBuilder
          projectRef={ref!}
          branch={branch}
          manifest={manifest}
          onClose={() => setBuilding(false)}
        />
      ) : (
        <ArchDiagram manifest={manifest} units={branch === 'main' ? services?.units : undefined} onLogs={setLogsUnit} />
      )}

      <h2 className="section-h">Services</h2>
      <div className="list">
        {services?.units.length === 0 && <p className="page-sub">Nothing declared in goku.yaml yet.</p>}
        {services?.units.map((u) => (
          <div className="branch-item" key={u.kind + u.name} style={{ cursor: u.container ? 'pointer' : 'default' }}
            onClick={() => u.container && setLogsUnit(u)}>
            <span className={`unit-dot ${u.status}`}>●</span>
            <span className="branch-name">{u.name}</span>
            <span className="pill human">{u.kind === 'database' ? 'postgres' : u.type}</span>
            {u.instance && <span className="pill open">on {u.instance}</span>}
            <span className="branch-subject">{u.image}</span>
            <span className="spacer" />
            <span className="branch-when">{u.status === 'not_deployed' ? 'not deployed' : u.uptime}</span>
            {u.container && (
              <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }}
                onClick={(e) => { e.stopPropagation(); setLogsUnit(u) }}>
                logs
              </button>
            )}
          </div>
        ))}
      </div>

      {logsUnit && <LogsDrawer projectRef={ref!} unit={logsUnit} onClose={() => setLogsUnit(null)} />}

      <div className="row" style={{ marginTop: 28 }}>
        <h2 className="section-h" style={{ margin: 0 }}>
          Deployments
        </h2>
        <div className="spacer" />
        <NewEnvButton projectRef={ref!} branches={branches?.branches ?? []} />
      </div>
      {deployments?.deployments.length === 0 ? (
        <p className="page-sub">No environments yet — press + to tie a branch to a fleet instance and deploy it.</p>
      ) : (
        <Environments projectRef={ref!} deployments={deployments?.deployments ?? []} />
      )}

      <h2 className="section-h">Secrets</h2>
      <SecretsPanel projectRef={ref!} />

      <h2 className="section-h">Branches</h2>
      <div className="list">
        {branches?.branches.map((b) => (
          <button
            key={b.name}
            className={`branch-item ${b.name === branch ? 'selected' : ''}`}
            onClick={() => (b.name === 'main' ? setParams({}) : setParams({ branch: b.name }))}
          >
            <span className="branch-name">{b.name}</span>
            {b.kind && <span className={`pill ${KIND_CLASS[b.kind] ?? 'human'}`}>{b.kind}</span>}
            {b.name !== 'main' && b.merged && <span className="pill merged">merged</span>}
            <span className="branch-subject">{b.subject}</span>
            <span className="spacer" />
            <span className="branch-sha">{b.sha.slice(0, 8)}</span>
            <span className="branch-when">{timeAgo(b.committed_at)}</span>
          </button>
        ))}
      </div>
    </>
  )
}

// BranchPanel is the review surface for a selected branch: ahead/behind,
// the diff against main, and the merge action.
// NewEnvButton opens the modal that ties a branch to a free fleet instance.
function NewEnvButton({ projectRef, branches }: { projectRef: string; branches: Branch[] }) {
  const [open, setOpen] = useState(false)
  return (
    <>
      <button className="btn" style={{ padding: '4px 12px' }} onClick={() => setOpen(true)} title="New environment">
        +
      </button>
      {open && <NewEnvModal projectRef={projectRef} branches={branches} onClose={() => setOpen(false)} />}
    </>
  )
}

type FleetRow = {
  instance: { id: string; name: string; driver: string; status: string }
  assignments: string[]
}

function NewEnvModal({
  projectRef,
  branches,
  onClose,
}: {
  projectRef: string
  branches: Branch[]
  onClose: () => void
}) {
  const fleet = usePoll<{ instances: FleetRow[] }>('/instances', 30000)
  const [branchName, setBranchName] = useState(branches[0]?.name ?? 'main')
  const [instance, setInstance] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const free = (row: FleetRow) =>
    row.instance.driver === 'local' || (row.instance.status === 'ready' && row.assignments.length === 0)

  const submit = async () => {
    setBusy(true)
    setError('')
    try {
      await api(`/projects/${projectRef}/deploy`, {
        method: 'POST',
        body: JSON.stringify({ branch: branchName, instance }),
      })
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 480 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">New environment</h2>
        <p className="page-sub">Tie a branch to a fleet instance and deploy it as its own live environment.</p>
        <div className="row" style={{ marginBottom: 12 }}>
          <label className="page-sub" style={{ margin: 0, width: 70 }}>branch</label>
          <select className="input" style={{ flex: 1 }} value={branchName} onChange={(e) => setBranchName(e.target.value)}>
            {branches.map((b) => (
              <option key={b.name} value={b.name}>
                {b.name}
              </option>
            ))}
          </select>
        </div>
        <div className="row" style={{ marginBottom: 4 }}>
          <label className="page-sub" style={{ margin: 0, width: 70 }}>instance</label>
          <select className="input" style={{ flex: 1 }} value={instance} onChange={(e) => setInstance(e.target.value)}>
            <option value="">auto (local instance)</option>
            {fleet?.instances.map((row) => (
              <option key={row.instance.id} value={row.instance.name} disabled={!free(row)}>
                {row.instance.name} · {row.instance.driver}
                {row.instance.driver !== 'local' && row.assignments.length > 0 ? ' (busy)' : ''}
                {row.instance.status !== 'ready' && row.instance.status !== 'verifying' ? ` (${row.instance.status})` : ''}
              </option>
            ))}
          </select>
        </div>
        <p className="page-sub" style={{ fontSize: 12 }}>
          Branch environments get their own containers and database, live at{' '}
          <span style={{ fontFamily: 'var(--mono)' }}>&lt;branch&gt;--{projectRef}.goku.host</span>.
        </p>
        {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
        <div className="row" style={{ marginTop: 12 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" onClick={submit} disabled={busy}>
            {busy ? 'Starting…' : 'Deploy environment'}
          </button>
        </div>
      </div>
    </div>
  )
}

// Environments groups deployments by branch: each environment shows its
// current live deployment, and expands into that environment's history.
function Environments({ projectRef, deployments }: { projectRef: string; deployments: Deployment[] }) {
  const [open, setOpen] = useState<string | null>(null)

  const byBranch = new Map<string, Deployment[]>()
  for (const d of deployments) {
    const list = byBranch.get(d.branch) ?? []
    list.push(d)
    byBranch.set(d.branch, list)
  }
  const envs = [...byBranch.entries()].sort((a, b) => {
    const liveA = a[1].some((d) => d.status === 'healthy') ? 0 : 1
    const liveB = b[1].some((d) => d.status === 'healthy') ? 0 : 1
    return liveA - liveB || a[0].localeCompare(b[0])
  })

  return (
    <div className="list">
      {envs.map(([branchName, history]) => {
        const live = history.find((d) => d.status === 'healthy')
        const latest = history[0]
        const expanded = open === branchName
        return (
          <div className="cs-item" key={branchName} style={{ cursor: 'pointer' }} onClick={() => setOpen(expanded ? null : branchName)}>
            <div className="row">
              <span className={`pill ${live ? 'merged' : 'human'}`}>{live ? 'live' : 'stopped'}</span>
              <span className="branch-name" style={{ fontSize: 15 }}>{branchName}</span>
              {live && (
                <>
                  <span className="branch-sha">{live.sha.slice(0, 8)}</span>
                  {live.url && (
                    <a href={live.url} target="_blank" rel="noreferrer" style={{ color: 'var(--accent)', fontSize: 13 }}
                      onClick={(e) => e.stopPropagation()}>
                      {live.url.replace('https://', '')}
                    </a>
                  )}
                </>
              )}
              {!live && latest && <span className={`pill ${latest.status === 'failed' ? 'agent' : 'human'}`}>{latest.status}</span>}
              {live?.instance && <span className="pill open">on {live.instance}</span>}
              <div className="spacer" />
              {live && branchName !== 'main' && (
                <button
                  className="btn ghost"
                  style={{ padding: '2px 10px', fontSize: 12 }}
                  onClick={async (e) => {
                    e.stopPropagation()
                    await api(`/projects/${projectRef}/environments/stop`, {
                      method: 'POST',
                      body: JSON.stringify({ branch: branchName }),
                    }).catch(() => {})
                  }}
                >
                  stop
                </button>
              )}
              <span className="branch-when">
                {history.length} deploy{history.length === 1 ? '' : 's'} · {timeAgo(latest.created_at)}
              </span>
              <span className="branch-sha">{expanded ? '▾' : '▸'}</span>
            </div>
            {expanded && (
              <div className="list" style={{ marginTop: 12 }} onClick={(e) => e.stopPropagation()}>
                {history.map((d) => (
                  <DeploymentRow key={d.id} d={d} />
                ))}
              </div>
            )}
          </div>
        )
      })}
    </div>
  )
}

function DeploymentRow({ d }: { d: Deployment }) {
  const [showLog, setShowLog] = useState(false)
  const statusClass =
    d.status === 'healthy' ? 'merged' : d.status === 'failed' ? 'agent' : d.status === 'stopped' ? 'human' : 'open'
  return (
    <div className="cs-item" style={{ cursor: 'default' }}>
      <div className="row">
        <span className={`pill ${statusClass}`}>{d.status}</span>
        <span className="branch-name">{d.branch}</span>
        <span className="branch-sha">{d.sha.slice(0, 8)}</span>
        {d.status === 'healthy' && d.url && (
          <a href={d.url} target="_blank" rel="noreferrer" style={{ color: 'var(--accent)', fontSize: 13 }}>
            {d.url.replace('https://', '')}
          </a>
        )}
        <div className="spacer" />
        <span className="pill human">{d.actor}</span>
        <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={() => setShowLog(!showLog)}>
          {showLog ? 'hide log' : 'log'}
        </button>
        <span className="branch-when">{timeAgo(d.created_at)}</span>
      </div>
      {showLog && (
        <div className="filebox" style={{ marginTop: 10 }}>
          <div className="path">deploy log</div>
          <pre>{d.log || '…'}</pre>
        </div>
      )}
    </div>
  )
}

function SecretsPanel({ projectRef }: { projectRef: string }) {
  const data = usePoll<{ secrets: { key: string; updated_at: string }[] }>(`/projects/${projectRef}/secrets`, 15000)
  const [key, setKey] = useState('')
  const [value, setValue] = useState('')
  const [error, setError] = useState('')

  const add = async () => {
    if (!key.trim() || !value) return
    try {
      await api(`/projects/${projectRef}/secrets`, {
        method: 'PUT',
        body: JSON.stringify({ key: key.trim().toUpperCase(), value }),
      })
      setKey('')
      setValue('')
      setError('')
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const remove = async (k: string) => {
    await api(`/projects/${projectRef}/secrets/${k}`, { method: 'DELETE' }).catch(() => {})
  }

  return (
    <div className="cs-item" style={{ cursor: 'default' }}>
      <p className="page-sub" style={{ marginTop: 0 }}>
        Write-only env vars injected into deployments. Values are never shown again; changes apply on the next deploy.
      </p>
      {data?.secrets.map((s) => (
        <div className="row" key={s.key} style={{ padding: '4px 0' }}>
          <span className="branch-name">{s.key}</span>
          <span className="branch-sha">••••••••</span>
          <span className="branch-when">{timeAgo(s.updated_at)}</span>
          <div className="spacer" />
          <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={() => remove(s.key)}>
            remove
          </button>
        </div>
      ))}
      <div className="row" style={{ marginTop: 8 }}>
        <input className="input" placeholder="KEY" style={{ width: 180 }} value={key} onChange={(e) => setKey(e.target.value)} />
        <input
          className="input"
          type="password"
          placeholder="value"
          style={{ flex: 1 }}
          value={value}
          onChange={(e) => setValue(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && add()}
        />
        <button className="btn ghost" onClick={add}>
          Set
        </button>
      </div>
      {error && <p style={{ color: 'var(--amber)', marginBottom: 0 }}>{error}</p>}
    </div>
  )
}

function SyncButton({ projectRef }: { projectRef: string }) {
  const [busy, setBusy] = useState(false)
  return (
    <button
      className="btn ghost"
      style={{ padding: '2px 10px', fontSize: 12 }}
      disabled={busy}
      onClick={async () => {
        setBusy(true)
        try {
          await api(`/projects/${projectRef}/sync`, { method: 'POST', body: '{}' })
        } finally {
          setBusy(false)
        }
      }}
    >
      {busy ? 'Syncing…' : 'Sync now'}
    </button>
  )
}

function BranchPanel({
  projectRef,
  detail,
  linked,
  onMerged,
}: {
  projectRef: string
  detail: BranchDetail
  linked: boolean
  onMerged: () => void
}) {
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [showDiff, setShowDiff] = useState(true)

  const merge = async () => {
    setBusy(true)
    setError('')
    try {
      await api(`/projects/${projectRef}/merge`, {
        method: 'POST',
        body: JSON.stringify({ branch: detail.name }),
      })
      onMerged()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="branch-panel">
      <div className="row">
        <span className="branch-name" style={{ fontSize: 15 }}>
          {detail.name}
        </span>
        {detail.kind && <span className={`pill ${KIND_CLASS[detail.kind] ?? 'human'}`}>{detail.kind}</span>}
        {detail.merged ? (
          <span className="pill merged">merged</span>
        ) : (
          <span className="pill open">
            {detail.ahead} ahead{detail.behind > 0 ? ` · ${detail.behind} behind` : ''}
          </span>
        )}
        <div className="spacer" />
        <button className="btn ghost" onClick={() => setShowDiff(!showDiff)}>
          {showDiff ? 'Hide diff' : 'Show diff'}
        </button>
        {!detail.merged && linked && <span className="pill human">merge on GitHub</span>}
        {!detail.merged && !linked && (
          <button className="btn" onClick={merge} disabled={busy || detail.behind > 0}>
            {busy ? 'Merging…' : detail.behind > 0 ? 'Rebase needed' : 'Merge'}
          </button>
        )}
      </div>
      {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
      {showDiff && (
        <div style={{ marginTop: 14 }}>
          {detail.files.length === 0 && <p className="page-sub">No changes against main.</p>}
          {detail.files.map((f) => (
            <div className="filebox" key={f.path}>
              <div className="path">{f.path}</div>
              <pre>{f.content}</pre>
            </div>
          ))}
        </div>
      )}
    </div>
  )
}
