import type { MemoryFile } from '../api/types'
import { apiErr, json } from './fixtures'
import type { Call, Handler } from './server'

// A fake memory backend for tests of the memory store and screen, with
// the server's rules: names profile.md, projects.md and notes/<slug>.md,
// If-Match required on PUT (428) and compared exactly (412), 409 for an
// existing name on create, 64 KiB per file (413 on write), a file above
// it shown cut with truncated and editable false.

export const MAX_FILE = 64 << 10

export interface MemFile {
  content: string
  etag: string
  modified: string
  size?: number // the listed size, when it should differ from the content
}

export interface MemoryState {
  files: Record<string, MemFile>
  n: number // the next etag number
  get?: Record<string, Response> // fixed GET responses per name
  put?: Response // the response of every PUT while set
  create?: Response // the response of every POST while set
  list?: Response // the response of every list while set
}

export const memoryState = (files: Record<string, string> = {}): MemoryState => {
  const st: MemoryState = { files: {}, n: 1 }
  for (const [name, content] of Object.entries(files)) setFile(st, name, content)
  return st
}

// setFile writes a file as someone else would (the agent, the Files app):
// a new etag, quotes included as oCIS sends them.
export const setFile = (st: MemoryState, name: string, content: string): string => {
  const etag = `"e${st.n++}"`
  st.files[name] = { content, etag, modified: '2026-01-01T10:00:00Z' }
  return etag
}

const NAME = /^(profile\.md|projects\.md|notes\/[a-z0-9][a-z0-9-]{0,63}\.md)$/

const nameOf = (path: string): string | null => {
  const m = /^\/memory\/(.+)$/.exec(path)
  if (!m) return null
  const name = m[1].split('/').map(decodeURIComponent).join('/')
  return NAME.test(name) ? name : null
}

const bytes = (s: string) => new TextEncoder().encode(s).length

const docOf = (name: string, f: MemFile): MemoryFile => {
  const big = bytes(f.content) > MAX_FILE
  return {
    name,
    content: big ? f.content.slice(0, MAX_FILE) : f.content,
    etag: f.etag,
    truncated: big,
    editable: !big
  }
}

export const memoryHandler =
  (st: MemoryState): Handler =>
  (c: Call) => {
    if (c.path === '/memory' && c.method === 'GET') {
      if (st.list) return st.list.clone()
      const files = Object.keys(st.files)
        .sort()
        .map((name) => ({
          name,
          size: st.files[name].size ?? bytes(st.files[name].content),
          etag: st.files[name].etag,
          modified: st.files[name].modified
        }))
      return json({ files })
    }
    if (c.path === '/memory' && c.method === 'POST') {
      if (st.create) return st.create.clone()
      const b = c.body as { name: string; content: string }
      if (!NAME.test(b.name)) return apiErr(400, 'invalid_request')
      if (st.files[b.name]) return apiErr(409, 'already_exists')
      if (bytes(b.content) > MAX_FILE) return apiErr(413, 'too_large')
      const etag = setFile(st, b.name, b.content)
      return json({ name: b.name, etag, diff: '' }, 201)
    }
    if (!c.path.startsWith('/memory/')) return undefined
    const name = nameOf(c.path)
    if (name === null) return apiErr(400, 'invalid_request')
    if (c.method === 'GET') {
      if (st.get?.[name]) return st.get[name].clone()
      const f = st.files[name]
      if (!f) return apiErr(404, 'not_found')
      return json(docOf(name, f))
    }
    if (c.method === 'PUT') {
      if (st.put) return st.put.clone()
      const match = c.headers.get('If-Match')
      if (!match) return apiErr(428, 'precondition_required')
      const f = st.files[name]
      if (!f) return apiErr(404, 'not_found')
      const content = (c.body as { content: string }).content
      if (bytes(content) > MAX_FILE) return apiErr(413, 'too_large')
      if (f.etag !== match) return apiErr(412, 'precondition_failed')
      const etag = setFile(st, name, content)
      return json({ name, etag, diff: '' })
    }
    return undefined
  }
