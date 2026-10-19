import type { Platform } from '../platform'
import type { ApiClient } from './client'
import { ApiError } from './errors'
import { parseSSE } from './sse'

// The event-stream client keeps the UI fresh from GET /events. The stream
// only says WHAT to refetch (kinds), never what changed, and it has no
// event ids: every `ready` means "refetch everything". Because a change can
// be missed (marks with millisecond resolution, changes that move no mark),
// the client also asks for a full refresh on a slow timer, and it falls
// back to polling when the server refuses streams (429, 503) or the stream
// keeps failing.

export const CHANGE_KINDS = ['jobs', 'messages', 'activity', 'settings'] as const
export type ChangeKind = (typeof CHANGE_KINDS)[number]

// StreamState is reported on changes only: 'streaming' after a ready,
// 'polling' in the fallback mode, 'offline' while reconnecting. Nothing is
// reported before the first ready or fallback, nor while the page is hidden
// (the last state stays until the next connection reports a new one).
export type StreamState = 'streaming' | 'polling' | 'offline'

// Reconnect delay: full jitter, random() * min(cap, base * 2^attempt).
export const BACKOFF_BASE_MS = 1_000
export const BACKOFF_CAP_MS = 60_000
// A stream that lived this long after its ready resets the backoff; younger
// ones do not, so a server that answers ready and bye at once causes no
// reconnect storm.
export const STABLE_AFTER_MS = 30_000
// Consecutive connections without a ready before falling back to polling.
export const MAX_FAILURES = 3
// Polling mode: onPoll every POLL_EVERY_MS, a stream attempt every
// STREAM_RETRY_MS (or the server's Retry-After, if longer, up to the cap).
export const POLL_EVERY_MS = 15_000
export const STREAM_RETRY_MS = 60_000
export const STREAM_RETRY_CAP_MS = 10 * 60_000
// Streaming mode: a full refresh (onPoll) REFRESH_EVERY_MS after the last
// refetch (onReady or onPoll), never sooner than REFRESH_MIN_MS after it.
export const REFRESH_EVERY_MS = 60_000
export const REFRESH_MIN_MS = 30_000
// Watchdog: a connection without a ready FIRST_READY_MS after it was opened
// (headers included), or a stream silent for IDLE_MS after its ready (the
// server pings every 25 s), is aborted and handled as lost.
export const FIRST_READY_MS = 15_000
export const IDLE_MS = 70_000
// When the page becomes visible again, the stream reconnects after a random
// delay of up to VISIBLE_JITTER_MS, so tabs that show together spread out.
export const VISIBLE_JITTER_MS = 1_000

export interface Timers {
  setTimeout(fn: () => void, ms: number): unknown
  clearTimeout(handle: unknown): void
}

export interface Visibility {
  hidden(): boolean
  // subscribe calls onChange on every visibility change and returns the
  // unsubscribe function
  subscribe(onChange: () => void): () => void
}

type Callback<A extends unknown[] = []> = (...args: A) => void | Promise<void>

export interface EventClientOptions {
  platform: Pick<Platform, 'now' | 'random'>
  client: Pick<ApiClient, 'openEvents'>
  // onReady: a stream (re)connected, refetch everything
  onReady: Callback
  // onChanged: refetch the listed kinds (known kinds only, never empty)
  onChanged: Callback<[ChangeKind[]]>
  // onPoll: refetch everything (polling mode and the slow full refresh)
  onPoll: Callback
  onState?: Callback<[StreamState]>
  timers?: Timers
  // visibility defaults to the document's; null disables it
  visibility?: Visibility | null
}

export interface EventClient {
  start(): void
  stop(): void
}

const globalTimers: Timers = {
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (h) => globalThis.clearTimeout(h as ReturnType<typeof globalThis.setTimeout>)
}

const documentVisibility = (): Visibility | null =>
  typeof document === 'undefined'
    ? null
    : {
        hidden: () => document.visibilityState === 'hidden',
        subscribe: (cb) => {
          document.addEventListener('visibilitychange', cb)
          return () => document.removeEventListener('visibilitychange', cb)
        }
      }

const parseKinds = (data: string): ChangeKind[] => {
  let v: unknown
  try {
    v = JSON.parse(data)
  } catch {
    return []
  }
  if (typeof v !== 'object' || v === null) return []
  const kinds = (v as { kinds?: unknown }).kinds
  if (!Array.isArray(kinds)) return []
  return CHANGE_KINDS.filter((k) => kinds.includes(k))
}

type TimerName = 'reconnect' | 'retry' | 'poll' | 'refresh' | 'watchdog'

