import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, gatewayErr, json } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import {
  auditHandler,
  auditReads,
  auditState,
  eventOf,
  eventsUpTo,
  forward,
  newest,
  type AuditState
} from '../test/audit'
import { errWithRetry } from '../test/chat'
import { AUDIT_PAGE, MAX_KEPT_EVENTS, useAuditStore } from './audit'
import { useAgentStore } from './agent'

const saved = vi.hoisted(() => [] as Array<{ blob: Blob; filename: string }>)
vi.mock('../util/download', () => ({
  saveBlob: (blob: Blob, filename: string) => {
    saved.push({ blob, filename })
  }
}))

let detach: (() => void) | undefined
afterEach(() => {
  detach?.()
  detach = undefined
  saved.length = 0
})

const setup = async (st: AuditState = auditState(), extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? auditHandler(st)(c))
  const agent = await startStore(server)
  const audit = useAuditStore()
  detach = audit.attach()
  return { st, server, audit, ...agent }
}

const seqs = (list: { seq: number }[]) => list.map((e) => e.seq)
const range = (from: number, to: number) => {
  const out: number[] = []
  for (let i = from; i >= to; i--) out.push(i)
  return out
}

describe('useAuditStore: reading', () => {
  it('reads a short log in one newest-first request', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(5) }))
    await audit.refresh()
    expect(seqs(audit.events)).toEqual([5, 4, 3, 2, 1])
    expect(audit.loaded).toBe(true)
    expect(audit.hasOlder).toBe(false)
    expect(auditReads(server.calls)).toEqual([newest()])
  })

  it('shows an empty log as loaded and empty', async () => {
    const { audit } = await setup()
    await audit.refresh()
    expect(audit.events).toEqual([])
    expect(audit.loaded).toBe(true)
    expect(audit.hasOlder).toBe(false)
  })

  it('reads the newest events of a very long log with one request and pages back with the server cursors', async () => {
    const st = auditState({ events: eventsUpTo(200_000) })
    const { audit, server } = await setup(st)
    await audit.refresh()
    expect(seqs(audit.events)).toEqual(range(200_000, 200_000 - AUDIT_PAGE + 1))
    expect(audit.hasOlder).toBe(true)
    expect(auditReads(server.calls)).toEqual([newest()])

    const before = server.calls.length
    await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(range(200_000, 200_000 - 2 * AUDIT_PAGE + 1))
    expect(auditReads(server.calls.slice(before))).toEqual([newest('d' + (200_000 - AUDIT_PAGE + 1))])
  })

  it('pages back to the start of the log', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(5 * AUDIT_PAGE + 3) }))
    await audit.refresh()
    while (audit.hasOlder) await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(range(5 * AUDIT_PAGE + 3, 1))
    expect(auditReads(server.calls)).toHaveLength(6)
  })

  it('copes with gaps in the sequence numbers', async () => {
    const events = eventsUpTo(400).filter((e) => e.seq % 3 !== 0 && (e.seq < 300 || e.seq > 380))
    const { audit } = await setup(auditState({ events }))
    await audit.refresh()
    expect(audit.events[0].seq).toBe(400)
    while (audit.hasOlder) await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(seqs([...events].reverse()))
  })

  it('follows a cursor of an empty page back', async () => {
    let n = 0
    const { audit } = await setup(auditState(), (c) => {
      if (c.path !== '/audit') return undefined
      n++
      if (n === 1) return json({ events: [eventOf(9)], next_cursor: 'd9' })
      if (n === 2) return json({ events: [], next_cursor: 'd4' })
      return json({ events: [eventOf(2)], next_cursor: null })
    })
    await audit.refresh()
    await audit.loadOlder()
    expect(audit.hasOlder).toBe(true)
    await audit.loadOlder()
    expect(seqs(audit.events)).toEqual([9, 2])
    expect(audit.hasOlder).toBe(false)
  })

  it('shows a fixed error and reads again on retry', async () => {
    let fail = true
    const st = auditState({ events: eventsUpTo(3) })
    const { audit } = await setup(st, (c) => (fail && c.path === '/audit' ? apiErr(500, 'internal') : undefined))
    await audit.refresh()
    expect(audit.error).toBe('Something went wrong in the agent service.')
    expect(audit.loaded).toBe(false)
    fail = false
    await audit.refresh()
    expect(audit.error).toBeNull()
    expect(seqs(audit.events)).toEqual([3, 2, 1])
  })

  it('shows the outage banner state only when the status cannot be read either', async () => {
    let down = false
    const { audit } = await setup(auditState(), (c) => (down && ('/audit' === c.path || c.path === '/status') ? gatewayErr() : undefined))
    down = true
    await audit.refresh()
    await flushPromises()
    expect(useAgentStore().serviceDown).toBe(true)
  })

  it('keeps the error to the screen when the sidecar itself answers 502 and the status works', async () => {
    let down = false
    const { audit, server } = await setup(auditState(), (c) => (down && '/audit' === c.path ? apiErr(502, 'upstream_unavailable') : undefined))
    down = true
    const reads = server.count('GET', '/status')
    await audit.refresh()
    await flushPromises()
    expect(useAgentStore().serviceDown).toBe(false)
    expect(server.count('GET', '/status')).toBe(reads)
  })

  it.each([
    ['not a page', { nope: true }],
    ['an ascending cursor on a newest-first page', { events: [eventOf(2)], next_cursor: '2' }],
    ['a malformed cursor', { events: [eventOf(2)], next_cursor: 'd0' }]
  ])('refuses an answer with %s', async (_name, body) => {
    const { audit } = await setup(auditState(), (c) => (c.path === '/audit' ? json(body) : undefined))
    await audit.refresh()
    expect(audit.error).toBe('Something went wrong in the agent service.')
    expect(audit.loaded).toBe(false)
  })

  it('shares one load between concurrent refreshes', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(3) }))
    await Promise.all([audit.refresh(), audit.refresh(), audit.refresh()])
    expect(auditReads(server.calls).length).toBeLessThanOrEqual(2)
    expect(seqs(audit.events)).toEqual([3, 2, 1])
  })
})

