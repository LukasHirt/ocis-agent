import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { makePlatform } from '../test/platform'
import { createClient } from './client'
import { ApiError } from './errors'
import {
  BACKOFF_BASE_MS,
  BACKOFF_CAP_MS,
  FIRST_READY_MS,
  IDLE_MS,
  POLL_EVERY_MS,
  REFRESH_EVERY_MS,
  STABLE_AFTER_MS,
  STREAM_RETRY_CAP_MS,
  STREAM_RETRY_MS,
  VISIBLE_JITTER_MS,
  createEventClient,
  type EventClientOptions,
  type Visibility
} from './events'

const enc = new TextEncoder()

class FakeStream {
  ctrl!: ReadableStreamDefaultController<Uint8Array>
  cancelled = false
  readonly stream: ReadableStream<Uint8Array>
  constructor() {
    this.stream = new ReadableStream<Uint8Array>({
      start: (c) => {
        this.ctrl = c
      },
      cancel: () => {
        this.cancelled = true
      }
    })
  }
  send(text: string) {
    if (!this.cancelled) this.ctrl.enqueue(enc.encode(text))
  }
  ready() {
    this.send('event: ready\ndata: {"v":1}\n\n')
  }
  changed(data: string) {
    this.send('event: changed\ndata: ' + data + '\n\n')
  }
  bye() {
    this.send('event: bye\ndata: {"reason":"lifetime"}\n\n')
  }
  close() {
    try {
      this.ctrl.close()
    } catch {
      // already closed or cancelled
    }
  }
}

interface Call {
  signal: AbortSignal
  resolve(r: Response): void
  reject(e: unknown): void
}

const flush = async () => {
  for (let i = 0; i < 5; i++) await vi.advanceTimersByTimeAsync(0)
}

const advance = async (ms: number) => {
  await vi.advanceTimersByTimeAsync(ms)
  await flush()
}

class FakeVisibility implements Visibility {
  isHidden = false
  listener: (() => void) | undefined
  hidden() {
    return this.isHidden
  }
  subscribe(cb: () => void) {
    this.listener = cb
    return () => {
      this.listener = undefined
    }
  }
  set(hidden: boolean) {
    this.isHidden = hidden
    this.listener?.()
  }
}

// untilCall advances the clock in 100 ms steps until fn gets a new call
const untilCall = async (fn: { mock: { calls: unknown[] } }) => {
  const before = fn.mock.calls.length
  for (let i = 0; i < 1000 && fn.mock.calls.length === before; i++) await advance(100)
}

const harness = (over: Partial<EventClientOptions> = {}) => {
  const random = vi.fn(() => 0.5)
  const platform = makePlatform({ now: () => Date.now(), random })
  const calls: Call[] = []
  const openEvents = vi.fn(
    (signal: AbortSignal) =>
      new Promise<Response>((resolve, reject) => {
        calls.push({ signal, resolve, reject })
      })
  )
  const onReady = vi.fn()
  const onChanged = vi.fn()
  const onState = vi.fn()
  const onPoll = vi.fn()
  const visibility = new FakeVisibility()
  const ec = createEventClient({
    platform,
    client: { openEvents },
    onReady,
    onChanged,
    onState,
    onPoll,
    visibility,
    ...over
  })
  // accept answers call i (default: the latest) with a fresh stream
  const accept = async (i = calls.length - 1) => {
    const s = new FakeStream()
    calls[i].resolve(new Response(s.stream, { status: 200 }))
    await flush()
    return s
  }
  const fail = async (e: unknown, i = calls.length - 1) => {
    calls[i].reject(e)
    await flush()
  }
  const states = () => onState.mock.calls.map((c) => c[0])
  return { ec, platform, random, calls, openEvents, onReady, onChanged, onState, onPoll, visibility, accept, fail, states }
}

let consoleSpies: ReturnType<typeof vi.spyOn>[] = []

beforeEach(() => {
  vi.useFakeTimers()
  consoleSpies = (['log', 'info', 'warn', 'error', 'debug'] as const).map((m) =>
    vi.spyOn(console, m).mockImplementation(() => {})
  )
})

afterEach(() => {
  for (const s of consoleSpies) {
    expect(s).not.toHaveBeenCalled()
    s.mockRestore()
  }
  vi.useRealTimers()
})

