import type { AuditEvent } from '../api/types'
import { apiErr, json } from './fixtures'
import type { Call, Handler } from './server'

// Fixtures and a fake audit backend for tests of the activity store and
// screen.

export const eventOf = (seq: number, over: Partial<AuditEvent> = {}): AuditEvent => ({
  seq,
  ts: '2026-01-01T10:00:00.000Z',
  job_id: '',
  kind: 'tool_call',
  tool: 'files.list',
  risk: 'read',
  args_hash: '',
  cred: 'app',
  outcome: 'done',
  detail: '',
  ...over
})

export const eventsUpTo = (n: number): AuditEvent[] => Array.from({ length: n }, (_, i) => eventOf(i + 1))

export interface AuditState {
  events: AuditEvent[] // ascending by seq, as stored
  verify?: Response // the response of every verify while set
  exportRes?: () => Response // the response of every export while set
}

export const auditState = (over: Partial<AuditState> = {}): AuditState => ({ events: [], ...over })

// auditHandler answers the audit routes from the state, like the server:
// ascending by seq, the cursor is the seq after which a page starts, a
// next_cursor (the last returned seq) only when more follow.
export const auditHandler =
  (st: AuditState): Handler =>
  (c: Call) => {
    if (c.method !== 'GET') return undefined
    if (c.path === '/audit') {
      const cs = c.query.get('cursor')
      if (cs !== null && !/^[1-9]\d{0,17}$/.test(cs)) return apiErr(400, 'invalid_request')
      const after = cs === null ? 0 : Number(cs)
      const limit = Number(c.query.get('limit') ?? '50')
      if (!Number.isInteger(limit) || limit < 1 || limit > 200) return apiErr(400, 'invalid_request')
      const rest = st.events.filter((e) => e.seq > after)
      const items = rest.slice(0, limit)
      const more = rest.length > limit
      return json({ events: items, next_cursor: more ? String(items[items.length - 1].seq) : null })
    }
    if (c.path === '/audit/verify') {
      if (st.verify) return st.verify.clone()
      return json({ ok: true, limitation: 'removal of the newest records cannot be detected' })
    }
    if (c.path === '/audit/export') {
      if (st.exportRes) return st.exportRes()
      const body = st.events.map((e) => JSON.stringify({ ...e, prev_hash: 'p', hash: 'h' }) + '\n').join('')
      return new Response(body, {
        status: 200,
        headers: {
          'Content-Type': 'application/x-ndjson',
          'Content-Disposition': 'attachment; filename="audit-0a1b2c3d.jsonl"'
        }
      })
    }
    return undefined
  }

// auditReads lists the cursor and limit of every list request.
export const auditReads = (calls: Call[]) =>
  calls
    .filter((c) => c.method === 'GET' && c.path === '/audit')
    .map((c) => ({ cursor: c.query.get('cursor'), limit: c.query.get('limit') }))