describe('useAuditStore: live updates', () => {
  it('reads only the events after the newest one on an activity change and never repeats one', async () => {
    const st = auditState({ events: eventsUpTo(3) })
    const { audit, server, events } = await setup(st)
    await audit.refresh()
    st.events.push(eventOf(4), eventOf(5))
    const before = server.calls.length
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([5, 4, 3, 2, 1])
    expect(auditReads(server.calls.slice(before))).toEqual([forward('3')])
    // the same events once more (a repeated delivery): no duplicates
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([5, 4, 3, 2, 1])
  })

  it('merges a page that repeats loaded events by seq', async () => {
    let n = 0
    const { audit, events } = await setup(auditState(), (c) => {
      if (c.path !== '/audit') return undefined
      n++
      if (n === 1) return json({ events: [eventOf(2), eventOf(1)], next_cursor: null })
      return json({ events: [eventOf(2), eventOf(3)], next_cursor: null })
    })
    await audit.refresh()
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([3, 2, 1])
  })

  it('follows next_cursor for new events, also through empty pages', async () => {
    let n = 0
    const { audit, events } = await setup(auditState(), (c) => {
      if (c.path !== '/audit') return undefined
      n++
      if (n === 1) return json({ events: [eventOf(1)], next_cursor: null })
      if (n === 2) return json({ events: [], next_cursor: '7' })
      return json({ events: [eventOf(8)], next_cursor: null })
    })
    await audit.refresh()
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([8, 1])
  })

  it('never makes up a cursor when next_cursor does not advance, but reads the newest page afresh', async () => {
    let n = 0
    const { audit, events, server } = await setup(auditState(), (c) => {
      if (c.path !== '/audit') return undefined
      n++
      if (n === 1) return json({ events: [eventOf(1)], next_cursor: null })
      if (n === 2) return json({ events: [], next_cursor: '1' }) // the cursor it was sent
      return json({ events: [eventOf(2), eventOf(1)], next_cursor: null })
    })
    await audit.refresh()
    events().onChanged(['activity'])
    await flushPromises()
    // the server's cursors must name existing events: nothing past them is sent
    expect(auditReads(server.calls)).toEqual([newest(), forward('1'), newest()])
    expect(seqs(audit.events)).toEqual([2, 1])
  })

  it('reads new events of a log that was empty', async () => {
    const st = auditState()
    const { audit, events } = await setup(st)
    await audit.refresh()
    st.events.push(eventOf(1))
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([1])
  })

  it('reads the newest page afresh when very many arrived', async () => {
    const st = auditState({ events: eventsUpTo(2) })
    const { audit, events } = await setup(st)
    await audit.refresh()
    st.events = eventsUpTo(5000)
    events().onChanged(['activity'])
    await flushPromises()
    expect(audit.events[0].seq).toBe(5000)
    expect(audit.events).toHaveLength(AUDIT_PAGE)
    expect(audit.hasOlder).toBe(true)
  })

  it('keeps at most MAX_KEPT_EVENTS events, dropping the oldest, and pages back to them', async () => {
    const st = auditState({ events: eventsUpTo(3) })
    const { audit, server, events } = await setup(st)
    await audit.refresh()
    expect(audit.hasOlder).toBe(false)
    let n = 3
    while (n < MAX_KEPT_EVENTS + 300) {
      const to = n + 400
      for (n++; n <= to; n++) st.events.push(eventOf(n))
      n = to
      events().onChanged(['activity'])
      await flushPromises()
    }
    expect(audit.events).toHaveLength(MAX_KEPT_EVENTS)
    expect(audit.events[0].seq).toBe(n)
    expect(seqs(audit.events)).toEqual(range(n, n - MAX_KEPT_EVENTS + 1))
    // the dropped ones are older events again
    expect(audit.hasOlder).toBe(true)
    await audit.loadOlder()
    expect(auditReads(server.calls).at(-1)).toEqual(newest('d' + (n - MAX_KEPT_EVENTS + 1)))
    expect(seqs(audit.events)).toEqual(range(n, n - MAX_KEPT_EVENTS + 1 - AUDIT_PAGE))
  })

  it('can still load older events after a fresh lookup interrupted a running one', async () => {
    const st = auditState({ events: eventsUpTo(1000) })
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const held = 'd' + (1000 - AUDIT_PAGE + 1)
    const { audit, server, events } = await setup(st, (c) =>
      c.path === '/audit' && c.query.get('cursor') === held ? gate.then(() => auditHandler(st)(c)!) : undefined
    )
    await audit.refresh()
    const older = audit.loadOlder()
    await flushPromises()
    // very many new events: the next change reads the newest page afresh
    st.events = eventsUpTo(5000)
    events().onChanged(['activity'])
    await flushPromises()
    expect(audit.events[0].seq).toBe(5000)
    release()
    await older
    expect(audit.events[audit.events.length - 1].seq).toBe(5000 - AUDIT_PAGE + 1) // the stale page was dropped
    expect(audit.olderLoading).toBe(false)
    await audit.loadOlder()
    expect(auditReads(server.calls).at(-1)).toEqual(newest('d' + (5000 - AUDIT_PAGE + 1)))
    expect(audit.events).toHaveLength(2 * AUDIT_PAGE)
  })

  it('ignores other change kinds and does nothing before the first read', async () => {
    const st = auditState({ events: eventsUpTo(2) })
    const { audit, server, events } = await setup(st)
    events().onChanged(['activity'])
    await flushPromises()
    expect(auditReads(server.calls)).toEqual([])
    await audit.refresh()
    const before = server.calls.length
    events().onChanged(['jobs', 'messages', 'settings'])
    await flushPromises()
    expect(auditReads(server.calls.slice(before))).toEqual([])
  })

  it('reads new events on a ready of the event stream', async () => {
    const st = auditState({ events: eventsUpTo(2) })
    const { audit, events } = await setup(st)
    await audit.refresh()
    st.events.push(eventOf(3))
    events().onReady()
    await flushPromises()
    expect(seqs(audit.events)).toEqual([3, 2, 1])
  })

  it('forgets everything when the session ends', async () => {
    const { audit } = await setup(auditState({ events: eventsUpTo(2) }))
    await audit.refresh()
    await audit.setKind('tool_call')
    useAgentStore().stop()
    await flushPromises()
    expect(audit.events).toEqual([])
    expect(audit.loaded).toBe(false)
    expect(audit.kind).toBe('')
  })
})