describe('createEventClient: events', () => {
  it('connects once, refetches on ready and reports streaming', async () => {
    const h = harness()
    h.ec.start()
    h.ec.start()
    await flush()
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    const s = await h.accept()
    expect(h.onReady).not.toHaveBeenCalled()
    s.ready()
    await flush()
    expect(h.onReady).toHaveBeenCalledTimes(1)
    expect(h.states()).toEqual(['streaming'])
    h.ec.stop()
  })

  it('passes known kinds of changed events and ignores garbage', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    s.changed('{"kinds":["jobs","bogus","activity","jobs"]}')
    s.changed('not json')
    s.changed('{"kinds":"jobs"}')
    s.changed('null')
    s.changed('{"kinds":["bogus"]}')
    s.changed('{"kinds":["settings","messages"]}')
    s.send('event: surprise\ndata: {"kinds":["jobs"]}\n\n')
    s.send('data: {"kinds":["jobs"]}\n\n')
    s.send(': ping\n\n')
    await flush()
    expect(h.onChanged.mock.calls).toEqual([[['jobs', 'activity']], [['messages', 'settings']]])
    expect(h.onReady).toHaveBeenCalledTimes(1)
    h.ec.stop()
  })

  it('keeps going when a callback throws or rejects', async () => {
    const h = harness({
      onReady: () => {
        throw new Error('boom')
      },
      onChanged: () => Promise.reject(new Error('async boom'))
    })
    h.ec.start()
    const s = await h.accept()
    s.ready()
    s.changed('{"kinds":["jobs"]}')
    s.bye()
    await flush()
    await advance(BACKOFF_BASE_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })
})

