import type { Approval, Conversation, Message, MessageJob, Sent } from '../api/types'
import { apiErr, json } from './fixtures'
import type { Call, Handler } from './server'

// Fixtures and a fake chat backend for tests of the chat store and screen.

export const conversationOf = (id: string, over: Partial<Conversation> = {}): Conversation => ({
  id,
  title: 'Conversation ' + id,
  created_at: '2026-01-01T10:00:00Z',
  updated_at: '2026-01-01T11:00:00Z',
  ...over
})

export const messageOf = (id: string, over: Partial<Message> = {}): Message => ({
  id,
  role: 'user',
  content: 'text of ' + id,
  job_id: '',
  created_at: '2026-01-01T11:00:00Z',
  job: null,
  ...over
})

export const userMessage = (id: string, job: MessageJob | null, content = 'question ' + id): Message =>
  messageOf(id, { role: 'user', content, job_id: job?.id ?? '', job })

export const answer = (id: string, content = 'answer ' + id): Message =>
  messageOf(id, { role: 'assistant', content })

export interface ChatState {
  conversations: Conversation[] // newest first
  messages: Record<string, Message[]> // per conversation, newest first
  approvals: Record<string, Approval[]> // pending approvals per job
  sent: Partial<Sent> // overrides of the 202 body
  failSend?: Response // the response of every send while set
  nextId: number
}

export const chatState = (over: Partial<ChatState> = {}): ChatState => ({
  conversations: [],
  messages: {},
  approvals: {},
  sent: {},
  nextId: 100,
  ...over
})

// page returns a keyset page of list after the entry with the cursor id.
const page = <T extends { id: string }>(list: T[], c: Call, key: string) => {
  const limit = Number(c.query.get('limit') ?? '50')
  const cursor = c.query.get('cursor')
  let start = 0
  if (cursor !== null) {
    const i = list.findIndex((x) => x.id === cursor)
    if (i < 0) return apiErr(400, 'invalid_request')
    start = i + 1
  }
  const items = list.slice(start, start + limit)
  const more = start + limit < list.length
  return json({ [key]: items, next_cursor: more ? items[items.length - 1].id : null })
}

// chatHandler answers the chat routes from the state, like the server:
// lists newest first with the id of the last entry as the cursor.
export const chatHandler =
  (st: ChatState): Handler =>
  (c: Call) => {
    if (c.path === '/conversations') {
      if (c.method === 'GET') return page(st.conversations, c, 'conversations')
      if (c.method === 'POST') {
        const title = (c.body as { title?: string } | undefined)?.title ?? ''
        const conv = conversationOf('c' + st.nextId++, { title })
        st.conversations.unshift(conv)
        st.messages[conv.id] = []
        return json(conv, 201)
      }
    }
    const conv = /^\/conversations\/([^/]+)$/.exec(c.path)
    if (conv) {
      const id = decodeURIComponent(conv[1])
      const i = st.conversations.findIndex((x) => x.id === id)
      if (i < 0) return apiErr(404, 'not_found')
      if (c.method === 'GET') return json(st.conversations[i])
      if (c.method === 'PUT') {
        st.conversations[i] = { ...st.conversations[i], title: (c.body as { title: string }).title }
        return json(st.conversations[i])
      }
      if (c.method === 'DELETE') {
        st.conversations.splice(i, 1)
        delete st.messages[id]
        return new Response(null, { status: 204 })
      }
    }
    const msgs = /^\/conversations\/([^/]+)\/messages$/.exec(c.path)
    if (msgs) {
      const id = decodeURIComponent(msgs[1])
      const list = st.messages[id]
      if (!list) return apiErr(404, 'not_found')
      if (c.method === 'GET') return page(list, c, 'messages')
      if (c.method === 'POST') {
        if (st.failSend) return st.failSend.clone()
        const jobId = 'j' + st.nextId++
        const job: MessageJob = { id: jobId, state: 'queued', error: '' }
        const m = userMessage('m' + st.nextId++, job, (c.body as { content: string }).content)
        list.unshift(m)
        const sent: Sent = {
          message: m,
          job: { id: jobId, state: 'queued' },
          agent: { paused: false, pause_reason: '', kill_switch: false },
          ...st.sent
        }
        return json(sent, 202)
      }
    }
    if (c.method === 'GET' && c.path === '/approvals' && c.query.has('job_id')) {
      return json({ approvals: st.approvals[c.query.get('job_id')!] ?? [], next_cursor: null })
    }
    return undefined
  }

export const errWithRetry = (status: number, code: string, retryAfter: number): Response =>
  new Response(JSON.stringify({ error: { code, message: 'server text' } }), {
    status,
    headers: { 'Content-Type': 'application/json', 'Retry-After': String(retryAfter) }
  })
