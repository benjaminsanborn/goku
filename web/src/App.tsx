import { useEffect, useState } from 'react'
import { NavLink, Route, Routes } from 'react-router-dom'
import { getToken, setToken } from './api'
import ProjectsPage from './ProjectsPage'
import ProjectPage from './ProjectPage'
import ChangesetPage from './ChangesetPage'
import OrgFooter from './OrgFooter'

export default function App() {
  const [authed, setAuthed] = useState(() => getToken() !== '')

  useEffect(() => {
    const onUnauthorized = () => {
      setToken('')
      setAuthed(false)
    }
    window.addEventListener('goku-unauthorized', onUnauthorized)
    return () => window.removeEventListener('goku-unauthorized', onUnauthorized)
  }, [])

  if (!authed) return <TokenGate onSave={() => setAuthed(true)} />

  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <span className="dot">◆</span> goku
        </div>
        <nav className="nav">
          <NavLink to="/" end>
            Projects
          </NavLink>
        </nav>
        <OrgFooter />
      </aside>
      <main className="main">
        <Routes>
          <Route path="/" element={<ProjectsPage />} />
          <Route path="/projects/:ref" element={<ProjectPage />} />
          <Route path="/projects/:ref/changesets/:id" element={<ChangesetPage />} />
        </Routes>
      </main>
    </div>
  )
}

function TokenGate({ onSave }: { onSave: () => void }) {
  const [value, setValue] = useState('')
  const save = () => {
    if (!value.trim()) return
    setToken(value.trim())
    onSave()
  }
  return (
    <div className="layout" style={{ alignItems: 'center', justifyContent: 'center' }}>
      <div className="card" style={{ width: 420, padding: 28 }}>
        <div className="brand" style={{ marginBottom: 12 }}>
          <span className="dot">◆</span> goku
        </div>
        <p className="page-sub">
          Enter your organization token (issued by the operator; run <code>goku login</code> to configure the CLI with it — operators: the
          root token lives in ~/.goku-token on the server).
        </p>
        <div className="row">
          <input
            className="input"
            style={{ flex: 1 }}
            type="password"
            placeholder="token"
            value={value}
            onChange={(e) => setValue(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && save()}
            autoFocus
          />
          <button className="btn" onClick={save}>
            Enter
          </button>
        </div>
      </div>
    </div>
  )
}

export function ActorPill({ actor }: { actor: string }) {
  const isAgent = actor.startsWith('agent:')
  return <span className={`pill ${isAgent ? 'agent' : 'human'}`}>{actor}</span>
}
