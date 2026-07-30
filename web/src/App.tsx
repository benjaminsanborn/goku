import { NavLink, Route, Routes } from 'react-router-dom'
import ProjectsPage from './ProjectsPage'
import ProjectPage from './ProjectPage'
import ChangesetPage from './ChangesetPage'

export default function App() {
  return (
    <div className="layout">
      <aside className="sidebar">
        <div className="brand">
          <span className="dot">◆</span> platform
        </div>
        <nav className="nav">
          <NavLink to="/" end>
            Projects
          </NavLink>
        </nav>
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

export function ActorPill({ actor }: { actor: string }) {
  const isAgent = actor.startsWith('agent:')
  return <span className={`pill ${isAgent ? 'agent' : 'human'}`}>{actor}</span>
}