describe('createEventClient: reconnect', () => {
  it('reconnects after bye with a jittered delay and reports offline meanwhile', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await flush()
    s.bye()
    await flush()
    expect(s.cancelled).toBe(true)
    expect(h.states()).toEqual(['streaming', 'offline'])
    // random 0.5 of the 1 s base
    await advance(499)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('reconnects when the stream just ends or breaks', async () => {
    const h = harness()
    h.ec.start()
    let s = await h.accept()
    s.ready()
    await flush()
    s.close()
    await flush()
    await advance(BACKOFF_BASE_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    s = await h.accept()
    s.ready()
    s.send('data: ' + 'x'.repeat(70 * 1024) + '\n\n')
    await flush()
    await advance(BACKOFF_CAP_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(3)
    h.ec.stop()
  })

  it('doubles the backoff window up to the cap while streams die young', async () => {
    const h = harness()
    h.random.mockReturnValue(0.999)
    h.ec.start()
    const delays: number[] = []
    for (let n = 0; n < 8; n++) {
      const s = await h.accept()
      s.ready()
      await flush()
      s.bye()
      await flush()
      const before = h.openEvents.mock.calls.length
      let waited = 0
      while (h.openEvents.mock.calls.length === before) {
        await advance(100)
        waited += 100
      }
      delays.push(waited)
    }
    // full jitter: random() * min(cap, base * 2^n), 100 ms resolution
    expect(delays).toEqual([1000, 2000, 4000, 8000, 16000, 32000, 60000, 60000])
    h.ec.stop()
  })

  it('may reconnect at once with the lowest jitter', async () => {
    const h = harness()
    h.random.mockReturnValue(0)
    h.ec.start()
    const s = await h.accept()
    s.ready()
    s.bye()
    await flush()
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('resets the backoff once a stream has been up for a while', async () => {
    const h = harness()
    h.random.mockReturnValue(0.999)
    h.ec.start()
    // three young streams grow the window to 8 s
    for (let n = 0; n < 3; n++) {
      const s = await h.accept()
      s.ready()
      s.bye()
      await flush()
      await untilCall(h.openEvents)
    }
    const s = await h.accept()
    s.ready()
    await advance(STABLE_AFTER_MS)
    s.bye()
    await flush()
    const before = h.openEvents.mock.calls.length
    await advance(998)
    expect(h.openEvents.mock.calls.length).toBe(before)
    await advance(1)
    expect(h.openEvents.mock.calls.length).toBe(before + 1)
    h.ec.stop()
  })

  it('opens every connection with the current token', async () => {
    let token = 'token-one'
    const platform = makePlatform({ now: () => Date.now(), random: () => 0.5, getToken: () => token })
    const streams: FakeStream[] = []
    platform.fetch.mockImplementation(async () => {
      const s = new FakeStream()
      streams.push(s)
      return new Response(s.stream, { status: 200, headers: { 'Content-Type': 'text/event-stream' } })
    })
    const onReady = vi.fn()
    const ec = createEventClient({
      platform,
      client: createClient(platform),
      onReady,
      onChanged: vi.fn(),
      onPoll: vi.fn(),
      visibility: new FakeVisibility()
    })
    ec.start()
    await flush()
    streams[0].ready()
    await flush()
    token = 'token-two'
    streams[0].bye()
    await flush()
    await advance(BACKOFF_BASE_MS)
    expect(platform.fetch).toHaveBeenCalledTimes(2)
    const auth = platform.fetch.mock.calls.map((c) => new Headers(c[1]?.headers).get('Authorization'))
    expect(auth).toEqual(['Bearer token-one', 'Bearer token-two'])
    for (const c of platform.fetch.mock.calls) expect(String(c[0])).not.toContain('token')
    ec.stop()
  })
})

describe('createEventClient: polling', () => {
  it('polls after a 503, retries the stream every minute and recovers', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(503, 'unavailable'))
    expect(h.states()).toEqual(['polling'])
    // the first poll comes at once (nothing was fetched yet), then every 15 s
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    await advance(POLL_EVERY_MS)
    expect(h.onPoll).toHaveBeenCalledTimes(2)
    await advance(POLL_EVERY_MS * 2)
    expect(h.onPoll).toHaveBeenCalledTimes(4)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    await advance(STREAM_RETRY_MS - POLL_EVERY_MS * 3)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    // the retry fails too: still polling, next retry a minute later
    await h.fail(new ApiError(503, 'unavailable'))
    await advance(STREAM_RETRY_MS - 1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(3)
    const s = await h.accept()
    s.ready()
    await flush()
    expect(h.states()).toEqual(['polling', 'streaming'])
    expect(h.onReady).toHaveBeenCalledTimes(1)
    const polls = h.onPoll.mock.calls.length
    await advance(POLL_EVERY_MS * 3)
    expect(h.onPoll.mock.calls.length).toBe(polls)
    h.ec.stop()
  })

  it('polls after a 429 and waits at least a minute before the retry', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(429, 'rate_limited', 5))
    expect(h.states()).toEqual(['polling'])
    await advance(STREAM_RETRY_MS - 1)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('honours a Retry-After longer than a minute, up to the cap', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(429, 'rate_limited', 120))
    await advance(120_000 - 1)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    await h.fail(new ApiError(503, 'unavailable', 86_400))
    await advance(STREAM_RETRY_CAP_MS - 1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(3)
    h.ec.stop()
  })

  it('polls after three connect failures in a row', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(0, 'unavailable'))
    expect(h.states()).toEqual(['offline'])
    await advance(BACKOFF_CAP_MS)
    await h.fail(new ApiError(401, 'unauthenticated'))
    await advance(BACKOFF_CAP_MS)
    expect(h.onPoll).not.toHaveBeenCalled()
    // a stream that ends before ready counts as a failure too
    const s = await h.accept()
    s.close()
    await flush()
    expect(h.states()).toEqual(['offline', 'polling'])
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    h.ec.stop()
  })

  it('does not poll right after a refresh when it falls back', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await advance(5000)
    s.bye()
    await flush()
    await advance(BACKOFF_BASE_MS)
    await h.fail(new ApiError(503, 'unavailable'))
    expect(h.states()).toEqual(['streaming', 'offline', 'polling'])
    expect(h.onPoll).not.toHaveBeenCalled()
    // the next poll is due 15 s after the ready (now 6 s)
    await advance(POLL_EVERY_MS - 6000 - 1)
    expect(h.onPoll).not.toHaveBeenCalled()
    await advance(1)
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    h.ec.stop()
  })
})

