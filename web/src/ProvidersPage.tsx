import { useState } from 'react'
import { api, timeAgo, usePoll } from './api'

type Provider = {
  id: string
  name: string
  kind: string
  region: string
  status: string
  account: string
  check_log: string
  created_at: string
  last_checked_at: string | null
}

type Row = { provider: Provider; deployable: boolean; role: string }

type Field = { key: string; label: string; optional?: boolean; secret?: boolean; hint?: string }

type Kind = {
  id: string
  label: string
  role: 'compute' | 'network'
  regionHint?: string
  sizeHint?: string
  blurb: string
  fields: Field[]
}

const KINDS: Kind[] = [
  {
    id: 'aws',
    label: 'AWS',
    role: 'compute',
    regionHint: 'us-east-1',
    sizeHint: 't3.small',
    blurb: 'Provisions EC2 instances that join your fleet and run deployments.',
    fields: [
      { key: 'access_key_id', label: 'Access key ID', secret: true },
      { key: 'secret_access_key', label: 'Secret access key', secret: true },
      { key: 'session_token', label: 'Session token', optional: true, secret: true },
    ],
  },
  {
    id: 'azure',
    label: 'Azure',
    role: 'compute',
    regionHint: 'eastus',
    blurb: 'Credentials are stored and verified; provisioning is not implemented yet.',
    fields: [
      { key: 'tenant_id', label: 'Tenant ID', secret: true },
      { key: 'client_id', label: 'Client ID', secret: true },
      { key: 'client_secret', label: 'Client secret', secret: true },
      { key: 'subscription_id', label: 'Subscription ID', optional: true, secret: true },
    ],
  },
  {
    id: 'digitalocean',
    label: 'DigitalOcean',
    role: 'compute',
    regionHint: 'nyc3',
    blurb: 'Credentials are stored and verified; provisioning is not implemented yet.',
    fields: [{ key: 'api_token', label: 'API token', secret: true }],
  },
  {
    id: 'tailscale',
    label: 'Tailscale',
    role: 'network',
    blurb:
      'The network your fleet is reached over. With a tailnet connected, provisioned instances join it at boot and take no public ingress at all — goku mints a short-lived, tagged auth key per instance.',
    fields: [
      { key: 'client_id', label: 'OAuth client ID', secret: true },
      { key: 'client_secret', label: 'OAuth client secret', secret: true },
      { key: 'tailnet', label: 'Tailnet', optional: true, hint: 'tailnet (default: the client\u2019s own)' },
      { key: 'tag', label: 'Tag', optional: true, hint: 'ACL tag (default tag:goku-fleet)' },
    ],
  },
]

const kindLabel = (id: string) => KINDS.find((k) => k.id === id)?.label ?? id

export default function ProvidersPage() {
  const data = usePoll<{ providers: Row[] }>('/providers', 5000)
  const [modal, setModal] = useState(false)

  return (
    <>
      <div className="row">
        <div>
          <h1 className="page-title">Providers</h1>
          <p className="page-sub">
            Cloud accounts goku can deploy into, plus the network it reaches them over. Credentials are stored
            write-only and verified against the provider's API.
          </p>
        </div>
        <div className="spacer" />
        <button className="btn" onClick={() => setModal(true)}>
          Add provider
        </button>
      </div>

      {modal && <AddProviderModal onClose={() => setModal(false)} />}

      <div className="list" style={{ marginTop: 16 }}>
        {data?.providers.map((row) => (
          <ProviderCard key={row.provider.id} p={row.provider} deployable={row.deployable} role={row.role} />
        ))}
        {data && data.providers.length === 0 && (
          <div className="empty">
            <p>
              No providers yet — connect an AWS account to deploy into, and a Tailscale tailnet so instances need no
              public ingress.
            </p>
          </div>
        )}
      </div>
    </>
  )
}

function ProviderCard({ p, deployable, role }: { p: Provider; deployable: boolean; role: string }) {
  const [showLog, setShowLog] = useState(false)
  const [provision, setProvision] = useState(false)
  const statusClass = p.status === 'ready' ? 'merged' : p.status === 'verifying' ? 'open' : 'agent'

  return (
    <div className="cs-item" style={{ cursor: 'default' }}>
      <div className="row">
        <span className="branch-name" style={{ fontSize: 15 }}>
          {p.name}
        </span>
        <span className={`pill ${statusClass}`}>{p.status}</span>
        <span className="pill human">{kindLabel(p.kind)}</span>
        {p.region && <span className="branch-sha">{p.region}</span>}
        {role === 'network' && <span className="pill human">network</span>}
        {role !== 'network' && !deployable && <span className="pill open">deployments pending</span>}
        <div className="spacer" />
        {deployable && (
          <button
            className="btn"
            style={{ padding: '2px 10px', fontSize: 12 }}
            disabled={p.status !== 'ready'}
            onClick={() => setProvision(true)}
          >
            provision instance
          </button>
        )}
        <button
          className="btn ghost"
          style={{ padding: '2px 10px', fontSize: 12 }}
          onClick={() => api(`/providers/${p.id}/verify`, { method: 'POST', body: '{}' })}
        >
          re-check
        </button>
        <button
          className="btn ghost"
          style={{ padding: '2px 10px', fontSize: 12 }}
          onClick={() => setShowLog(!showLog)}
        >
          {showLog ? 'hide checks' : 'checks'}
        </button>
        <button
          className="btn ghost"
          style={{ padding: '2px 10px', fontSize: 12 }}
          onClick={() => api(`/providers/${p.id}`, { method: 'DELETE' })}
        >
          remove
        </button>
      </div>
      <div className="meta" style={{ marginTop: 6, color: 'var(--text-dim)', fontSize: 12, fontFamily: 'var(--mono)' }}>
        {p.account || 'not verified yet'}
        {p.last_checked_at && ` · checked ${timeAgo(p.last_checked_at)}`}
      </div>
      {showLog && (
        <div className="filebox" style={{ marginTop: 10 }}>
          <div className="path">verification</div>
          <pre>{p.check_log || 'not checked yet'}</pre>
        </div>
      )}
      {provision && <ProvisionModal p={p} onClose={() => setProvision(false)} />}
    </div>
  )
}

