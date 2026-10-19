import { afterEach, describe, expect, it, vi } from 'vitest'
import { ApiError } from '../api/errors'
import type { EventClient, EventClientOptions, Timers } from '../api/events'
import { apiErr, approvalOf, json, settingsOf, statusOf } from '../test/fixtures'
import { makePlatform, type FakePlatform } from '../test/platform'
import { FULL_REFRESH_MIN_MS, useAgentStore } from './agent'

const gettext = (s: string) => s

type Handler = (url: URL, init: RequestInit) => Response | Promise<Response>

// fakeServer answers by "METHOD /path" (path below the backend URL) and
// records every request.
const fakeServer = (handlers: Record<string, Handler>) => {
  const calls: Array<{ key: string; url: URL; body: unknown }> = []
  const fetch = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = new URL(String(input), 'https://ocis.test')
    const key = `${init.method ?? 'GET'} ${url.pathname.replace('/agent-api/v1', '')}`
    calls.push({ key, url, body: init.body ? JSON.parse(String(init.body)) : undefined })
    const h = handlers[key]
    if (!h) return apiErr(404, 'not_found')
    return h(url, init)
  })
  return { fetch, calls, count: (key: string) => calls.filter((c) => c.key === key).length }
}

const defaultHandlers = (over: Record<string, Handler> = {}): Record<string, Handler> => ({
  'GET /status': () => json(statusOf()),
  'GET /settings': () => json(settingsOf()),
  'GET /approvals': (url) =>
    url.searchParams.get('state') === 'unknown'
      ? json({ approvals: [approvalOf('u1', { state: 'unknown', needs_verification: true })], next_cursor: null })
      : json({ approvals: [approvalOf('a1'), approvalOf('a2', { job_ended: true })], next_cursor: null }),
  ...over
})

// a fake event client that records its options
const fakeEvents = () => {
  type Fake = EventClient & { start: ReturnType<typeof vi.fn<() => void>>; stop: ReturnType<typeof vi.fn<() => void>> }
  const made: { opts?: EventClientOptions; client?: Fake } = {}
  const create = (opts: EventClientOptions): EventClient => {
    made.opts = opts
    const client: Fake = { start: vi.fn<() => void>(), stop: vi.fn<() => void>() }
    made.client = client
    return client
  }
  return { made, create }
}

// manual timers
const fakeTimers = () => {
  let next = 1
  const pending = new Map<number, () => void>()
  const timers: Timers = {
    setTimeout: (fn) => {
      const h = next++
      pending.set(h, fn)
      return h
    },
    clearTimeout: (h) => {
      pending.delete(h as number)
    }
  }
  const fire = () => {
    const fns = [...pending.values()]
    pending.clear()
    fns.forEach((f) => f())
  }
  return { timers, fire, size: () => pending.size }
}

const flush = async () => {
  for (let i = 0; i < 20; i++) await Promise.resolve()
  await new Promise((r) => setTimeout(r, 0))
}

const setup = (handlers = defaultHandlers()) => {
  const server = fakeServer(handlers)
  let clock = Date.UTC(2026, 0, 1, 12, 0, 0)
  const platform: FakePlatform = makePlatform({ fetch: server.fetch as unknown as typeof fetch })
  platform.now.mockImplementation(() => clock)
  const events = fakeEvents()
  const t = fakeTimers()
  const store = useAgentStore()
  const start = () => store.start({ platform, gettext, createEvents: events.create, timers: t.timers })
  return {
    server,
    platform,
    events,
    timers: t,
    store,
    start,
    advance: (ms: number) => {
      clock += ms
    }
  }
}

let current: ReturnType<typeof setup> | undefined
afterEach(() => {
  current?.store.stop()
  current = undefined
})