describe('useAuditStore: kind filter', () => {
  const mixed = () =>
    eventsUpTo(300).map((e) => (e.seq % 10 === 0 ? { ...e, kind: 'memory_edited' } : e))

  it('asks the server for the events of one kind, newest first, and pages back through them', async () => {
    const st = auditState({ events: mixed() })
    const { audit, server } = await setup(st)
    await audit.refresh()
    const before = server.calls.length
    await audit.setKind('memory_edited')
    expect(audit.kind).toBe('memory_edited')
    expect(seqs(audit.events)).toEqual(range(30, 1).map((i) => i * 10))
    expect(audit.hasOlder).toBe(false)
    // the newest seq of the whole log first, then the kind's newest page
    expect(auditReads(server.calls.slice(before))).toEqual([newest(null, null, 1), newest(null, 'memory_edited')])

    await audit.setKind('')
    expect(seqs(audit.events)).toEqual(range(300, 300 - AUDIT_PAGE + 1))
  })

  it('pages back through the events of one kind with its cursors', async () => {
    const st = auditState({ events: eventsUpTo(200).map((e) => (e.seq % 2 === 0 ? { ...e, kind: 'chat_message' } : e)) })
    const { audit, server } = await setup(st)
    await audit.setKind('chat_message')
    expect(audit.events).toHaveLength(AUDIT_PAGE)
    const before = server.calls.length
    await audit.loadOlder()
    expect(auditReads(server.calls.slice(before))).toEqual([newest('d' + (200 - 2 * (AUDIT_PAGE - 1)), 'chat_message')])
    expect(seqs(audit.events)).toEqual(range(100, 100 - 2 * AUDIT_PAGE + 1).map((i) => i * 2))
  })

  it('reads new events of the kind after the newest seq of the log, not after the newest shown', async () => {
    const st = auditState({ events: mixed() })
    const { audit, server, events } = await setup(st)
    await audit.setKind('approval_expired') // none of this kind
    expect(audit.events).toEqual([])
    st.events.push(eventOf(301), eventOf(302, { kind: 'approval_expired' }))
    const before = server.calls.length
    events().onChanged(['activity'])
    await flushPromises()
    expect(auditReads(server.calls.slice(before))).toEqual([forward('300', 'approval_expired')])
    expect(seqs(audit.events)).toEqual([302])
  })

  it('drops the answer for the kind shown before', async () => {
    const st = auditState({ events: mixed() })
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const { audit } = await setup(st, (c) =>
      c.path === '/audit' && c.query.get('kind') === 'memory_edited'
        ? gate.then(() => auditHandler(st)(c)!)
        : undefined
    )
    const first = audit.setKind('memory_edited')
    await flushPromises()
    await audit.setKind('tool_call')
    release()
    await first
    await flushPromises()
    expect(audit.kind).toBe('tool_call')
    expect(audit.events.every((e) => e.kind === 'tool_call')).toBe(true)
    expect(audit.events).toHaveLength(AUDIT_PAGE)
    expect(audit.loading).toBe(false)
  })
})