// ProvisionModal launches a machine in the provider's account; it shows up in
// the Fleet tab immediately and becomes deployable once docker is installed.
function ProvisionModal({ p, onClose }: { p: Provider; onClose: () => void }) {
  const kind = KINDS.find((k) => k.id === p.kind)
  const [name, setName] = useState('')
  const [size, setSize] = useState('')
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const launch = async () => {
    setBusy(true)
    setError('')
    try {
      await api(`/providers/${p.id}/instances`, {
        method: 'POST',
        body: JSON.stringify({ name: name.trim(), size: size.trim() }),
      })
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 520 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">Provision instance</h2>
        <p className="page-sub">
          Launches a machine in <b>{p.name}</b> ({p.region || 'default region'}), installs docker, and enrolls it in
          your fleet. Removing it from Fleet terminates it.
        </p>
        <div className="row" style={{ marginBottom: 10 }}>
          <input
            className="input"
            placeholder="name (e.g. prod-1)"
            style={{ flex: 1 }}
            value={name}
            onChange={(e) => setName(e.target.value)}
            autoFocus
          />
          <input
            className="input"
            placeholder={`size (default ${kind?.sizeHint ?? 't3.small'})`}
            style={{ width: 200 }}
            value={size}
            onChange={(e) => setSize(e.target.value)}
          />
        </div>
        {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
        <div className="row" style={{ marginTop: 14 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" onClick={launch} disabled={busy}>
            {busy ? 'Launching…' : 'Launch'}
          </button>
        </div>
      </div>
    </div>
  )
}

function AddProviderModal({ onClose }: { onClose: () => void }) {
  const [kind, setKind] = useState<Kind>(KINDS[0])
  const [name, setName] = useState('')
  const [region, setRegion] = useState('')
  const [creds, setCreds] = useState<Record<string, string>>({})
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const add = async () => {
    setBusy(true)
    setError('')
    try {
      await api('/providers', {
        method: 'POST',
        body: JSON.stringify({ name: name.trim(), kind: kind.id, region: region.trim(), credentials: creds }),
      })
      onClose()
    } catch (e) {
      setError((e as Error).message)
      setBusy(false)
    }
  }

  return (
    <div className="overlay" onClick={onClose}>
      <div className="modal" style={{ width: 520 }} onClick={(e) => e.stopPropagation()}>
        <h2 className="page-title">Add provider</h2>
        <p className="page-sub">{kind.blurb}</p>
        <div className="row" style={{ marginBottom: 10, gap: 6 }}>
          {KINDS.map((k) => (
            <button
              key={k.id}
              className={`btn ${kind.id === k.id ? '' : 'ghost'}`}
              onClick={() => {
                setKind(k)
                setCreds({})
              }}
            >
              {k.label}
            </button>
          ))}
        </div>
        <div className="row" style={{ marginBottom: 10 }}>
          <input
            className="input"
            placeholder="name (e.g. prod-aws)"
            style={{ flex: 1 }}
            value={name}
            onChange={(e) => setName(e.target.value)}
          />
          {kind.role === 'compute' && (
            <input
              className="input"
              placeholder={`region (e.g. ${kind.regionHint})`}
              style={{ width: 180 }}
              value={region}
              onChange={(e) => setRegion(e.target.value)}
            />
          )}
        </div>
        {kind.fields.map((f) => (
          <input
            key={f.key}
            className="input"
            type={f.secret ? 'password' : 'text'}
            placeholder={f.hint ?? (f.optional ? `${f.label} (optional)` : f.label)}
            style={{ width: '100%', marginBottom: 8, fontFamily: 'var(--mono)', fontSize: 12 }}
            value={creds[f.key] ?? ''}
            onChange={(e) => setCreds({ ...creds, [f.key]: e.target.value })}
          />
        ))}
        {error && <p style={{ color: 'var(--amber)' }}>{error}</p>}
        <div className="row" style={{ marginTop: 14 }}>
          <div className="spacer" />
          <button className="btn ghost" onClick={onClose}>
            Cancel
          </button>
          <button className="btn" onClick={add} disabled={busy}>
            {busy ? 'Adding…' : 'Add & verify'}
          </button>
        </div>
      </div>
    </div>
  )
}
