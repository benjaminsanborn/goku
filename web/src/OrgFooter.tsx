import { useState } from 'react'
import { getToken, setToken, usePoll } from './api'

type Me = { organization: { id: string; name: string } }

export default function OrgFooter() {
  const me = usePoll<Me>('/me', 60000)
  const [open, setOpen] = useState(() => window.location.hash === '#org')

  if (!me) return null
  const org = me.organization

  return (
    <>
      <button className="org-footer" onClick={() => setOpen(true)} title="Organization details">
        <span className="org-dot">●</span>
        <span className="org-name">{org.name}</span>
      </button>

      {open && (
        <div className="overlay" onClick={() => setOpen(false)}>
          <div className="modal" onClick={(e) => e.stopPropagation()}>
            <div className="row">
              <h2 className="page-title" style={{ fontFamily: 'var(--mono)' }}>
                {org.name}
              </h2>
              <div className="spacer" />
              <button className="btn ghost" onClick={() => setOpen(false)}>
                ✕
              </button>
            </div>
            <p className="meta-line">
              org id <code>{org.id}</code>
            </p>
            <p className="meta-line">
              control plane <code>{window.location.origin}</code>
            </p>

            <h3 className="section-h">Share access</h3>
            <p className="page-sub" style={{ marginBottom: 12 }}>
              Anyone with your organization token gets full access to every project in <b>{org.name}</b> — its repos,
              changesets, and audit log. Share it only with people (and agents) you trust to act as your org.
            </p>
            <Snippet label="1. Install the CLI" text="brew install benjaminsanborn/goku/goku" />
            <Snippet label="2. Log in (registers Claude automatically)" text={`goku login --token ${getToken()}`} mask />
            <p className="page-sub" style={{ marginTop: 12 }}>
              That's it — their Claude gets the goku tools via <code>goku mcp</code>; the token stays in the CLI's
              config.
            </p>

            <div className="row" style={{ marginTop: 20 }}>
              <div className="spacer" />
              <button
                className="btn ghost"
                onClick={() => {
                  setToken('')
                  window.location.reload()
                }}
              >
                Sign out
              </button>
            </div>
          </div>
        </div>
      )}
    </>
  )
}

function Snippet({ label, text, mask }: { label: string; text: string; mask?: boolean }) {
  const [copied, setCopied] = useState(false)
  const shown = mask ? text.replace(/gk_[0-9a-f]+/, (t) => t.slice(0, 7) + '…' + t.slice(-4)) : text
  return (
    <div className="snippet">
      <div className="snippet-label">{label}</div>
      <div className="row">
        <code className="snippet-code">{shown}</code>
        <button
          className="btn ghost"
          onClick={() => {
            navigator.clipboard.writeText(text)
            setCopied(true)
            setTimeout(() => setCopied(false), 1500)
          }}
        >
          {copied ? 'copied' : 'copy'}
        </button>
      </div>
    </div>
  )
}
