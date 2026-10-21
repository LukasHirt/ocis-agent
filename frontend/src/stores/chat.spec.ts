import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, approvalOf, json, statusOf } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import {
  answer,
  chatHandler,
  chatState,
  conversationOf,
  errWithRetry,
  jobOfMessage,
  messageOf,
  userMessage,
  type ChatState
} from '../test/chat'
import { LOCAL_BLOCK_MAX_MS, MESSAGE_PAGE, useChatStore } from './chat'
import type { Message, MessageJob } from '../api/types'

let detach: (() => void) | undefined
afterEach(() => {
  detach?.()
  detach = undefined
})

const setup = async (st: ChatState = chatState(), extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? chatHandler(st)(c))
  const agent = await startStore(server)
  const chat = useChatStore()
  detach = chat.attach()
  return { st, server, chat, ...agent }
}

const ids = (list: { id: string }[]) => list.map((x) => x.id)

// many returns n user messages m<n>..m1, newest first.
const many = (n: number): Message[] =>
  Array.from({ length: n }, (_, i) => messageOf('m' + (n - i)))

describe('useChatStore', () => {
  it('loads the conversations newest first and more with the cursor', async () => {
    const convs = Array.from({ length: 60 }, (_, i) => conversationOf('c' + i))
    const { chat, server } = await setup(chatState({ conversations: convs }))
    await chat.loadConversations()
    expect(chat.conversations).toHaveLength(50)
    expect(chat.conversations[0].id).toBe('c0')
    expect(chat.conversationsCursor).toBe('c49')
    await chat.loadMoreConversations()
    expect(ids(chat.conversations)).toEqual(ids(convs))
    expect(chat.conversationsCursor).toBeNull()
    const last = server.calls.filter((c) => c.path === '/conversations').at(-1)!
    expect(last.query.get('cursor')).toBe('c49')
  })

  it('follows an empty page that still carries a cursor', async () => {
    let n = 0
    const { chat } = await setup(chatState(), (c) => {
      if (c.method !== 'GET' || c.path !== '/conversations') return undefined
      n++
      if (n === 1)
        return new Response(JSON.stringify({ conversations: [], next_cursor: 'k1' }), { status: 200 })
      return new Response(
        JSON.stringify({ conversations: [conversationOf('c1')], next_cursor: null }),
        { status: 200 }
      )
    })
    await chat.loadConversations()
    expect(chat.conversations).toEqual([])
    expect(chat.conversationsCursor).toBe('k1')
    await chat.loadMoreConversations()
    expect(ids(chat.conversations)).toEqual(['c1'])
  })

  it('keeps a fixed error and the old list when loading fails', async () => {
    let fail = false
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { chat } = await setup(st, (c) =>
      fail && c.path === '/conversations' ? apiErr(503, 'unavailable') : undefined
    )
    await chat.loadConversations()
    fail = true
    await chat.loadConversations()
    expect(ids(chat.conversations)).toEqual(['c1'])
    expect(chat.conversationsError).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await chat.loadConversations()
    expect(chat.conversationsError).toBeNull()
  })

  it('opens a conversation and shows the thread oldest first', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [answer('m2'), userMessage('m1', jobOfMessage('j1', 'succeeded'))] }
    })
    const { chat } = await setup(st)
    await chat.openConversation('c1')
    expect(chat.current).toBe('c1')
    expect(ids(chat.messages)).toEqual(['m2', 'm1']) // API order
    expect(ids(chat.thread)).toEqual(['m1', 'm2']) // chronological
    expect(chat.olderCursor).toBeNull()
  })

  it('loads older messages with the cursor and keeps them on a refresh', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: many(120) } })
    const { chat, server } = await setup(st)
    await chat.openConversation('c1')
    expect(chat.messages).toHaveLength(MESSAGE_PAGE)
    expect(chat.olderCursor).toBe('m71')
    await chat.loadOlder()
    expect(chat.messages).toHaveLength(MESSAGE_PAGE * 2)
    expect(chat.thread[0].id).toBe('m21')
    const last = server.calls.filter((c) => c.path === '/conversations/c1/messages').at(-1)!
    expect(last.query.get('cursor')).toBe('m71')
    // a new message arrives: the head is merged, the older page stays
    st.messages.c1.unshift(messageOf('m121'))
    await chat.refreshOpen()
    expect(chat.messages).toHaveLength(MESSAGE_PAGE * 2 + 1)
    expect(chat.thread.at(-1)!.id).toBe('m121')
    expect(new Set(ids(chat.messages)).size).toBe(chat.messages.length)
    expect(chat.olderCursor).toBe('m21')
  })

  it('reports a conversation that no longer exists', async () => {
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { chat } = await setup(st)
    await chat.loadConversations()
    await chat.openConversation('c1')
    expect(chat.missing).toBe(true)
    expect(chat.conversations).toEqual([]) // dropped from the list
  })

  it('ignores the answer for a conversation that is no longer open', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({
      conversations: [conversationOf('c1'), conversationOf('c2')],
      messages: { c1: [messageOf('a1')], c2: [messageOf('b1')] }
    })
    const { chat } = await setup(st, (c) =>
      c.path === '/conversations/c1/messages' ? gate.then(() => chatHandler(st)(c)!) : undefined
    )
    const p1 = chat.openConversation('c1')
    await chat.openConversation('c2')
    release()
    await p1
    expect(chat.current).toBe('c2')
    expect(ids(chat.messages)).toEqual(['b1'])
  })

  it('sends a message, clears the draft and shows the server message after the 202', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, server } = await setup(st)
    await chat.openConversation('c1')
    chat.draftByConversation.c1 = 'Hello'
    const p = chat.send('Hello')
    expect(chat.sending).toBe(true)
    expect(chat.thread).toEqual([]) // no optimistic message
    expect(await p).toBe(true)
    expect(chat.sending).toBe(false)
    expect(chat.draftByConversation.c1).toBeUndefined()
    expect(chat.thread.map((m) => m.content)).toEqual(['Hello'])
    expect(server.calls.find((c) => c.method === 'POST')!.body).toEqual({ content: 'Hello' })
    expect(chat.sendError).toBeNull()
  })

  it('blocks a second send while one is in flight', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, server } = await setup(st, (c) =>
      c.method === 'POST' ? gate.then(() => chatHandler(st)(c)!) : undefined
    )
    await chat.openConversation('c1')
    const a = chat.send('one')
    const b = chat.send('one')
    expect(await b).toBe(false)
    release()
    expect(await a).toBe(true)
    expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(1)
  })

  it('waits for the cooldown after a send', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, server, platform } = await setup(st)
    let clock = Date.UTC(2026, 0, 1, 12)
    platform.now.mockImplementation(() => clock)
    await chat.openConversation('c1')
    expect(await chat.send('one')).toBe(true)
    clock += 400
    expect(await chat.send('two')).toBe(false)
    expect(chat.sendError?.code).toBe('cooldown')
    expect(chat.sendError?.message).toBe('Wait 1s before sending again.')
    clock += 600
    expect(await chat.send('two')).toBe(true)
    expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(2)
  })

  it('validates the text before sending', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, server } = await setup(st)
    await chat.openConversation('c1')
    expect(await chat.send('   ')).toBe(false)
    expect(chat.sendError?.code).toBe('empty')
    expect(await chat.send('€'.repeat(2731))).toBe(false)
    expect(chat.sendError?.code).toBe('too_long')
    expect(chat.sendError?.message).toBe('The message is longer than 8192 bytes. Shorten it.')
    expect(await chat.send('a\u0007b')).toBe(false)
    expect(chat.sendError?.code).toBe('control')
    expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  })

  const refusals: Array<[string, Response, string, number | undefined]> = [
    [
      'too_many_pending',
      errWithRetry(429, 'too_many_pending', 5),
      'You have too many questions waiting. Wait for an answer, then try again.',
      5
    ],
    [
      'budget_exhausted',
      errWithRetry(429, 'budget_exhausted', 3600),
      "Today's token budget is used up. It resets at midnight UTC.",
      3600
    ],
    [
      'limit_reached',
      errWithRetry(429, 'limit_reached', 5400),
      "You have reached today's limit of questions. You can ask again in 1h 30m.",
      5400
    ],
    [
      'rate_limited',
      errWithRetry(429, 'rate_limited', 1),
      'You are sending messages too quickly. Try again in 1s.',
      1
    ],
    ['not_enrolled', apiErr(409, 'not_enrolled'), 'The agent is not set up for you yet. Set it up first.', undefined],
    ['not_found', apiErr(404, 'not_found'), 'This conversation no longer exists.', undefined],
    ['invalid_request', apiErr(400, 'invalid_request'), 'The request was not accepted. Check your input and try again.', undefined]
  ]

  it.each(refusals)('keeps the draft and shows a fixed message on %s', async (code, res, text, retry) => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] }, failSend: res })
    const { chat, platform } = await setup(st)
    const clock = Date.UTC(2026, 0, 1, 12)
    platform.now.mockImplementation(() => clock)
    await chat.openConversation('c1')
    chat.draftByConversation.c1 = 'keep me'
    expect(await chat.send('keep me')).toBe(false)
    expect(chat.draftByConversation.c1).toBe('keep me')
    expect(chat.sendError).toEqual({ code, message: text, ...(retry !== undefined && { retryAfter: retry }) })
    expect(chat.needsEnrol).toBe(code === 'not_enrolled')
    expect(chat.sending).toBe(false)
    // a refused send uses the cooldown too, as long as the server says
    // ...but never longer than LOCAL_BLOCK_MAX_MS: day-long refusals are left to the server
    expect(chat.blockedUntil).toBe(clock + Math.min(Math.max(1, retry ?? 1) * 1000, LOCAL_BLOCK_MAX_MS))
  })

  it('finishes a send that outlives a switch to another conversation', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1'), conversationOf('c2')], messages: { c1: [], c2: [] } })
    let gated = true
    const { chat, platform } = await setup(st, (c) =>
      gated && c.method === 'POST' ? gate.then(() => chatHandler(st)(c)!) : undefined
    )
    await chat.openConversation('c1')
    chat.draftByConversation.c1 = 'sent text'
    const p = chat.send('sent text')
    expect(chat.sending).toBe(true)
    await chat.openConversation('c2')
    expect(chat.sending).toBe(false) // c2 is not sending
    release()
    await p
    expect(chat.sending).toBe(false)
    expect(chat.draftByConversation.c1).toBeUndefined() // it was sent
    expect(chat.sendError).toBeNull()
    expect(chat.current).toBe('c2')
    expect(chat.thread).toEqual([]) // c2's thread, not c1's
    gated = false
    platform.now.mockReturnValue(Date.UTC(2026, 0, 1, 13)) // after the cooldown
    expect(await chat.send('in c2')).toBe(true)
  })

  it('finishes a send that outlives the deletion of its conversation', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat } = await setup(st, (c) =>
      c.method === 'POST' ? gate.then(() => chatHandler(st)(c)!) : undefined
    )
    await chat.loadConversations()
    await chat.openConversation('c1')
    const p = chat.send('x')
    expect(await chat.remove('c1')).toBe(true)
    release()
    await p
    expect(chat.sending).toBe(false)
    expect(chat.current).toBeNull()
  })

  it('does not show a send refusal of another conversation', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1'), conversationOf('c2')], messages: { c1: [], c2: [] } })
    const { chat } = await setup(st, (c) =>
      c.method === 'POST' ? gate.then(() => apiErr(503, 'unavailable')) : undefined
    )
    await chat.openConversation('c1')
    chat.draftByConversation.c1 = 'keep'
    const p = chat.send('keep')
    await chat.openConversation('c2')
    release()
    await p
    expect(chat.sendError).toBeNull()
    expect(chat.draftByConversation.c1).toBe('keep')
  })

  it('keeps a list load that is in flight when a conversation is opened', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat } = await setup(st, (c) =>
      c.method === 'GET' && c.path === '/conversations' ? gate.then(() => chatHandler(st)(c)!) : undefined
    )
    const list = chat.loadConversations()
    await chat.openConversation('c1')
    release()
    await list
    expect(ids(chat.conversations)).toEqual(['c1'])
    expect(chat.conversationsLoaded).toBe(true)
    expect(chat.conversationsLoading).toBe(false)
  })

  it('keeps a "load more" that is in flight when a conversation is opened', async () => {
    const convs = Array.from({ length: 60 }, (_, i) => conversationOf('c' + i))
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: convs, messages: { c0: [] } })
    const { chat } = await setup(st, (c) =>
      c.method === 'GET' && c.path === '/conversations' && c.query.has('cursor')
        ? gate.then(() => chatHandler(st)(c)!)
        : undefined
    )
    await chat.loadConversations()
    const more = chat.loadMoreConversations()
    await chat.openConversation('c0')
    release()
    await more
    expect(chat.conversations).toHaveLength(60)
    expect(chat.conversationsLoading).toBe(false)
  })

  it('reads the list from the top again when the cursor became unknown', async () => {
    const convs = Array.from({ length: 60 }, (_, i) => conversationOf('c' + i))
    const st = chatState({ conversations: convs })
    const { chat } = await setup(st)
    await chat.loadConversations()
    expect(chat.conversationsCursor).toBe('c49')
    st.conversations = st.conversations.filter((c) => c.id !== 'c49') // deleted elsewhere: 400 for the cursor
    await chat.loadMoreConversations()
    expect(ids(chat.conversations)).toEqual(ids(st.conversations.slice(0, 50)))
    expect(chat.conversationsCursor).toBe('c50')
    expect(chat.conversationsError).toBeNull()
    await chat.loadMoreConversations()
    expect(ids(chat.conversations)).toEqual(ids(st.conversations))
  })

  it('drops a long local block when the settings or the budget change', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [] },
      failSend: errWithRetry(429, 'budget_exhausted', 3600)
    })
    const { chat, events, store } = await setup(st)
    await chat.openConversation('c1')
    await chat.send('x')
    expect(chat.blockedUntil).toBeGreaterThan(0)
    await events().onChanged(['settings'])
    expect(chat.blockedUntil).toBe(0)
    await chat.send('x')
    expect(chat.blockedUntil).toBeGreaterThan(0)
    store.status = statusOf({ budget: { used_tokens: 0, daily_tokens: 500000, resets_at: null } })
    await flushPromises()
    expect(chat.blockedUntil).toBe(0)
  })

  it('refreshes the inline approvals on an activity change', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [userMessage('m1', jobOfMessage('j1', 'waiting_approval'))] },
      approvals: { j1: [approvalOf('a1', { job_id: 'j1' }), approvalOf('a2', { job_id: 'j1' })] }
    })
    const { chat, events } = await setup(st)
    await chat.openConversation('c1')
    await flushPromises()
    expect(chat.jobApprovals.j1?.items).toHaveLength(2)
    st.approvals.j1 = [approvalOf('a2', { job_id: 'j1' })] // a1 was decided
    await events().onChanged(['activity'])
    await flushPromises()
    expect(chat.jobApprovals.j1?.items.map((a) => a.id)).toEqual(['a2'])
  })

  it('enrol clears the not_enrolled refusal', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] }, failSend: apiErr(409, 'not_enrolled') })
    const { chat } = await setup(st, (c) => (c.method === 'POST' && c.path === '/enroll' ? json(statusOf()) : undefined))
    await chat.openConversation('c1')
    await chat.send('x')
    expect(chat.sendError?.code).toBe('not_enrolled')
    expect(await chat.enrol()).toBe(true)
    expect(chat.sendError).toBeNull()
    expect(chat.needsEnrol).toBe(false)
  })

  it('remembers why nothing runs after a 202 for the sent message', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [] },
      sent: { agent: { paused: true, pause_reason: 'user', kill_switch: false } }
    })
    const { chat } = await setup(st)
    await chat.openConversation('c1')
    await chat.send('hi')
    const m = chat.thread[0]
    expect(chat.notices[m.id]).toBe('paused')
  })

  it.each([
    [{ paused: false, pause_reason: '', kill_switch: true }, 'killed'],
    [{ paused: true, pause_reason: 'credential_revoked', kill_switch: false }, 'credentials'],
    [{ paused: true, pause_reason: 'token_anomaly', kill_switch: false }, 'paused'],
    [{ paused: false, pause_reason: '', kill_switch: false }, undefined]
  ])('maps the agent of a 202 (%o) to %s', async (agentInfo, notice) => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] }, sent: { agent: agentInfo } })
    const { chat } = await setup(st)
    await chat.openConversation('c1')
    await chat.send('hi')
    expect(chat.notices[chat.thread[0].id]).toBe(notice)
  })

  it('creates a conversation at the top of the list', async () => {
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { chat, server } = await setup(st)
    await chat.loadConversations()
    const c = await chat.createConversation()
    expect(c?.id).toBe('c100')
    expect(ids(chat.conversations)).toEqual(['c100', 'c1'])
    expect(server.calls.find((x) => x.method === 'POST')!.body).toEqual({})
  })

  it('explains the conversation limit on 409 limit_reached', async () => {
    const { chat } = await setup(chatState(), (c) =>
      c.method === 'POST' && c.path === '/conversations' ? apiErr(409, 'limit_reached') : undefined
    )
    expect(await chat.createConversation()).toBeNull()
    expect(chat.actionError).toBe(
      'You have 200 conversations, the most there can be. Delete one to start a new one.'
    )
  })

  it('offers enrolment when creating a conversation is refused with not_enrolled', async () => {
    const { chat } = await setup(chatState(), (c) =>
      c.method === 'POST' && c.path === '/conversations' ? apiErr(409, 'not_enrolled') : undefined
    )
    expect(await chat.createConversation()).toBeNull()
    expect(chat.needsEnrol).toBe(true)
  })

  it('renames with a trimmed, checked title', async () => {
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { chat, server } = await setup(st)
    await chat.loadConversations()
    expect(await chat.rename('c1', '  ')).toBe(false)
    expect(chat.actionError).toBe('Enter a title.')
    expect(await chat.rename('c1', 'x'.repeat(101))).toBe(false)
    expect(chat.actionError).toBe('The title can have at most 100 characters.')
    expect(await chat.rename('c1', 'a\u202eb')).toBe(false)
    expect(chat.actionError).toBe('The title cannot contain control or text direction characters.')
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(0)
    expect(await chat.rename('c1', '  Plans  ')).toBe(true)
    expect(server.calls.find((c) => c.method === 'PUT')!.body).toEqual({ title: 'Plans' })
    expect(chat.conversations[0].title).toBe('Plans')
    expect(chat.actionError).toBeNull()
  })

  it('deletes after a confirmation that names the cancelled questions', async () => {
    const st = chatState({ conversations: [conversationOf('c1'), conversationOf('c2')], messages: { c1: [], c2: [] } })
    const { chat, platform, server } = await setup(st)
    await chat.loadConversations()
    await chat.openConversation('c1')
    platform.confirm.mockResolvedValueOnce(false)
    expect(await chat.remove('c1')).toBe(false)
    expect(server.calls.filter((c) => c.method === 'DELETE')).toHaveLength(0)
    const opts = platform.confirm.mock.calls[0][0]
    expect(opts.danger).toBe(true)
    expect(opts.message).toContain('still waiting or being answered are cancelled')
    expect(await chat.remove('c1')).toBe(true)
    expect(ids(chat.conversations)).toEqual(['c2'])
    expect(chat.current).toBeNull()
    expect(chat.messages).toEqual([])
  })

  it('treats a conversation already deleted elsewhere as deleted', async () => {
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { chat } = await setup(st)
    await chat.loadConversations()
    st.conversations = []
    expect(await chat.remove('c1')).toBe(true)
    expect(chat.conversations).toEqual([])
    expect(chat.actionError).toBeNull()
  })

  it('loads the pending approvals of a job that waits for one', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: {
        c1: [
          userMessage('m2', jobOfMessage('j2', 'waiting_approval')),
          userMessage('m1', jobOfMessage('j1', 'succeeded'))
        ]
      },
      approvals: { j2: [approvalOf('a1', { job_id: 'j2' })] }
    })
    const { chat, server } = await setup(st)
    await chat.openConversation('c1')
    await flushPromises()
    expect(chat.jobApprovals.j2?.items.map((a) => a.id)).toEqual(['a1'])
    expect(chat.jobApprovals.j1).toBeUndefined()
    const q = server.calls.find((c) => c.path === '/approvals' && c.query.has('job_id'))!.query
    expect(q.get('job_id')).toBe('j2')
    expect(q.has('state')).toBe(false) // pending is the default
  })

  it('refreshes the list and the open thread on a messages change', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [messageOf('m1')] } })
    const { chat, events } = await setup(st)
    await chat.loadConversations()
    await chat.openConversation('c1')
    st.messages.c1.unshift(answer('m2'))
    st.conversations.unshift(conversationOf('c2'))
    await events().onChanged(['messages'])
    await flushPromises()
    expect(ids(chat.thread)).toEqual(['m1', 'm2'])
    expect(ids(chat.conversations)).toEqual(['c2', 'c1'])
  })

  it('refreshes the thread on a jobs change only while a question is unanswered', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [userMessage('m1', jobOfMessage('j1', 'succeeded'))] }
    })
    const { chat, events, server } = await setup(st)
    await chat.openConversation('c1')
    const reads = () => server.calls.filter((c) => c.path === '/conversations/c1/messages').length
    const before = reads()
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(reads()).toBe(before)
    st.messages.c1 = [userMessage('m1', jobOfMessage('j1', 'running'))]
    await chat.refreshOpen()
    st.messages.c1 = [userMessage('m1', jobOfMessage('j1', 'failed', { error: 'The agent stopped.' }))]
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(chat.thread[0].job?.state).toBe('failed')
  })

  it('follows the partial answer on change events and replaces it with the answer', async () => {
    const running = (partial: string, partial_state: string): MessageJob => ({
      id: 'j1',
      state: 'running',
      error: '',
      partial,
      partial_state
    })
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [userMessage('m1', running('', 'thinking'))] }
    })
    const { chat, events, server } = await setup(st)
    await chat.openConversation('c1')
    expect(chat.partials).toEqual({ m1: { text: '', state: 'thinking' } })

    st.messages.c1 = [userMessage('m1', running('Hello', 'writing'))]
    await events().onChanged(['jobs', 'messages'])
    await flushPromises()
    expect(chat.partials).toEqual({ m1: { text: 'Hello', state: 'writing' } })

    // nothing is read between change events (no polling of its own)
    const reads = () => server.calls.filter((c) => c.path === '/conversations/c1/messages').length
    const before = reads()
    vi.useFakeTimers({ toFake: ['setTimeout', 'setInterval'] })
    try {
      vi.advanceTimersByTime(30_000)
      await flushPromises()
      expect(reads()).toBe(before)
    } finally {
      vi.useRealTimers()
    }

    // the end: the answer is listed, the partial gone in the same read
    st.messages.c1 = [
      { ...answer('m2', 'Hello world'), job_id: 'j1' },
      userMessage('m1', { ...running('', ''), state: 'succeeded' })
    ]
    await events().onChanged(['jobs', 'messages'])
    await flushPromises()
    expect(chat.partials).toEqual({})
    expect(chat.thread.map((m) => m.content)).toEqual(['question m1', 'Hello world'])
  })

  it.each([
    ['waiting_approval', 'text', 'writing'],
    ['failed', 'text', 'writing'],
    ['cancelled', 'text', 'writing'],
    ['queued', 'text', 'thinking'],
    ['succeeded', 'text', 'writing'],
    ['running', '', ''],
    ['running', 'text', ''],
    ['running', 'text', 'dreaming']
  ])('shows no partial for a %s job with %j (%s)', async (state, partial, partial_state) => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [userMessage('m1', { id: 'j1', state, error: '', partial, partial_state })] }
    })
    const { chat } = await setup(st)
    await chat.openConversation('c1')
    expect(chat.partials).toEqual({})
  })

  it('shows no partial once the answer of its job is listed, nor on an answer or notice', async () => {
    const job: MessageJob = { id: 'j1', state: 'running', error: '', partial: 'x', partial_state: 'writing' }
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: {
        c1: [
          { ...answer('m3', 'final'), job_id: 'j1', job },
          messageOf('m2', { role: 'system', job_id: 'j1', job }),
          userMessage('m1', job)
        ]
      }
    })
    const { chat } = await setup(st)
    await chat.openConversation('c1')
    expect(chat.partials).toEqual({})
  })

  it('reads everything again on a ready or poll', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, events } = await setup(st)
    await chat.loadConversations()
    await chat.openConversation('c1')
    st.messages.c1.unshift(answer('m9'))
    st.conversations.unshift(conversationOf('c2'))
    await events().onReady()
    await flushPromises()
    expect(ids(chat.thread)).toEqual(['m9'])
    expect(ids(chat.conversations)).toEqual(['c2', 'c1'])
  })

  it('coalesces refreshes while one is in flight', async () => {
    let release!: () => void
    let gate = new Promise<void>((r) => (release = r))
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    let reads = 0
    const { chat } = await setup(st, (c) => {
      if (c.path !== '/conversations/c1/messages') return undefined
      reads++
      return gate.then(() => chatHandler(st)(c)!)
    })
    const open = chat.openConversation('c1')
    release()
    await open
    reads = 0
    gate = new Promise<void>((r) => (release = r))
    const a = chat.refreshOpen()
    const b = chat.refreshOpen()
    const c = chat.refreshOpen()
    release()
    await Promise.all([a, b, c])
    expect(reads).toBe(2) // the one in flight and one follow-up
  })

  it('does nothing on changes once detached', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [] } })
    const { chat, events, server } = await setup(st)
    await chat.openConversation('c1')
    detach!()
    detach = undefined
    const n = server.calls.length
    await events().onChanged(['messages'])
    await flushPromises()
    expect(server.calls.length).toBe(n)
  })

  it('forgets everything when the session ends, drafts too', async () => {
    const st = chatState({ conversations: [conversationOf('c1')], messages: { c1: [messageOf('m1')] } })
    const { chat, store } = await setup(st)
    await chat.loadConversations()
    await chat.openConversation('c1')
    chat.draftByConversation.c1 = 'secret plan'
    store.stop()
    expect(chat.conversations).toEqual([])
    expect(chat.current).toBeNull()
    expect(chat.messages).toEqual([])
    expect(chat.draftByConversation).toEqual({})
    expect(chat.sendError).toBeNull()
  })
})
