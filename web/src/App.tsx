import { useCallback, useEffect, useState } from 'react'
import { NavLink, Route, Routes } from 'react-router-dom'
import { api, setToken } from './api'
import ProjectsPage from './ProjectsPage'
import FleetPage from './FleetPage'
import ProvidersPage from './ProvidersPage'
import ProjectPage from './ProjectPage'
import OrgFooter from './OrgFooter'

export type Me = {
  user?: { email: string; name: string; avatar_url: string }
  organization: { id: string; name: string } | null
}

export default function App() {
  const [state, setState] = useState<'loading' | 'unauth' | 'noorg' | 'ready'>('loading')
  const [me, setMe] = useState<Me>()

  const load = useCallback(() => {
    api<Me>('/me')
      .then((m) => {
        setMe(m)
        setState(m.organization ? 'ready' : 'noorg')
      })
      .catch(() => setState('unauth'))
  }, [])

  useEffect(() => {
    load()
    const onUnauthorized = () => {
      setToken('')
      setState('unauth')
    }
    window.addEventListener('goku-unauthorized', onUnauthorized)
    return () => window.removeEventListener('goku-unauthorized', onUnauthorized)
  }, [load])

  if (state === 'loading') return null
  if (state === 'unauth') return <LoginGate />
  if (state === 'noorg') return <JoinGate me={me} />

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
          <NavLink to="/fleet">
            Fleet
          </NavLink>
          <NavLink to="/providers">
            Providers
          </NavLink>
        </nav>
        <OrgFooter />
      </aside>
      <main className="main">
        <Routes>
          <Route path="/" element={<ProjectsPage />} />
          <Route path="/fleet" element={<FleetPage />} />
          <Route path="/providers" element={<ProvidersPage />} />
          <Route path="/projects/:ref" element={<ProjectPage />} />
        </Routes>
      </main>
    </div>
  )
}

function LoginGate() {
  const [providers, setProviders] = useState<{ github: boolean; google: boolean }>()
  const [token, setTokenInput] = useState('')

  useEffect(() => {
    fetch('/auth/providers')
      .then((r) => r.json())
      .then(setProviders)
      .catch(() => setProviders({ github: false, google: false }))
  }, [])

  const useToken = () => {
    if (!token.trim()) return
    setToken(token.trim())
    window.location.reload()
  }

  const anySSO = providers?.github || providers?.google
  return (
    <div className="layout" style={{ alignItems: 'center', justifyContent: 'center' }}>
      <div className="card" style={{ width: 400, padding: 28 }}>
        <div className="brand" style={{ marginBottom: 6 }}>
          <span className="dot">◆</span> goku
        </div>
        <p className="page-sub">Compliant CI/CD for AI agents.</p>
        {providers?.github && (
          <a className="sso-btn" href="/auth/github">
            Continue with GitHub
          </a>
        )}
        {providers?.google && (
          <a className="sso-btn" href="/auth/google">
            Continue with Google
          </a>
        )}
        {anySSO && <div className="divider">or</div>}
        <div className="row">
          <input
            className="input"
            style={{ flex: 1 }}
            type="password"
            placeholder="organization token"
            value={token}
            onChange={(e) => setTokenInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && useToken()}
          />
          <button className="btn" onClick={useToken}>
            Enter
          </button>
        </div>
      </div>
    </div>
  )
}

function JoinGate({ me }: { me?: Me }) {
  const [token, setTokenInput] = useState('')
  const [error, setError] = useState('')

  const join = async () => {
    try {
      await api('/orgs/join', { method: 'POST', body: JSON.stringify({ token: token.trim() }) })
      window.location.reload()
    } catch (e) {
      setError((e as Error).message)
    }
  }
  const signOut = async () => {
    await fetch('/v1/logout', { method: 'POST' }).catch(() => {})
    window.location.reload()
  }

  return (
    <div className="layout" style={{ alignItems: 'center', justifyContent: 'center' }}>
      <div className="card" style={{ width: 440, padding: 28 }}>
        <div className="brand" style={{ marginBottom: 12 }}>
          <span className="dot">◆</span> goku
        </div>
        <p className="page-sub">
          Signed in as <b>{me?.user?.email}</b> — but you're not in an organization yet. Paste the organization
          token your operator gave you to join.
        </p>
        <div className="row">
          <input
            className="input"
            style={{ flex: 1 }}
            type="password"
            placeholder="organization token"
            value={token}
            onChange={(e) => setTokenInput(e.target.value)}
            onKeyDown={(e) => e.key === 'Enter' && join()}
            autoFocus
          />
          <button className="btn" onClick={join}>
            Join
          </button>
        </div>
        {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
        <div className="row" style={{ marginTop: 16 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={signOut}>
            Sign out
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
