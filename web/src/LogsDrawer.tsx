import { useEffect, useRef, useState } from 'react'
import { getToken } from './api'
import type { Unit } from './ArchDiagram'

// LogsDrawer live-tails a container's logs over a streaming fetch.
export default function LogsDrawer({
  projectRef,
  unit,
  onClose,
}: {
  projectRef: string
  unit: Unit
  onClose: () => void
}) {
  const [text, setText] = useState('connecting…\n')
  const [paused, setPaused] = useState(false)
  const preRef = useRef<HTMLPreElement>(null)
  const pausedRef = useRef(false)
  pausedRef.current = paused

  useEffect(() => {
    const ctrl = new AbortController()
    const token = getToken()
    fetch(`/v1/projects/${projectRef}/logs?container=${encodeURIComponent(unit.container!)}&follow=1&tail=300`, {
      signal: ctrl.signal,
      headers: token ? { Authorization: `Bearer ${token}` } : {},
    })
      .then(async (res) => {
        if (!res.ok || !res.body) {
          setText(`error: ${res.status} ${res.statusText}\n`)
          return
        }
        setText('')
        const reader = res.body.getReader()
        const decoder = new TextDecoder()
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          const chunk = decoder.decode(value, { stream: true })
          setText((t) => {
            const next = t + chunk
            // keep the tail bounded
            return next.length > 400_000 ? next.slice(-300_000) : next
          })
        }
        setText((t) => t + '\n— stream ended —\n')
      })
      .catch(() => {})
    return () => ctrl.abort()
  }, [projectRef, unit.container])

  useEffect(() => {
    if (!pausedRef.current && preRef.current) {
      preRef.current.scrollTop = preRef.current.scrollHeight
    }
  }, [text])

  return (
    <div className="logs-drawer">
      <div className="row logs-head">
        <span className={`unit-dot ${unit.status}`}>●</span>
        <span className="branch-name">{unit.name}</span>
        <span className="branch-sha">{unit.container}</span>
        <span className="pill open">live</span>
        <div className="spacer" />
        <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={() => setPaused(!paused)}>
          {paused ? 'auto-scroll' : 'pause scroll'}
        </button>
        <button className="btn ghost" style={{ padding: '2px 10px', fontSize: 12 }} onClick={onClose}>
          close
        </button>
      </div>
      <pre ref={preRef} className="logs-body">{text}</pre>
    </div>
  )
}