describe('useAuditStore: verify', () => {
  it('reports an intact chain with the number of verified events', async () => {
    const { audit } = await setup(auditState({ events: eventsUpTo(7) }))
    await audit.verify()
    expect(audit.verifyResult).toBe('ok')
    expect(audit.verifyChecked).toBe(7)
    expect(audit.verifyBrokenAt).toBeNull()
    expect(audit.verifyError).toBeNull()
  })

  it('reports where the chain breaks', async () => {
    const st = auditState({
      verify: json({ ok: false, message: 'the audit chain is broken', limitation: 'x', checked: 3, broken_at: 4 })
    })
    const { audit } = await setup(st)
    await audit.verify()
    expect(audit.verifyResult).toBe('broken')
    expect(audit.verifyChecked).toBe(3)
    expect(audit.verifyBrokenAt).toBe(4)
  })

  it('reports a broken chain without a position', async () => {
    const st = auditState({ verify: json({ ok: false, message: 'm', limitation: 'x', checked: 0 }) })
    const { audit } = await setup(st)
    await audit.verify()
    expect(audit.verifyResult).toBe('broken')
    expect(audit.verifyBrokenAt).toBeNull()
  })

  it.each([
    ['a non-boolean ok', { ok: 'yes', limitation: 'x', checked: 1 }],
    ['no count', { ok: true, limitation: 'x' }],
    ['a negative count', { ok: true, limitation: 'x', checked: -1 }],
    ['a fractional position', { ok: false, limitation: 'x', checked: 1, broken_at: 1.5 }]
  ])('refuses an answer with %s', async (_name, body) => {
    const { audit } = await setup(auditState({ verify: json(body) }))
    await audit.verify()
    expect(audit.verifyResult).toBeNull()
    expect(audit.verifyChecked).toBeNull()
    expect(audit.verifyError).toBe('Something went wrong in the agent service.')
  })

  it('shows when to try again after a 429', async () => {
    const { audit } = await setup(auditState({ verify: errWithRetry(429, 'rate_limited', 5) }))
    await audit.verify()
    expect(audit.verifyResult).toBeNull()
    expect(audit.verifyError).toBe('A check is already running. Try again in 5s.')
  })

  it('runs one check at a time', async () => {
    const { audit, server } = await setup()
    await Promise.all([audit.verify(), audit.verify()])
    expect(server.count('GET', '/audit/verify')).toBe(1)
  })
})

