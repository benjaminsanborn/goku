export type Project = {
  id: string
  org_id: string
  name: string
  region: string
  status: string
  changeset_count: number
  created_at: string
}

export type FileEntry = { path: string; content: string }

export type Changeset = {
  id: string
  project_id: string
  number: number
  title: string
  description: string
  branch: string
  status: string
  opened_by: string
  head_sha: string
  files: FileEntry[]
  created_at: string
  updated_at: string
}

export type AuditEvent = {
  seq: number
  actor: string
  action: string
  subject: string
  detail: Record<string, unknown>
  at: string
}

import { useEffect, useState } from 'react'

const BASE = '/v1'

export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(BASE + path, {
    headers: { 'Content-Type': 'application/json' },
    ...init,
  })
  const body = await res.json()
  if (!res.ok) throw new Error(body.error ?? res.statusText)
  return body as T
}

// usePoll fetches immediately and then on an interval, so agent-driven
// changes (via MCP) show up in the UI within a few seconds.
export function usePoll<T>(path: string, intervalMs = 3000): T | undefined {
  const [data, setData] = useState<T>()
  useEffect(() => {
    let active = true
    const load = () =>
      api<T>(path)
        .then((d) => active && setData(d))
        .catch(() => {})
    load()
    const t = setInterval(load, intervalMs)
    return () => {
      active = false
      clearInterval(t)
    }
  }, [path, intervalMs])
  return data
}

export function timeAgo(iso: string): string {
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000)
  if (s < 60) return 'just now'
  if (s < 3600) return `${Math.floor(s / 60)}m ago`
  if (s < 86400) return `${Math.floor(s / 3600)}h ago`
  return `${Math.floor(s / 86400)}d ago`
}