describe('useAgentStore', () => {
  it('starts the event client and refreshes everything at start', async () => {
    const s = (current = setup())
    s.start()
    expect(s.events.made.client?.start).toHaveBeenCalledTimes(1)
    await flush()
    expect(s.store.status?.enrolled).toBe(true)
    expect(s.store.settings?.sweep_interval).toBe('15m0s')
    expect(s.store.approvals.pending.map((a) => a.id)).toEqual(['a1', 'a2'])
    expect(s.store.approvals.verify.map((a) => a.id)).toEqual(['u1'])
    expect(s.store.approvals.counts).toEqual({ pending: 2, live: 1, verify: 1 })
    expect(s.store.loading).toEqual({ status: false, approvals: false, settings: false })
    expect(s.store.error).toBeNull()
    expect(s.store.lastRefreshed).toBe(s.platform.now())
  })

  it('start twice is a no-op', async () => {
    const s = (current = setup())
    s.start()
    const first = s.events.made.client
    s.start()
    expect(s.events.made.client).toBe(first)
    await flush()
    expect(s.server.count('GET /status')).toBe(1)
  })

  it('reuses a refresh that is already in flight', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const s = (current = setup(
      defaultHandlers({
        'GET /status': async () => {
          await gate
          return json(statusOf())
        }
      })
    ))
    s.start()
    const a = s.store.refreshStatus()
    const b = s.store.refreshStatus()
    expect(s.store.loading.status).toBe(true)
    release()
    await Promise.all([a, b])
    expect(s.server.count('GET /status')).toBe(1)
  })

  it('follows next_cursor for the pending approvals', async () => {
    const s = (current = setup(
      defaultHandlers({
        'GET /approvals': (url) => {
          if (url.searchParams.get('state') === 'unknown') return json({ approvals: [], next_cursor: null })
          return url.searchParams.get('cursor') === 'p2'
            ? json({ approvals: [approvalOf('b')], next_cursor: null })
            : json({ approvals: [approvalOf('a')], next_cursor: 'p2' })
        }
      })
    ))
    s.start()
    await flush()
    expect(s.store.approvals.pending.map((a) => a.id)).toEqual(['a', 'b'])
    expect(s.store.approvals.complete).toBe(true)
  })

  it('keeps the other resources when one fails and shows a fixed message', async () => {
    let fail = false
    const s = (current = setup(
      defaultHandlers({
        'GET /settings': () => (fail ? apiErr(503, 'unavailable') : json(settingsOf()))
      })
    ))
    s.start()
    await flush()
    fail = true
    s.advance(FULL_REFRESH_MIN_MS)
    await s.store.refreshAll()
    expect(s.store.settings?.sweep_interval).toBe('15m0s') // not blanked
    expect(s.store.status?.enrolled).toBe(true)
    expect(s.store.approvals.pending).toHaveLength(2)
    expect(s.store.errors.settings).toBe('The agent service is not reachable right now. Try again later.')
    expect(s.store.errors.status).toBeNull()
    expect(s.store.error).toBe(s.store.errors.settings)
    expect(s.store.error).not.toContain('server text')
  })

  it('clears an error after a successful retry', async () => {
    let fail = true
    const s = (current = setup(
      defaultHandlers({ 'GET /status': () => (fail ? apiErr(500, 'internal') : json(statusOf())) })
    ))
    s.start()
    await flush()
    expect(s.store.errors.status).toBe('Something went wrong in the agent service.')
    fail = false
    await s.store.refreshStatus()
    expect(s.store.errors.status).toBeNull()
  })

  it('maps change kinds to the resources they imply', async () => {
    const s = (current = setup())
    s.start()
    await flush()
    const before = (k: string) => s.server.count(k)
    const st = before('GET /status')
    const ap = before('GET /approvals')
    const se = before('GET /settings')

    await s.events.made.opts!.onChanged(['jobs'])
    await flush()
    expect(s.server.count('GET /status')).toBe(st + 1)
    expect(s.server.count('GET /approvals')).toBe(ap + 2) // pending and unknown
    expect(s.server.count('GET /settings')).toBe(se)

    await s.events.made.opts!.onChanged(['settings'])
    await flush()
    expect(s.server.count('GET /settings')).toBe(se + 1)
    expect(s.server.count('GET /status')).toBe(st + 2)
    expect(s.server.count('GET /approvals')).toBe(ap + 2)

    await s.events.made.opts!.onChanged(['activity'])
    await flush()
    expect(s.server.count('GET /approvals')).toBe(ap + 4)

    const total = s.server.calls.length
    await s.events.made.opts!.onChanged(['messages'])
    await flush()
    expect(s.server.calls.length).toBe(total)
  })

  it('refreshes once more after a change that arrives during a refresh', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let settingsCalls = 0
    const s = (current = setup(
      defaultHandlers({
        'GET /settings': async (): Promise<Response> => {
          const n = settingsCalls++
          await gate
          return json(settingsOf({ quiet_hours: String(n + 1) }))
        }
      })
    ))
    s.start()
    await flush()
    expect(s.server.count('GET /settings')).toBe(1)
    const p1 = s.events.made.opts!.onChanged(['settings'])
    const p2 = s.events.made.opts!.onChanged(['settings'])
    await flush()
    expect(s.server.count('GET /settings')).toBe(1) // still waiting for the first
    release()
    await Promise.all([p1, p2])
    await flush()
    // the start refresh, then one coalesced follow-up for both changes
    expect(s.server.count('GET /settings')).toBe(2)
    expect(s.store.settings?.quiet_hours).toBe('2')
  })

  it('rate-limits full refreshes to one per second', async () => {
    const s = (current = setup())
    s.start()
    await flush()
    expect(s.server.count('GET /status')).toBe(1)
    const p = s.events.made.opts!.onReady()
    const q = s.events.made.opts!.onPoll()
    await flush()
    expect(s.server.count('GET /status')).toBe(1)
    expect(s.timers.size()).toBe(1)
    s.advance(FULL_REFRESH_MIN_MS)
    s.timers.fire()
    await Promise.all([p, q])
    await flush()
    expect(s.server.count('GET /status')).toBe(2)
  })

  it('reports the connection state of the event client', async () => {
    const s = (current = setup())
    s.start()
    expect(s.store.connection).toBe('streaming')
    await s.events.made.opts!.onState!('polling')
    expect(s.store.connection).toBe('polling')
    await s.events.made.opts!.onState!('offline')
    expect(s.store.connection).toBe('offline')
  })

  it('refreshes everything on every poll in polling mode', async () => {
    const s = (current = setup())
    s.start()
    await flush()
    await s.events.made.opts!.onState!('polling')
    s.advance(15_000)
    await s.events.made.opts!.onPoll()
    await flush()
    expect(s.server.count('GET /status')).toBe(2)
    expect(s.server.count('GET /settings')).toBe(2)
  })

  it('decide sends the given args_hash, drops the item and refreshes', async () => {
    const s = (current = setup(
      defaultHandlers({
        'POST /approvals/a1/decision': () => json(approvalOf('a1', { state: 'approved' }))
      })
    ))
    s.start()
    await flush()
    const st = s.server.count('GET /status')
    const res = s.store.decide('a1', 'approve', 'h-a1')
    expect(s.store.approvals.pending.map((a) => a.id)).toEqual(['a1', 'a2'])
    await res
    const call = s.server.calls.find((c) => c.key === 'POST /approvals/a1/decision')
    expect(call?.body).toEqual({ decision: 'approve', args_hash: 'h-a1' })
    expect(s.server.count('GET /status')).toBe(st + 1)
  })

  it('decide removes the decided item even before the refresh answers', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let slow = false
    const s = (current = setup(
      defaultHandlers({
        'POST /approvals/a1/decision': () => {
          slow = true
          return json(approvalOf('a1', { state: 'rejected' }))
        },
        'GET /approvals': async (url) => {
          if (slow) await gate
          return url.searchParams.get('state') === 'unknown'
            ? json({ approvals: [], next_cursor: null })
            : json({ approvals: [approvalOf('a1'), approvalOf('a2')], next_cursor: null })
        }
      })
    ))
    s.start()
    await flush()
    const p = s.store.decide('a1', 'reject', 'h-a1')
    await flush()
    expect(s.store.approvals.pending.map((a) => a.id)).toEqual(['a2'])
    expect(s.store.approvals.counts.pending).toBe(1)
    release()
    await p
  })

  it('decide refreshes and rethrows on a stale decision', async () => {
    const s = (current = setup(
      defaultHandlers({ 'POST /approvals/a1/decision': () => apiErr(409, 'stale_arguments') })
    ))
    s.start()
    await flush()
    const ap = s.server.count('GET /approvals')
    const err = await s.store.decide('a1', 'approve', 'h-a1').catch((e) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect((err as ApiError).code).toBe('stale_arguments')
    expect(s.server.count('GET /approvals')).toBe(ap + 2)
  })

  it('bulkDecide sends every item and drops the decided ones', async () => {
    const s = (current = setup(
      defaultHandlers({
        'POST /approvals/bulk-decision': () =>
          json({ approvals: [approvalOf('a1', { state: 'approved' }), approvalOf('a2', { state: 'approved' })] })
      })
    ))
    s.start()
    await flush()
    await s.store.bulkDecide('approve', [
      { id: 'a1', args_hash: 'h-a1' },
      { id: 'a2', args_hash: 'h-a2' }
    ])
    const call = s.server.calls.find((c) => c.key === 'POST /approvals/bulk-decision')
    expect(call?.body).toEqual({
      decision: 'approve',
      items: [
        { id: 'a1', args_hash: 'h-a1' },
        { id: 'a2', args_hash: 'h-a2' }
      ]
    })
  })

  it('saveSettings stores the server answer and refreshes the status', async () => {
    const s = (current = setup(
      defaultHandlers({ 'PUT /settings': () => json(settingsOf({ quiet_hours: '22:00-07:00' })) })
    ))
    s.start()
    await flush()
    const st = s.server.count('GET /status')
    await s.store.saveSettings({ quiet_hours: '22:00-07:00' })
    expect(s.store.settings?.quiet_hours).toBe('22:00-07:00')
    expect(s.server.count('GET /status')).toBe(st + 1)
  })

  it('pause, resume and enrol store the returned status', async () => {
    const s = (current = setup(
      defaultHandlers({
        'POST /pause': () => json(statusOf({ paused: true, pause_reason: 'user' })),
        'POST /resume': () => json(statusOf()),
        'POST /enroll': () => json(statusOf({ user: { id: 'u1', name: 'New', login: 'einstein' } }))
      })
    ))
    s.start()
    await flush()
    await s.store.pause()
    expect(s.store.status?.pause_reason).toBe('user')
    await s.store.resume('user')
    expect(s.server.calls.find((c) => c.key === 'POST /resume')?.body).toEqual({ reason: 'user' })
    expect(s.store.status?.paused).toBe(false)
    await s.store.enrol()
    expect(s.store.status?.user.name).toBe('New')
  })

  it('unenrol sends delete_memory and refreshes everything', async () => {
    let enrolled = true
    const s = (current = setup(
      defaultHandlers({
        'GET /status': () => json(statusOf({ enrolled })),
        'POST /unenroll': () => {
          enrolled = false
          return new Response(null, { status: 204 })
        }
      })
    ))
    s.start()
    await flush()
    const counts = ['GET /status', 'GET /settings', 'GET /approvals'].map((k) => s.server.count(k))
    await s.store.unenrol(true)
    expect(s.server.calls.find((c) => c.key === 'POST /unenroll')?.body).toEqual({ delete_memory: true })
    expect(s.store.status?.enrolled).toBe(false)
    const after = ['GET /status', 'GET /settings', 'GET /approvals'].map((k) => s.server.count(k))
    after.forEach((n, i) => expect(n).toBeGreaterThan(counts[i]))
    await s.store.unenrol(false)
    expect(s.server.calls.filter((c) => c.key === 'POST /unenroll')[1]?.body).toEqual({ delete_memory: false })
  })

  it('a failed unenrol rethrows and still refreshes the status', async () => {
    const s = (current = setup(defaultHandlers({ 'POST /unenroll': () => apiErr(502, 'upstream_unavailable') })))
    s.start()
    await flush()
    const st = s.server.count('GET /status')
    await expect(s.store.unenrol(true)).rejects.toMatchObject({ code: 'upstream_unavailable' })
    expect(s.server.count('GET /status')).toBe(st + 1)
  })

  it('the token actions call their endpoints and refresh the status', async () => {
    const s = (current = setup(
      defaultHandlers({
        'POST /token/renew': () => json({ renewed: true, old_cleaned: true }),
        'DELETE /token': () => json({ revoked: 2 }),
        'GET /token/revoke-all/preview': () =>
          json({ tokens: [{ label: 'x', created_at: null, expires_at: null, ours: false }] }),
        'POST /token/revoke-all': () => json({ revoked: 3 })
      })
    ))
    s.start()
    await flush()
    let st = s.server.count('GET /status')
    expect(await s.store.renewToken()).toEqual({ renewed: true, old_cleaned: true })
    expect(s.server.count('GET /status')).toBe(++st)
    expect(await s.store.revokeToken()).toEqual({ revoked: 2 })
    expect(s.server.count('GET /status')).toBe(++st)
    expect((await s.store.previewRevokeAll()).tokens).toHaveLength(1)
    expect(s.server.count('GET /status')).toBe(st)
    expect(await s.store.revokeAll()).toEqual({ revoked: 3 })
    expect(s.server.calls.find((c) => c.key === 'POST /token/revoke-all')?.body).toEqual({ confirm: true })
    expect(s.server.count('GET /status')).toBe(++st)
  })

  it('a failed token action rethrows and still refreshes the status', async () => {
    const s = (current = setup(defaultHandlers({ 'DELETE /token': () => apiErr(502, 'upstream_unavailable') })))
    s.start()
    await flush()
    const st = s.server.count('GET /status')
    await expect(s.store.revokeToken()).rejects.toMatchObject({ code: 'upstream_unavailable' })
    expect(s.server.count('GET /status')).toBe(st + 1)
  })

  it('an older status refresh does not overwrite the status an action returned', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let slow = false
    const s = (current = setup(
      defaultHandlers({
        'GET /status': async () => {
          if (slow) await gate
          return json(statusOf())
        },
        'POST /pause': () => json(statusOf({ paused: true, pause_reason: 'user' }))
      })
    ))
    s.start()
    await flush()
    slow = true
    const r = s.store.refreshStatus()
    await s.store.pause()
    release()
    await r
    expect(s.store.status?.paused).toBe(true)
  })

  it('stop ends the event client and ignores later answers', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const s = (current = setup(
      defaultHandlers({
        'GET /status': async () => {
          await gate
          return json(statusOf())
        }
      })
    ))
    s.start()
    const client = s.events.made.client!
    s.store.stop()
    expect(client.stop).toHaveBeenCalledTimes(1)
    release()
    await flush()
    expect(s.store.status).toBeNull()
    expect(s.store.loading.status).toBe(false)
    await s.events.made.opts!.onReady()
    await flush()
    expect(s.store.status).toBeNull()
  })

  it('a ready during a full refresh queues one more full refresh', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    let statusCalls = 0
    const s = (current = setup(
      defaultHandlers({
        'GET /status': async (): Promise<Response> => {
          const n = ++statusCalls
          if (n === 1) await gate
          return json(statusOf({ pending_approvals: n }))
        }
      })
    ))
    s.start() // the start refresh, held at the status
    await flush()
    const r1 = s.events.made.opts!.onReady()
    const r2 = s.events.made.opts!.onReady()
    s.advance(FULL_REFRESH_MIN_MS)
    release()
    await flush()
    s.timers.fire() // in case the follow-up was deferred
    await Promise.all([r1, r2])
    await flush()
    expect(s.server.count('GET /status')).toBe(2) // one coalesced follow-up
    expect(s.store.status?.pending_approvals).toBe(2)
  })

  it('a poll during a full refresh reuses it', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const s = (current = setup(
      defaultHandlers({
        'GET /status': async () => {
          await gate
          return json(statusOf())
        }
      })
    ))
    s.start()
    await flush()
    const p = s.events.made.opts!.onPoll()
    release()
    await p
    await flush()
    expect(s.server.count('GET /status')).toBe(1)
  })

  it('stop clears the data, and a new start begins from scratch', async () => {
    let fail = true
    const s = (current = setup(
      defaultHandlers({ 'GET /settings': () => (fail ? apiErr(500, 'internal') : json(settingsOf())) })
    ))
    s.start()
    await flush()
    expect(s.store.status).not.toBeNull()
    expect(s.store.errors.settings).not.toBeNull()
    s.store.stop()
    expect(s.store.status).toBeNull()
    expect(s.store.settings).toBeNull()
    expect(s.store.approvals.pending).toEqual([])
    expect(s.store.approvals.counts).toEqual({ pending: 0, live: 0, verify: 0 })
    expect(s.store.errors).toEqual({ status: null, approvals: null, settings: null })
    expect(s.store.lastRefreshed).toBeNull()
    fail = false
    s.start()
    expect(s.store.status).toBeNull() // nothing of the previous session shows
    await flush()
    expect(s.store.status?.enrolled).toBe(true)
    expect(s.store.settings).not.toBeNull()
    expect(s.store.error).toBeNull()
    // the rate limit of the old session does not delay the new one
    expect(s.server.count('GET /status')).toBe(2)
  })

  it('stop cancels a deferred full refresh', async () => {
    const s = (current = setup())
    s.start()
    await flush()
    const p = s.events.made.opts!.onPoll()
    expect(s.timers.size()).toBe(1)
    s.store.stop()
    await p
    expect(s.timers.size()).toBe(0)
    expect(s.server.count('GET /status')).toBe(1)
  })

  it('refreshes do nothing before start', async () => {
    const s = (current = setup())
    await s.store.refreshAll()
    expect(s.server.calls).toHaveLength(0)
  })
})