export const createEventClient = (opts: EventClientOptions): EventClient => {
  const timers = opts.timers ?? globalTimers
  const visibility = opts.visibility === undefined ? documentVisibility() : opts.visibility
  const now = () => opts.platform.now()

  let running = false
  let hidden = false
  // conn identifies the current connection; a callback of an older one
  // (after stop, hide or a newer connect) is ignored
  let conn = 0
  let abort: AbortController | undefined
  let polling = false
  let attempt = 0
  let failures = 0
  let readyAt: number | undefined
  let lastRefresh = -Infinity
  let state: StreamState | undefined
  let unsubscribe: (() => void) | undefined
  const pending = new Map<TimerName, unknown>()

  const clear = (name: TimerName) => {
    if (pending.has(name)) {
      timers.clearTimeout(pending.get(name))
      pending.delete(name)
    }
  }
  const clearAll = () => {
    for (const name of [...pending.keys()]) clear(name)
  }
  // later schedules fn; nothing is scheduled once stopped (a callback may
  // have called stop())
  const later = (name: TimerName, ms: number, fn: () => void) => {
    clear(name)
    if (!running) return
    const h = timers.setTimeout(
      () => {
        if (pending.get(name) !== h) return
        pending.delete(name)
        if (running && !hidden) fn()
      },
      Math.max(0, ms)
    )
    pending.set(name, h)
  }

  const call = <A extends unknown[]>(fn: Callback<A> | undefined, ...args: A) => {
    if (!running || !fn) return
    try {
      const r = fn(...args)
      if (r && typeof (r as Promise<void>).then === 'function') (r as Promise<void>).then(undefined, () => {})
    } catch {
      // a failing callback must not stop the event loop
    }
  }

  const setState = (s: StreamState) => {
    if (s === state) return
    state = s
    call(opts.onState, s)
  }

  const disconnect = () => {
    conn++
    clear('watchdog')
    abort?.abort()
    abort = undefined
  }

  const scheduleRefresh = () => {
    // every refetch reschedules this timer, so refetches coalesce: a ready
    // after a reconnect moves the next full refresh instead of adding one
    later('refresh', Math.max(REFRESH_MIN_MS, lastRefresh + REFRESH_EVERY_MS - now()), () => {
      lastRefresh = now()
      call(opts.onPoll)
      if (!running) return
      scheduleRefresh()
    })
  }

  const schedulePoll = () => {
    later('poll', lastRefresh + POLL_EVERY_MS - now(), () => {
      lastRefresh = now()
      call(opts.onPoll)
      if (!running) return
      schedulePoll()
    })
  }

  const scheduleRetry = (err?: unknown) => {
    const after = err instanceof ApiError && err.retryAfter ? err.retryAfter * 1000 : 0
    later('retry', Math.min(STREAM_RETRY_CAP_MS, Math.max(STREAM_RETRY_MS, after)), () => void connect())
  }

  const enterPolling = (err?: unknown) => {
    polling = true
    attempt = 0
    clear('reconnect')
    clear('refresh')
    setState('polling')
    if (!running) return
    schedulePoll()
    scheduleRetry(err)
  }

  const onReady = () => {
    readyAt = now()
    failures = 0
    if (polling) {
      polling = false
      clear('poll')
      clear('retry')
    }
    setState('streaming')
    if (!running) return
    lastRefresh = now()
    call(opts.onReady)
    if (!running) return
    scheduleRefresh()
  }

  // lost handles the end of a connection, with or without a ready before
  const lost = (gotReady: boolean, err?: unknown) => {
    abort = undefined
    clear('refresh')
    clear('watchdog')
    if (gotReady && readyAt !== undefined && now() - readyAt >= STABLE_AFTER_MS) attempt = 0
    readyAt = undefined
    if (!gotReady) failures++
    if (polling) {
      scheduleRetry(err)
      return
    }
    const refused = err instanceof ApiError && (err.status === 429 || err.status === 503)
    if (refused || failures >= MAX_FAILURES) {
      enterPolling(err)
      return
    }
    setState('offline')
    if (!running) return
    const delay = opts.platform.random() * Math.min(BACKOFF_CAP_MS, BACKOFF_BASE_MS * 2 ** attempt)
    attempt++
    later('reconnect', delay, () => void connect())
  }

  const connect = async () => {
    if (!running || hidden) return
    disconnect()
    const my = conn
    const ac = new AbortController()
    abort = ac
    // the watchdog aborts the connection; the aborted request or stream then
    // ends below and is handled as lost
    const watch = (ms: number) => later('watchdog', ms, () => ac.abort())
    watch(FIRST_READY_MS)
    let res: Response
    try {
      res = await opts.client.openEvents(ac.signal)
    } catch (err) {
      if (my === conn) lost(false, err)
      return
    }
    if (my !== conn) {
      res.body?.cancel().catch(() => {})
      return
    }
    if (!res.body) {
      lost(false)
      return
    }
    let gotReady = false
    try {
      const onChunk = () => {
        if (gotReady && my === conn) watch(IDLE_MS)
      }
      for await (const ev of parseSSE(res.body, { signal: ac.signal, onChunk })) {
        if (my !== conn) return
        if (ev.event === 'ready') {
          gotReady = true
          onReady()
          if (my !== conn) return
          watch(IDLE_MS)
        } else if (ev.event === 'changed') {
          const kinds = parseKinds(ev.data)
          if (kinds.length > 0) call(opts.onChanged, kinds)
        } else if (ev.event === 'bye') {
          break
        }
      }
    } catch {
      // a broken or oversized stream is handled like its end
    }
    if (my === conn) lost(gotReady)
  }

  const onVisibility = () => {
    if (!running || !visibility) return
    const h = visibility.hidden()
    if (h === hidden) return
    hidden = h
    if (h) {
      // a hidden page is not a connection problem: the state is left as it
      // was (the next connection reports the new one)
      disconnect()
      clearAll()
      readyAt = undefined
      return
    }
    attempt = 0
    if (polling) schedulePoll()
    later('reconnect', opts.platform.random() * VISIBLE_JITTER_MS, () => void connect())
  }

  return {
    start() {
      if (running) return
      running = true
      polling = false
      attempt = 0
      failures = 0
      readyAt = undefined
      lastRefresh = -Infinity
      state = undefined
      hidden = visibility?.hidden() ?? false
      unsubscribe = visibility?.subscribe(onVisibility)
      void connect()
    },
    stop() {
      if (!running) return
      running = false
      disconnect()
      clearAll()
      unsubscribe?.()
      unsubscribe = undefined
    }
  }
}
