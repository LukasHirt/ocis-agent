import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, approvalOf } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import {
  answer,
  chatHandler,
  chatState,
  conversationOf,
  errWithRetry,
  messageOf,
  userMessage,
  type ChatState
} from '../test/chat'
import { MESSAGE_PAGE, mergeHead, useChatStore } from './chat'
import type { Message } from '../api/types'

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

describe('mergeHead', () => {
  it('replaces everything when the head reaches the end', () => {
    expect(mergeHead([messageOf('a')], { items: [messageOf('b')], next: null }, 'x')).toEqual({
      items: [messageOf('b')],
      cursor: null
    })
  })

  it('keeps the older pages behind the head without duplicates', () => {
    const existing = [messageOf('m5'), messageOf('m4'), messageOf('m3'), messageOf('m2'), messageOf('m1')]
    const head = { items: [messageOf('m6'), messageOf('m5'), messageOf('m4')], next: 'm4' }
    const r = mergeHead(existing, head, 'm1-cursor')
    expect(ids(r.items)).toEqual(['m6', 'm5', 'm4', 'm3', 'm2', 'm1'])
    expect(r.cursor).toBe('m1-cursor')
  })

  it('drops the older pages when the head does not reach them (a gap)', () => {
    const existing = [messageOf('m2'), messageOf('m1')]
    const head = { items: [messageOf('m9'), messageOf('m8')], next: 'm8' }
    const r = mergeHead(existing, head, null)
    expect(ids(r.items)).toEqual(['m9', 'm8'])
    expect(r.cursor).toBe('m8')
  })
})

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
      messages: { c1: [answer('m2'), userMessage('m1', { id: 'j1', state: 'succeeded', error: '' })] }
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
    expect(chat.blockedUntil).toBe(clock + Math.max(1, retry ?? 1) * 1000)
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
          userMessage('m2', { id: 'j2', state: 'waiting_approval', error: '' }),
          userMessage('m1', { id: 'j1', state: 'succeeded', error: '' })
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
      messages: { c1: [userMessage('m1', { id: 'j1', state: 'succeeded', error: '' })] }
    })
    const { chat, events, server } = await setup(st)
    await chat.openConversation('c1')
    const reads = () => server.calls.filter((c) => c.path === '/conversations/c1/messages').length
    const before = reads()
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(reads()).toBe(before)
    st.messages.c1 = [userMessage('m1', { id: 'j1', state: 'running', error: '' })]
    await chat.refreshOpen()
    st.messages.c1 = [userMessage('m1', { id: 'j1', state: 'failed', error: 'The agent stopped.' })]
    await events().onChanged(['jobs'])
    await flushPromises()
    expect(chat.thread[0].job?.state).toBe('failed')
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
