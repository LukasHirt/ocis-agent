import { defineStore } from 'pinia'
import { computed, reactive, ref, shallowRef } from 'vue'
import type { Platform } from '../platform'
import { createClient, type ApiClient, type ResumeReason } from '../api/client'
import { ApiError, describeError, isAbortError, type Gettext } from '../api/errors'
import {
  createEventClient,
  type ChangeKind,
  type EventClient,
  type EventClientOptions,
  type StreamState,
  type Timers,
  type Visibility
} from '../api/events'
import type {
  Approval,
  Approvals,
  Decision,
  DecisionItem,
  Renewed,
  Revoked,
  RevokeAllPreview,
  Settings,
  SettingsUpdate,
  Status
} from '../api/types'

// Full refreshes (ready, poll) start at most once per FULL_REFRESH_MIN_MS;
// a request inside the window is deferred to its end and coalesced.
export const FULL_REFRESH_MIN_MS = 1_000
// The approvals lists are read in pages of APPROVAL_PAGE_LIMIT (the
// server's cap), at most APPROVAL_MAX_PAGES of them per full refresh.
export const APPROVAL_PAGE_LIMIT = 100
export const APPROVAL_MAX_PAGES = 10
// A head refresh of the list to verify follows empty pages (the server caps
// the candidates one request examines, so a page may be empty and carry a
// cursor) up to VERIFY_HEAD_PAGES, stopping at the first non-empty page.
export const VERIFY_HEAD_PAGES = 5

export type Resource = 'status' | 'approvals' | 'settings'
const RESOURCES: readonly Resource[] = ['status', 'approvals', 'settings']

// Depth of a refresh: 'full' reads every page (start, ready, poll, the
// user's refresh, after an action); 'head' only the first page of each
// list (change events, which arrive every few seconds on a busy agent).
// A head refresh replaces the lists with their first pages, so `complete`
// turns false until the next full one when more pages exist.
type Depth = 'head' | 'full'

export interface ApprovalsState {
  pending: Approval[] // newest first; includes ended jobs' approvals (job_ended)
  verify: Approval[] // needs_verification: the user must check the outcome
  counts: { pending: number; live: number; verify: number } // of the loaded lists
  complete: boolean // pending: false when more pages exist than were read
  verifyComplete: boolean // verify: false when more pages exist than were read
}

export interface StartOptions {
  platform: Platform
  gettext: Gettext
  timers?: Timers
  visibility?: Visibility | null
  createEvents?: (opts: EventClientOptions) => EventClient
}

interface Session {
  platform: Platform
  gettext: Gettext
  timers: Timers
  client: ApiClient
  events: EventClient
  abort: AbortController
  handle: SessionHandle
}

// SessionHandle is what screen stores use of a running session: one object
// per session, so comparing handles tells whether the session is still the
// same. signal aborts when the session ends.
export interface SessionHandle {
  readonly client: ApiClient
  readonly platform: Platform
  readonly gettext: Gettext
  readonly signal: AbortSignal
}

// StoreEvent is told to subscribers: 'changed' for the kinds of a change
// event, 'refresh' when everything should be read again (fresh after a
// ready, since changes may have been missed), 'reset' when the session
// ended (a later one may be another user's).
export type StoreEvent =
  | { type: 'changed'; kinds: ChangeKind[] }
  | { type: 'refresh'; fresh: boolean }
  | { type: 'reset' }

export type StoreListener = (e: StoreEvent) => void | Promise<void>

const globalTimers: Timers = {
  setTimeout: (fn, ms) => globalThis.setTimeout(fn, ms),
  clearTimeout: (h) => globalThis.clearTimeout(h as ReturnType<typeof globalThis.setTimeout>)
}

const emptyApprovals = (): ApprovalsState => ({
  pending: [],
  verify: [],
  counts: { pending: 0, live: 0, verify: 0 },
  complete: true,
  verifyComplete: true
})

