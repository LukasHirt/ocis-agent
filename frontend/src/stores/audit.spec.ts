import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, json } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import { auditHandler, auditReads, auditState, eventOf, eventsUpTo, type AuditState } from '../test/audit'
import { errWithRetry } from '../test/chat'
import { AUDIT_PAGE, useAuditStore } from './audit'
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
  it('reads a short log in one request, newest first', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(5) }))
    await audit.refresh()
    expect(seqs(audit.events)).toEqual([5, 4, 3, 2, 1])
    expect(audit.loaded).toBe(true)
    expect(audit.hasOlder).toBe(false)
    expect(auditReads(server.calls)).toEqual([{ cursor: null, limit: String(AUDIT_PAGE) }])
  })

  it('shows an empty log as loaded and empty', async () => {
    const { audit } = await setup()
    await audit.refresh()
    expect(audit.events).toEqual([])
    expect(audit.loaded).toBe(true)
    expect(audit.hasOlder).toBe(false)
  })

  it('finds the newest events of a long log with few requests and pages back to the start', async () => {
    const st = auditState({ events: eventsUpTo(1000) })
    const { audit, server } = await setup(st)
    await audit.refresh()
    expect(seqs(audit.events)).toEqual(range(1000, 1000 - AUDIT_PAGE + 1))
    expect(audit.hasOlder).toBe(true)
    expect(auditReads(server.calls).length).toBeLessThan(20)

    const before = server.calls.length
    await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(range(1000, 1000 - 2 * AUDIT_PAGE + 1))
    expect(auditReads(server.calls.slice(before))).toEqual([
      { cursor: String(1000 - 2 * AUDIT_PAGE), limit: String(AUDIT_PAGE) }
    ])
    while (audit.hasOlder) await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(range(1000, 1))
  })

  it('needs few requests even for a very long log', async () => {
    const { audit, server } = await setup(auditState({ events: eventsUpTo(200_000) }))
    await audit.refresh()
    expect(audit.events[0].seq).toBe(200_000)
    expect(audit.events).toHaveLength(AUDIT_PAGE)
    expect(auditReads(server.calls).length).toBeLessThan(45)
  })

  it('copes with gaps in the sequence numbers', async () => {
    const events = eventsUpTo(400).filter((e) => e.seq % 3 !== 0 && (e.seq < 300 || e.seq > 380))
    const { audit } = await setup(auditState({ events }))
    await audit.refresh()
    expect(audit.events[0].seq).toBe(400)
    while (audit.hasOlder) await audit.loadOlder()
    expect(seqs(audit.events)).toEqual(seqs([...events].reverse()))
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

  it('refuses an answer that is not a page', async () => {
    const { audit } = await setup(auditState(), (c) => (c.path === '/audit' ? json({ nope: true }) : undefined))
    await audit.refresh()
    expect(audit.error).not.toBeNull()
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
    expect(auditReads(server.calls.slice(before))).toEqual([{ cursor: '3', limit: String(AUDIT_PAGE) }])
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
      if (n === 1) return json({ events: [eventOf(1), eventOf(2)], next_cursor: null })
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

  it('reads new events of a log that was empty', async () => {
    const st = auditState()
    const { audit, events } = await setup(st)
    await audit.refresh()
    st.events.push(eventOf(1))
    events().onChanged(['activity'])
    await flushPromises()
    expect(seqs(audit.events)).toEqual([1])
  })

  it('jumps to the newest events when very many arrived', async () => {
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

  it('can still load older events after a fresh lookup interrupted a running one', async () => {
    const st = auditState({ events: eventsUpTo(1000) })
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const held = String(1000 - 2 * AUDIT_PAGE)
    const { audit, server, events } = await setup(st, (c) =>
      c.path === '/audit' && c.query.get('cursor') === held ? gate.then(() => auditHandler(st)(c)!) : undefined
    )
    await audit.refresh()
    const older = audit.loadOlder()
    await flushPromises()
    // very many new events: the next change looks up the newest page afresh
    st.events = eventsUpTo(5000)
    events().onChanged(['activity'])
    await flushPromises()
    expect(audit.events[0].seq).toBe(5000)
    release()
    await older
    expect(audit.events[audit.events.length - 1].seq).toBe(5000 - AUDIT_PAGE + 1) // the stale page was dropped
    expect(audit.olderLoading).toBe(false)
    await audit.loadOlder()
    expect(auditReads(server.calls).at(-1)).toEqual({ cursor: String(5000 - 2 * AUDIT_PAGE), limit: String(AUDIT_PAGE) })
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
    audit.setKind('tool_call')
    useAgentStore().stop()
    await flushPromises()
    expect(audit.events).toEqual([])
    expect(audit.loaded).toBe(false)
    expect(audit.kind).toBe('')
  })
})

describe('useAuditStore: kind filter', () => {
  it('filters the loaded events by kind', async () => {
    const events = [eventOf(1, { kind: 'tool_call' }), eventOf(2, { kind: 'memory_edited' }), eventOf(3, { kind: 'tool_call' })]
    const { audit } = await setup(auditState({ events }))
    await audit.refresh()
    expect(seqs(audit.shown)).toEqual([3, 2, 1])
    audit.setKind('tool_call')
    expect(seqs(audit.shown)).toEqual([3, 1])
    audit.setKind('')
    expect(seqs(audit.shown)).toEqual([3, 2, 1])
  })
})

describe('useAuditStore: verify', () => {
  it('reports an intact chain', async () => {
    const { audit } = await setup()
    await audit.verify()
    expect(audit.verifyResult).toBe('ok')
    expect(audit.verifyError).toBeNull()
  })

  it('reports a broken chain', async () => {
    const st = auditState({
      verify: json({ ok: false, message: 'the audit chain is broken', limitation: 'x' })
    })
    const { audit } = await setup(st)
    await audit.verify()
    expect(audit.verifyResult).toBe('broken')
  })

  it('refuses an answer without a boolean ok', async () => {
    const { audit } = await setup(auditState({ verify: json({ ok: 'yes', limitation: 'x' }) }))
    await audit.verify()
    expect(audit.verifyResult).toBeNull()
    expect(audit.verifyError).not.toBeNull()
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
