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
// ascending by seq (the cursor names an event of the log, the page starts
// after it) or, with order=desc, newest first (the cursor "d<seq>" names an
// event of the log, the page starts below it); a cursor naming no event is
// 400; kind filters; a next_cursor (the last
// returned seq) only when more follow.
export const auditHandler =
  (st: AuditState): Handler =>
  (c: Call) => {
    if (c.method !== 'GET') return undefined
    if (c.path === '/audit') {
      const order = c.query.get('order') ?? 'asc'
      if (order !== 'asc' && order !== 'desc') return apiErr(400, 'invalid_request')
      const desc = order === 'desc'
      const cs = c.query.get('cursor')
      if (cs !== null && !(desc ? /^d[1-9]\d{0,17}$/ : /^[1-9]\d{0,17}$/).test(cs)) return apiErr(400, 'invalid_request')
      const pos = cs === null ? 0 : Number(desc ? cs.slice(1) : cs)
      if (pos > 0 && !st.events.some((e) => e.seq === pos)) return apiErr(400, 'invalid_request')
      const limit = Number(c.query.get('limit') ?? '50')
      if (!Number.isInteger(limit) || limit < 1 || limit > 200) return apiErr(400, 'invalid_request')
      const kind = c.query.get('kind')
      const ordered = desc ? [...st.events].reverse() : st.events
      const rest = ordered.filter(
        (e) => (desc ? pos === 0 || e.seq < pos : e.seq > pos) && (kind === null || e.kind === kind)
      )
      const items = rest.slice(0, limit)
      const more = rest.length > limit
      const last = items[items.length - 1]?.seq
      return json({ events: items, next_cursor: more ? (desc ? 'd' : '') + String(last) : null })
    }
    if (c.path === '/audit/verify') {
      if (st.verify) return st.verify.clone()
      return json({ ok: true, limitation: 'removal of the newest records cannot be detected', checked: st.events.length })
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

// auditReads lists the order, cursor, limit and kind of every list
// request (null when not sent).
export const auditReads = (calls: Call[]) =>
  calls
    .filter((c) => c.method === 'GET' && c.path === '/audit')
    .map((c) => ({
      order: c.query.get('order'),
      cursor: c.query.get('cursor'),
      limit: c.query.get('limit'),
      kind: c.query.get('kind')
    }))

// newest and forward are the expected reads: a newest-first page, and a
// page of events after a seq.
export const newest = (cursor: string | null = null, kind: string | null = null, limit = 50) => ({
  order: 'desc',
  cursor,
  limit: String(limit),
  kind
})
export const forward = (cursor: string | null, kind: string | null = null, limit = 50) => ({
  order: null,
  cursor,
  limit: String(limit),
  kind
})