const approvalsOf = (
  pending: Approval[],
  verify: Approval[],
  complete: boolean,
  verifyComplete: boolean
): ApprovalsState => ({
  pending,
  verify,
  counts: {
    pending: pending.length,
    live: pending.filter((a) => !a.job_ended).length,
    verify: verify.length
  },
  complete,
  verifyComplete
})

// Codes after which a decision's (or an acknowledgement's) list is out of
// date.
const REFETCH_CODES = new Set([
  'stale_arguments',
  'expired',
  'already_decided',
  'job_ended',
  'not_found',
  'conflict',
  'not_acknowledgeable'
])

// useAgentStore keeps the agent's status, the pending approvals and the
// settings fresh: refreshAll on every ready and poll of the event stream,
// only the implied resources on a change (the approvals then only their
// first pages, see Depth). A refresh already in flight is
// reused; a change or an action that needs data newer than the request in
// flight queues ONE follow-up. Errors are kept per resource as fixed texts
// and never blank the data of the others.
export const useAgentStore = defineStore('ocis-agent', () => {
  const status = shallowRef<Status | null>(null)
  const approvals = shallowRef<ApprovalsState>(emptyApprovals())
  const settings = shallowRef<Settings | null>(null)
  // the views acknowledgements of this session returned, by approval id
  const acknowledged = shallowRef<Map<string, Approval>>(new Map())
  const loading = reactive<Record<Resource, boolean>>({ status: false, approvals: false, settings: false })
  const errors = reactive<Record<Resource, string | null>>({ status: null, approvals: null, settings: null })
  const error = computed(() => errors.status ?? errors.approvals ?? errors.settings ?? null)
  const connection = ref<StreamState>('streaming')
  const lastRefreshed = ref<number | null>(null)

  let session: Session | null = null
  const listeners = new Set<StoreListener>()

  // subscribe registers a screen store for the session's events; it
  // returns the function that ends the subscription.
  const subscribe = (fn: StoreListener): (() => void) => {
    listeners.add(fn)
    return () => {
      listeners.delete(fn)
    }
  }

  // tell calls every listener; a failing one never stops the others or
  // the store's own refresh.
  const tell = (e: StoreEvent): Promise<void> =>
    Promise.all(
      [...listeners].map(async (fn) => {
        try {
          await fn(e)
        } catch {
          // a screen store's problem: it keeps its own error state
        }
      })
    ).then(() => undefined)

  const currentSession = (): SessionHandle | null => session?.handle ?? null
  const inflight = new Map<Resource, Promise<void>>()
  const inflightDepth = new Map<Resource, Depth>()
  const queued = new Map<Resource, Promise<void>>()
  const queuedDepth = new Map<Resource, Depth>()
  // writes counts the writes of each resource; a refresh stores its answer
  // only if nothing was written since it started (an action's answer is
  // newer than a refresh that began before it)
  const writes: Record<Resource, number> = { status: 0, approvals: 0, settings: 0 }
  let fullInflight: Promise<void> | null = null
  let fullQueued: Promise<void> | null = null
  let lastFull = -Infinity
  let deferred: { handle: unknown; promise: Promise<void>; resolve: () => void } | null = null

  const loaders: Record<Resource, (s: Session, depth: Depth) => Promise<() => void>> = {
    status: async (s) => {
      const v = await s.client.getStatus(s.abort.signal)
      return () => {
        status.value = v
      }
    },
    settings: async (s) => {
      const v = await s.client.getSettings(s.abort.signal)
      return () => {
        settings.value = v
      }
    },
    approvals: async (s, depth) => {
      const full = depth === 'full'
      const list = async (needsVerification?: true) => {
        const pages = full ? APPROVAL_MAX_PAGES : needsVerification ? VERIFY_HEAD_PAGES : 1
        const out: Approval[] = []
        let cursor: string | undefined
        for (let i = 0; i < pages; i++) {
          const page = await s.client.listApprovals(
            { needsVerification, limit: APPROVAL_PAGE_LIMIT, cursor },
            s.abort.signal
          )
          if (!Array.isArray(page?.approvals)) throw new ApiError(502, 'internal')
          out.push(...page.approvals)
          if (!page.next_cursor) return { out, complete: true }
          cursor = page.next_cursor
          if (!full && out.length > 0) break
        }
        return { out, complete: false }
      }
      const [pending, verify] = await Promise.all([list(), list(true)])
      return () => {
        approvals.value = approvalsOf(pending.out, verify.out, pending.complete, verify.complete)
      }
    }
  }

  const load = (key: Resource, depth: Depth): Promise<void> => {
    const s = session
    if (!s) return Promise.resolve()
    const running = inflight.get(key)
    if (running) return running
    const seq = writes[key]
    loading[key] = true
    inflightDepth.set(key, depth)
    const p = loaders[key](s, depth).then(
      (apply) => {
        if (session !== s) return
        if (writes[key] === seq) {
          apply()
          writes[key]++
        }
        errors[key] = null
      },
      (e: unknown) => {
        if (session !== s || isAbortError(e)) return
        errors[key] = describeError(e, s.gettext)
      }
    )
    const done = p.then(() => {
      if (inflight.get(key) === done) {
        inflight.delete(key)
        inflightDepth.delete(key)
        loading[key] = false
      }
    })
    inflight.set(key, done)
    return done
  }

  // refresh reuses a request in flight that reads at least as deep; with
  // fresh, or when the one in flight reads less, it waits for that one and
  // then makes one more (shared by every such caller meanwhile, as deep as
  // the deepest of them asked).
  const refresh = (key: Resource, fresh = false, want: Depth = 'full'): Promise<void> => {
    const depth: Depth = key === 'approvals' ? want : 'full' // only the approvals have pages
    const running = inflight.get(key)
    if (!running) return load(key, depth)
    const covers = depth === 'head' || inflightDepth.get(key) === 'full'
    if (!fresh && covers) return running
    if (depth === 'full') queuedDepth.set(key, 'full')
    else if (!queuedDepth.has(key)) queuedDepth.set(key, 'head')
    const q = queued.get(key)
    if (q) return q
    const s = session
    const next = running.then(() => {
      queued.delete(key)
      const d = queuedDepth.get(key) ?? 'full'
      queuedDepth.delete(key)
      return session === s ? refresh(key, false, d) : undefined
    })
    queued.set(key, next)
    return next
  }

  const refreshStatus = () => refresh('status')
  const refreshApprovals = () => refresh('approvals')
  const refreshSettings = () => refresh('settings')

  const runFull = (s: Session): Promise<void> => {
    lastFull = s.platform.now()
    const p = Promise.all(RESOURCES.map((k) => refresh(k))).then(() => {
      if (session === s && RESOURCES.every((k) => errors[k] === null)) {
        lastRefreshed.value = s.platform.now()
      }
    })
    fullInflight = p
    void p.finally(() => {
      if (fullInflight === p) fullInflight = null
    })
    return p
  }

  // refreshAll reuses a full refresh in flight; with fresh (a stream
  // reconnected, so changes may have been missed) it waits for that one and
  // then makes one more, shared by every fresh caller meanwhile.
  const refreshAll = (opts: { fresh?: boolean } = {}): Promise<void> => {
    const s = session
    if (!s) return Promise.resolve()
    if (fullInflight) {
      if (!opts.fresh) return fullInflight
      if (!fullQueued) {
        const q: Promise<void> = fullInflight.then(() => {
          if (fullQueued === q) fullQueued = null
          return session === s ? refreshAll() : undefined
        })
        fullQueued = q
      }
      return fullQueued
    }
    if (deferred) return deferred.promise
    const wait = lastFull + FULL_REFRESH_MIN_MS - s.platform.now()
    if (wait <= 0) return runFull(s)
    let resolve!: () => void
    const promise = new Promise<void>((r) => (resolve = r))
    const handle = s.timers.setTimeout(() => {
      deferred = null
      if (session !== s) return resolve()
      void runFull(s).then(resolve)
    }, wait)
    deferred = { handle, promise, resolve }
    return promise
  }

  // KIND_RESOURCES is what a change kind rereads here (first pages). The
  // screen stores (chat, jobs, audit, memory) react to the kinds themselves
  // through subscribe, and only while their screen is mounted.
  const KIND_RESOURCES: Record<ChangeKind, Resource[]> = {
    jobs: ['status', 'approvals'],
    activity: ['status', 'approvals'],
    settings: ['settings', 'status'],
    messages: []
  }

  const onChanged = async (kinds: ChangeKind[]) => {
    const keys = new Set<Resource>()
    for (const k of kinds) for (const r of KIND_RESOURCES[k] ?? []) keys.add(r)
    await Promise.all([...keys].map((k) => refresh(k, true, 'head')))
  }

  // reset forgets everything of a session, so a new one (maybe of another
  // user in the same page) never shows old data or errors.
  const reset = () => {
    acknowledged.value = new Map()
    status.value = null
    approvals.value = emptyApprovals()
    settings.value = null
    for (const k of RESOURCES) {
      loading[k] = false
      errors[k] = null
      writes[k] = 0
    }
    inflight.clear()
    inflightDepth.clear()
    queued.clear()
    queuedDepth.clear()
    fullInflight = null
    fullQueued = null
    lastFull = -Infinity
    lastRefreshed.value = null
    connection.value = 'streaming'
  }

  const start = (opts: StartOptions) => {
    if (session) return
    reset()
    const client = createClient(opts.platform)
    const make = opts.createEvents ?? createEventClient
    const abort = new AbortController()
    let s: Session | null = null
    const guard =
      <A extends unknown[]>(fn: (...a: A) => void | Promise<void>) =>
      (...a: A) =>
        session === s && s !== null ? fn(...a) : undefined
    const events = make({
      platform: opts.platform,
      client,
      onReady: guard(() =>
        Promise.all([refreshAll({ fresh: true }), tell({ type: 'refresh', fresh: true })]).then(() => undefined)
      ),
      onPoll: guard(() =>
        Promise.all([refreshAll(), tell({ type: 'refresh', fresh: false })]).then(() => undefined)
      ),
      onChanged: guard((kinds: ChangeKind[]) =>
        Promise.all([onChanged(kinds), tell({ type: 'changed', kinds: [...kinds] })]).then(() => undefined)
      ),
      onState: guard((st: StreamState) => {
        connection.value = st
      }),
      ...(opts.timers && { timers: opts.timers }),
      ...(opts.visibility !== undefined && { visibility: opts.visibility })
    })
    s = {
      platform: opts.platform,
      gettext: opts.gettext,
      timers: opts.timers ?? globalTimers,
      client,
      events,
      abort,
      handle: Object.freeze({ client, platform: opts.platform, gettext: opts.gettext, signal: abort.signal })
    }
    session = s
    connection.value = 'streaming'
    events.start()
    void refreshAll()
  }

  const stop = () => {
    const s = session
    if (!s) return
    session = null
    s.events.stop()
    s.abort.abort()
    if (deferred) {
      s.timers.clearTimeout(deferred.handle)
      deferred.resolve()
      deferred = null
    }
    reset()
    void tell({ type: 'reset' })
  }

  const active = (): Session => {
    if (!session) throw new ApiError(0, 'unavailable')
    return session
  }

  const setStatus = (s: Session, v: Status) => {
    if (session !== s) return
    status.value = v
    writes.status++
  }

  const dropApprovals = (s: Session, ids: Set<string>) => {
    if (session !== s) return
    const cur = approvals.value
    approvals.value = approvalsOf(
      cur.pending.filter((a) => !ids.has(a.id)),
      cur.verify.filter((a) => !ids.has(a.id)),
      cur.complete,
      cur.verifyComplete
    )
    writes.approvals++
  }

  const afterDecision = () => Promise.all([refresh('approvals', true), refresh('status', true)])

  const decided = async <T>(s: Session, ids: string[], call: () => Promise<T>): Promise<T> => {
    try {
      const res = await call()
      dropApprovals(s, new Set(ids))
      await afterDecision()
      return res
    } catch (e) {
      if (e instanceof ApiError && REFETCH_CODES.has(e.code)) await afterDecision()
      throw e
    }
  }

  // decide sends the decision bound to the args_hash the user was shown.
  const decide = (id: string, decision: Decision, argsHash: string): Promise<Approval> => {
    const s = active()
    return decided(s, [id], () => s.client.decide(id, decision, argsHash))
  }

  const bulkDecide = (decision: Decision, items: DecisionItem[]): Promise<Approvals> => {
    const s = active()
    return decided(
      s,
      items.map((i) => i.id),
      () => s.client.bulkDecide(decision, items)
    )
  }

  // acknowledge records that the user checked the outcome of an action
  // whose outcome is unknown; it then leaves the list to verify.
  const acknowledge = async (id: string): Promise<Approval> => {
    const s = active()
    const v = await decided(s, [id], () => s.client.acknowledge(id))
    if (session === s && v && typeof v === 'object' && v.id === id) {
      acknowledged.value = new Map(acknowledged.value).set(id, v)
    }
    return v
  }

  // viewOf is the view an acknowledgement returned for this approval (so
  // a card read before it, such as one in the history, shows it as
  // checked), else the approval as given.
  const viewOf = (a: Approval): Approval => acknowledged.value.get(a.id) ?? a

  const saveSettings = async (update: SettingsUpdate): Promise<Settings> => {
    const s = active()
    const v = await s.client.putSettings(update)
    if (session === s) {
      settings.value = v
      writes.settings++
      errors.settings = null
    }
    await refresh('status', true)
    return v
  }

  const enrol = async (): Promise<Status> => {
    const s = active()
    const v = await s.client.enrol()
    setStatus(s, v)
    await Promise.all([refresh('approvals', true), refresh('settings', true)])
    return v
  }

  const pause = async (): Promise<Status> => {
    const s = active()
    const v = await s.client.pause()
    setStatus(s, v)
    return v
  }

  const resume = async (reason: ResumeReason = 'user'): Promise<Status> => {
    const s = active()
    const v = await s.client.resume(reason)
    setStatus(s, v)
    return v
  }

  // unenrol disables the agent (and with deleteMemory deletes its memory
  // folder); everything is read again, so the enrolment shows.
  const unenrol = async (deleteMemory: boolean): Promise<void> => {
    const s = active()
    try {
      await s.client.unenrol(deleteMemory)
    } catch (e) {
      // the tokens may be gone even though a later step failed
      await refresh('status', true)
      throw e
    }
    await Promise.all(RESOURCES.map((k) => refresh(k, true)))
  }

  // withStatus runs a token action and refreshes the status afterwards,
  // also after a failure (the token may have changed anyway).
  const withStatus = async <T>(call: () => Promise<T>): Promise<T> => {
    try {
      return await call()
    } finally {
      await refresh('status', true)
    }
  }

  const renewToken = (): Promise<Renewed> => {
    const s = active()
    return withStatus(() => s.client.renewToken())
  }

  const revokeToken = (): Promise<Revoked> => {
    const s = active()
    return withStatus(() => s.client.revokeToken())
  }

  // previewRevokeAll lists all of the user's app tokens for the incident
  // revoke's confirmation.
  const previewRevokeAll = (): Promise<RevokeAllPreview> => {
    const s = active()
    return s.client.previewRevokeAll(s.abort.signal)
  }

  // revokeAll is the incident revoke: ALL of the user's app tokens.
  const revokeAll = (): Promise<Revoked> => {
    const s = active()
    return withStatus(() => s.client.revokeAll())
  }

  return {
    status,
    approvals,
    settings,
    loading,
    errors,
    error,
    connection,
    lastRefreshed,
    refreshAll,
    refreshStatus,
    refreshApprovals,
    refreshSettings,
    decide,
    bulkDecide,
    acknowledge,
    viewOf,
    saveSettings,
    enrol,
    pause,
    resume,
    unenrol,
    renewToken,
    revokeToken,
    previewRevokeAll,
    revokeAll,
    subscribe,
    session: currentSession,
    start,
    stop
  }
})