describe('createEventClient: full refresh', () => {
  it('refreshes every minute while streaming, counted from the last refetch', async () => {
    const h = harness()
    h.ec.start()
    let s = await h.accept()
    s.ready()
    await flush()
    await advance(REFRESH_EVERY_MS - 1)
    expect(h.onPoll).not.toHaveBeenCalled()
    await advance(1)
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    // a reconnect's ready refetches everything, so it moves the next refresh
    s.send(': ping\n\n')
    await advance(40_000)
    s.bye()
    await flush()
    await advance(500)
    s = await h.accept()
    s.ready()
    await flush()
    await advance(REFRESH_EVERY_MS - 1)
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(h.onPoll).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('never polls during a burst of short-lived streams', async () => {
    const h = harness()
    h.ec.start()
    for (let n = 0; n < 6; n++) {
      const s = await h.accept()
      s.ready()
      s.bye()
      await flush()
      await untilCall(h.openEvents)
    }
    expect(h.onReady).toHaveBeenCalledTimes(6)
    expect(h.onPoll).not.toHaveBeenCalled()
    h.ec.stop()
  })
})

describe('createEventClient: visibility', () => {
  it('closes the stream while hidden and reconnects with jitter when visible', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await flush()
    h.visibility.set(true)
    await flush()
    expect(h.calls[0].signal.aborted).toBe(true)
    expect(s.cancelled).toBe(true)
    // hidden is not a connection problem: the last state stays
    expect(h.states()).toEqual(['streaming'])
    await advance(10 * 60_000)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    expect(h.onPoll).not.toHaveBeenCalled()
    expect(vi.getTimerCount()).toBe(0)
    h.visibility.set(false)
    await flush()
    // random 0.5 of the 1 s visibility jitter
    await advance(VISIBLE_JITTER_MS / 2 - 1)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    await advance(1)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    const s2 = await h.accept()
    s2.ready()
    await flush()
    expect(h.onReady).toHaveBeenCalledTimes(2)
    expect(h.states()).toEqual(['streaming'])
    h.ec.stop()
  })

  it('waits for visibility before the first connection', async () => {
    const visibility = new FakeVisibility()
    visibility.isHidden = true
    const h = harness({ visibility })
    h.ec.start()
    await advance(60_000)
    expect(h.openEvents).not.toHaveBeenCalled()
    visibility.set(false)
    await advance(VISIBLE_JITTER_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    h.ec.stop()
  })

  it('polls at once and retries the stream soon when visible again in polling mode', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(503, 'unavailable'))
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    h.visibility.set(true)
    await advance(5 * 60_000)
    expect(h.onPoll).toHaveBeenCalledTimes(1)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    h.visibility.set(false)
    await flush()
    expect(h.onPoll).toHaveBeenCalledTimes(2)
    await advance(VISIBLE_JITTER_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    expect(h.states()).toEqual(['polling'])
    h.ec.stop()
  })

  it('reconnects once after a quick hide and show flicker', async () => {
    const h = harness()
    h.ec.start()
    h.visibility.set(true)
    h.visibility.set(false)
    h.visibility.set(true)
    h.visibility.set(false)
    await advance(VISIBLE_JITTER_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    expect(h.calls.filter((c) => !c.signal.aborted)).toHaveLength(1)
    h.ec.stop()
  })
})

describe('createEventClient: watchdog', () => {
  it('gives up on a connection that sends headers but no ready', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.send(': ping\n\n')
    await advance(FIRST_READY_MS - 1)
    expect(s.cancelled).toBe(false)
    await advance(1)
    expect(s.cancelled).toBe(true)
    expect(h.calls[0].signal.aborted).toBe(true)
    expect(h.states()).toEqual(['offline'])
    await advance(BACKOFF_BASE_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('gives up on a request that never gets headers', async () => {
    const h = harness()
    h.ec.start()
    await flush()
    await advance(FIRST_READY_MS - 1)
    expect(h.calls[0].signal.aborted).toBe(false)
    await advance(1)
    expect(h.calls[0].signal.aborted).toBe(true)
    // the real client rejects when its signal aborts; the fake has to be told
    await h.fail(new DOMException('aborted', 'AbortError'))
    expect(h.states()).toEqual(['offline'])
    h.ec.stop()
  })

  it('recycles a stream that goes silent after ready', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await flush()
    await advance(IDLE_MS - 1)
    expect(s.cancelled).toBe(false)
    await advance(1)
    expect(s.cancelled).toBe(true)
    expect(h.states()).toEqual(['streaming', 'offline'])
    await advance(BACKOFF_BASE_MS)
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    h.ec.stop()
  })

  it('keeps a stream alive while pings arrive', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await flush()
    for (let n = 0; n < 12; n++) {
      await advance(25_000)
      s.send(': ping\n\n')
      await flush()
    }
    expect(s.cancelled).toBe(false)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    expect(h.states()).toEqual(['streaming'])
    h.ec.stop()
  })
})

describe('createEventClient: stop', () => {
  it('aborts a pending connect and ignores its late answer', async () => {
    const h = harness()
    h.ec.start()
    await flush()
    h.ec.stop()
    expect(h.calls[0].signal.aborted).toBe(true)
    const s = await h.accept(0)
    s.ready()
    await flush()
    expect(s.cancelled).toBe(true)
    await advance(10 * 60_000)
    expect(h.onReady).not.toHaveBeenCalled()
    expect(h.onState).not.toHaveBeenCalled()
    expect(h.onPoll).not.toHaveBeenCalled()
    expect(h.openEvents).toHaveBeenCalledTimes(1)
  })

  it('aborts a running stream and calls nothing afterwards', async () => {
    const h = harness()
    h.ec.start()
    const s = await h.accept()
    s.ready()
    await flush()
    h.onState.mockClear()
    h.ec.stop()
    expect(h.calls[0].signal.aborted).toBe(true)
    s.changed('{"kinds":["jobs"]}')
    s.bye()
    h.visibility.set(true)
    h.visibility.set(false)
    await advance(10 * 60_000)
    expect(h.onChanged).not.toHaveBeenCalled()
    expect(h.onState).not.toHaveBeenCalled()
    expect(h.onPoll).not.toHaveBeenCalled()
    expect(h.openEvents).toHaveBeenCalledTimes(1)
    expect(h.visibility.listener).toBeUndefined()
  })

  it('stops polling and ignores a late connect failure', async () => {
    const h = harness()
    h.ec.start()
    await h.fail(new ApiError(503, 'unavailable'))
    await advance(STREAM_RETRY_MS)
    h.ec.stop()
    h.onState.mockClear()
    h.onPoll.mockClear()
    await h.fail(new ApiError(503, 'unavailable'))
    await advance(10 * 60_000)
    expect(h.onPoll).not.toHaveBeenCalled()
    expect(h.onState).not.toHaveBeenCalled()
    expect(h.openEvents).toHaveBeenCalledTimes(2)
  })

  it('leaves no timer when a callback stops it', async () => {
    let h!: ReturnType<typeof harness>
    const stop = () => h.ec.stop()
    // stop inside onReady
    h = harness({ onReady: stop })
    h.ec.start()
    let s = await h.accept()
    s.ready()
    await flush()
    expect(vi.getTimerCount()).toBe(0)
    expect(s.cancelled).toBe(true)
    // stop inside onChanged
    h = harness({ onChanged: stop })
    h.ec.start()
    s = await h.accept()
    s.ready()
    s.changed('{"kinds":["jobs"]}')
    await flush()
    expect(vi.getTimerCount()).toBe(0)
    // stop inside onPoll: the full refresh while streaming
    h = harness({ onPoll: stop })
    h.ec.start()
    s = await h.accept()
    s.ready()
    await advance(REFRESH_EVERY_MS)
    expect(vi.getTimerCount()).toBe(0)
    // stop inside onPoll: polling mode
    h = harness({ onPoll: stop })
    h.ec.start()
    await h.fail(new ApiError(503, 'unavailable'))
    expect(vi.getTimerCount()).toBe(0)
    // stop inside onState
    h = harness({ onState: stop })
    h.ec.start()
    await h.fail(new ApiError(0, 'unavailable'))
    expect(vi.getTimerCount()).toBe(0)
    expect(h.openEvents).toHaveBeenCalledTimes(1)
  })

  it('can be started again after stop', async () => {
    const h = harness()
    h.ec.start()
    h.ec.stop()
    h.ec.start()
    await flush()
    expect(h.openEvents).toHaveBeenCalledTimes(2)
    const s = await h.accept()
    s.ready()
    await flush()
    expect(h.onReady).toHaveBeenCalledTimes(1)
    h.ec.stop()
  })
})