describe('useAuditStore: export', () => {
  it('downloads the export with the bearer header and no token in the URL', async () => {
    const st = auditState({ events: eventsUpTo(2) })
    const { audit, server } = await setup(st)
    await audit.exportLog()
    const call = server.fetch.mock.calls.find(([u]) => String(u).includes('/audit/export'))!
    expect(String(call[0])).toBe('/agent-api/v1/audit/export')
    expect(String(call[0])).not.toContain('test-token')
    expect((call[1]?.headers as Record<string, string>).Authorization).toBe('Bearer test-token')
    expect(saved).toHaveLength(1)
    expect(saved[0].filename).toBe('audit-0a1b2c3d.jsonl')
    expect((await saved[0].blob.text()).split('\n')).toHaveLength(3)
    expect(audit.exporting).toBe(false)
    expect(audit.exportError).toBeNull()
    expect(audit.exportBytes).toBeGreaterThan(0)
  })

  it('shows when to try again after a 429 and saves nothing', async () => {
    const { audit } = await setup(auditState({ exportRes: () => errWithRetry(429, 'rate_limited', 5) }))
    await audit.exportLog()
    expect(saved).toHaveLength(0)
    expect(audit.exportError).toBe('An export is already running. Try again in 5s.')
  })

  it('says the transfer broke off and saves nothing', async () => {
    const { audit } = await setup(
      auditState({ exportRes: () => new Response('{"seq":1}\n{"se', { status: 200 }) })
    )
    await audit.exportLog()
    expect(saved).toHaveLength(0)
    expect(audit.exportError).toBe('The export was interrupted, so no file was saved. Try again.')
  })

  it('stops without saving or an error when aborted', async () => {
    let push!: ReadableStreamDefaultController<Uint8Array>
    const body = new ReadableStream<Uint8Array>({
      start(c) {
        push = c
      }
    })
    const { audit } = await setup(auditState({ exportRes: () => new Response(body, { status: 200 }) }))
    const p = audit.exportLog()
    await flushPromises()
    expect(audit.exporting).toBe(true)
    audit.abortExport()
    try {
      push.enqueue(new TextEncoder().encode('{"seq":1}\n'))
    } catch {
      // the reader was cancelled: the stream is closed
    }
    await p
    expect(saved).toHaveLength(0)
    expect(audit.exportError).toBeNull()
    expect(audit.exporting).toBe(false)
  })

  it('runs one export at a time', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(1) }))
    await Promise.all([audit.exportLog(), audit.exportLog()])
    expect(server.count('GET', '/audit/export')).toBe(1)
    expect(saved).toHaveLength(1)
  })
})
