import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { useAgentStore, type SessionHandle, type StoreEvent } from './agent'
import { ApiError, describeError, isAbortError } from '../api/errors'
import type { AuditEvent } from '../api/types'
import { coalesce } from '../util/coalesce'
import { formatDuration } from '../util/format'
import { saveBlob } from '../util/download'

// Events per page (the server caps audit pages at 200).
export const AUDIT_PAGE = 50
// The most lookups to find the newest events of a log.
const MAX_PROBES = 64
// The most pages of new events read after a change; with more the newest
// page is looked up afresh.
const MAX_NEW_PAGES = 10
// The most events kept while new ones arrive; the oldest are dropped (and
// can be paged back to as older events).
export const MAX_KEPT_EVENTS = 1000

export type VerifyResult = 'ok' | 'broken'

interface Page {
  events: AuditEvent[]
  next: number | null
}

const isEvent = (v: unknown): v is AuditEvent =>
  typeof v === 'object' &&
  v !== null &&
  Number.isSafeInteger((v as { seq?: unknown }).seq) &&
  (v as { seq: number }).seq > 0 &&
  typeof (v as { kind?: unknown }).kind === 'string'

const pageOf = (v: unknown): Page => {
  const doc = v as Record<string, unknown> | null
  const items = doc?.events
  if (!Array.isArray(items) || !items.every(isEvent)) throw new ApiError(502, 'internal')
  const n = doc?.next_cursor
  const next = typeof n === 'string' && /^[1-9]\d{0,17}$/.test(n) ? Number(n) : null
  if (n !== null && n !== undefined && next === null) throw new ApiError(502, 'internal')
  return { events: items, next }
}

// merge adds events to a list (newest first), by seq, without repeats.
const merge = (list: AuditEvent[], add: AuditEvent[]): AuditEvent[] => {
  const bySeq = new Map<number, AuditEvent>()
  for (const e of list) bySeq.set(e.seq, e)
  for (const e of add) if (!bySeq.has(e.seq)) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((a, b) => b.seq - a.seq)
}

// useAuditStore holds the activity screen's data: the user's audit log
// newest first, the chain check and the export.
//
// The API lists the log ascending by seq (the cursor is the seq after
// which a page starts) and seqs count up from 1. The newest events are
// found by looking ahead with one-event pages (doubling the step, then
// halving the range) and then read as one page; older pages are read by
// seq range below the oldest loaded event, new events forward from the
// newest one. Gaps in the seqs only make pages shorter.
export const useAuditStore = defineStore('ocis-agent-audit', () => {
  const agent = useAgentStore()

  const events = shallowRef<AuditEvent[]>([])
  const loading = ref(false)
  const loaded = ref(false)
  const error = ref<string | null>(null)
  const olderLoading = ref(false)
  // every event with a seq above floor (up to the newest) has been read
  const floor = ref(0)
  const hasOlder = computed(() => loaded.value && floor.value > 0)

  const kind = ref('')
  const shown = computed(() => (kind.value === '' ? events.value : events.value.filter((e) => e.kind === kind.value)))

  const verifying = ref(false)
  const verifyResult = ref<VerifyResult | null>(null)
  const verifyError = ref<string | null>(null)

  const exporting = ref(false)
  const exportBytes = ref(0)
  const exportError = ref<string | null>(null)
  let exportAbort: AbortController | null = null

  let bound: SessionHandle | null = null
  let gen = 0 // grows on reset and every fresh load: older answers are dropped
  let epoch = 0 // grows on reset only
  let newest = 0 // the highest seq read (0: none)

  const session = (): SessionHandle | null => {
    const s = agent.session()
    if (s !== bound) {
      if (bound !== null) reset()
      bound = s
    }
    return s
  }

  // syncSession drops the data of an earlier session (call it before the
  // screen first shows what the store holds).
  const syncSession = (): void => {
    session()
  }

  const describe = (s: SessionHandle, e: unknown) => describeError(e, s.gettext)
  const fmtSeconds = (secs: number) => formatDuration(Math.max(1, secs) * 1000)

  const list = async (s: SessionHandle, after: number, limit: number): Promise<Page> =>
    pageOf(await s.client.listAudit({ limit, ...(after > 0 ? { cursor: String(after) } : {}) }, s.signal))

  // forward reads every event after `after`, following next_cursor; it
  // returns null when more than maxPages pages follow.
  const forward = async (s: SessionHandle, after: number, maxPages: number): Promise<AuditEvent[] | null> => {
    const out: AuditEvent[] = []
    let c = after
    for (let i = 0; i < maxPages; i++) {
      const p = await list(s, c, AUDIT_PAGE)
      out.push(...p.events.filter((e) => e.seq > after))
      if (p.next === null) return out
      c = Math.max(p.next, c + 1)
    }
    return null
  }

  // newestPage finds the newest page of the log: the events and the floor
  // below them.
  const newestPage = async (s: SessionHandle): Promise<{ events: AuditEvent[]; floor: number }> => {
    const first = await list(s, 0, AUDIT_PAGE)
    if (first.next === null) return { events: first.events, floor: 0 }
    // lo: a seq that exists with more after it; hi: nothing after it
    let lo = first.events.length > 0 ? first.events[first.events.length - 1].seq : first.next
    let hi: number | null = null
    let probes = 0
    const probe = async (c: number) => {
      if (++probes > MAX_PROBES) throw new ApiError(502, 'internal')
      return list(s, c, 1)
    }
    for (let step = AUDIT_PAGE * 4; hi === null; step *= 2) {
      const c = lo + step
      const p = await probe(c)
      if (p.events.length === 0) {
        if (p.next === null) hi = c
        else lo = Math.max(lo, p.next)
      } else if (p.next === null) {
        lo = hi = p.events[0].seq
      } else {
        lo = p.events[0].seq
      }
    }
    while (hi - lo > AUDIT_PAGE) {
      const mid = Math.floor((lo + hi) / 2)
      const p = await probe(mid)
      if (p.events.length === 0 && p.next === null) hi = mid
      else if (p.events.length > 0 && p.next === null) lo = hi = p.events[0].seq
      else lo = Math.max(lo, p.events[0]?.seq ?? p.next ?? lo, mid)
    }
    // the tail is in [lo, hi]: a page below lo covers at least one page
    const start = Math.max(0, lo - AUDIT_PAGE)
    const got = await forward(s, start, MAX_NEW_PAGES)
    if (got === null) throw new ApiError(502, 'internal')
    // keep one page: the floor is then the newest event left out
    const sorted = merge([], got)
    const left = sorted.slice(AUDIT_PAGE)
    return { events: sorted.slice(0, AUDIT_PAGE), floor: left.length > 0 ? left[0].seq : start }
  }

  // load reads the newest page from scratch.
  const load = async (s: SessionHandle) => {
    const g = ++gen
    // a page of older events read meanwhile belongs to the old floor
    olderInflight = null
    olderLoading.value = false
    const p = await newestPage(s)
    if (g !== gen) return
    events.value = merge([], p.events)
    floor.value = p.floor
    newest = events.value[0]?.seq ?? p.floor
    loaded.value = true
  }

  // readNew adds the events after the newest one read.
  const readNew = async (s: SessionHandle) => {
    const g = gen
    const got = await forward(s, newest, MAX_NEW_PAGES)
    if (g !== gen) return
    if (got === null) {
      await load(s)
      return
    }
    if (got.length === 0) return
    const all = merge(events.value, got)
    newest = Math.max(newest, all[0].seq)
    if (all.length > MAX_KEPT_EVENTS) {
      // every event above the newest dropped one is still kept
      floor.value = Math.max(floor.value, all[MAX_KEPT_EVENTS].seq)
      // a page of older events read meanwhile belongs to the old floor
      gen++
      olderInflight = null
      olderLoading.value = false
      events.value = all.slice(0, MAX_KEPT_EVENTS)
    } else {
      events.value = all
    }
  }

  const sync = coalesce(async () => {
    const s = session()
    if (!s) return
    const ep = epoch
    loading.value = true
    try {
      if (loaded.value) await readNew(s)
      else await load(s)
      if (ep === epoch) error.value = null
    } catch (e) {
      if (ep !== epoch || isAbortError(e)) return
      error.value = describe(s, e)
    } finally {
      if (ep === epoch) loading.value = false
    }
  })

  // refresh reads the newest events (the first time) or the ones that
  // came after the newest one read.
  const refresh = (): Promise<void> => sync()

  let olderInflight: Promise<void> | null = null
  // loadOlder reads the page of events below the oldest one read.
  const loadOlder = (): Promise<void> => {
    if (olderInflight) return olderInflight
    const s = session()
    const to = floor.value
    if (!s || !loaded.value || to === 0) return Promise.resolve()
    const g = gen
    const from = Math.max(0, to - AUDIT_PAGE)
    olderLoading.value = true
    const p = (async () => {
      try {
        const page = await list(s, from, to - from)
        if (g !== gen) return
        events.value = merge(
          events.value,
          page.events.filter((e) => e.seq > from && e.seq <= to)
        )
        floor.value = from
        error.value = null
      } catch (e) {
        if (g !== gen || isAbortError(e)) return
        error.value = describe(s, e)
      } finally {
        // load() and reset() drop the promise of an older generation
        if (g === gen) {
          olderLoading.value = false
          olderInflight = null
        }
      }
    })()
    olderInflight = p
    return p
  }

  // setKind shows only the loaded events of one kind ('' for all).
  const setKind = (k: string) => {
    kind.value = typeof k === 'string' ? k : ''
  }

  // --- verify

  let verifyInflight: Promise<void> | null = null
  // verify asks the server to check the hash chain of the log.
  const verify = (): Promise<void> => {
    if (verifyInflight) return verifyInflight
    const s = session()
    if (!s) return Promise.resolve()
    const ep = epoch
    verifying.value = true
    verifyResult.value = null
    verifyError.value = null
    const run = async () => {
      try {
        const v = (await s.client.verifyAudit(s.signal)) as { ok?: unknown }
        if (ep !== epoch) return
        if (typeof v?.ok !== 'boolean') throw new ApiError(502, 'internal')
        verifyResult.value = v.ok ? 'ok' : 'broken'
      } catch (e) {
        if (ep !== epoch || isAbortError(e)) return
        if (e instanceof ApiError && e.status === 429) {
          verifyError.value =
            e.retryAfter !== undefined
              ? s.gettext('A check is already running. Try again in %{time}.').replace('%{time}', fmtSeconds(e.retryAfter))
              : s.gettext('A check is already running. Try again in a moment.')
        } else {
          verifyError.value = describe(s, e)
        }
      } finally {
        if (ep === epoch) {
          verifying.value = false
          verifyInflight = null
        }
      }
    }
    verifyInflight = run()
    return verifyInflight
  }

  // --- export

  // exportLog downloads the whole log as an NDJSON file.
  const exportLog = async (): Promise<void> => {
    const s = session()
    if (!s || exporting.value) return
    const ctl = new AbortController()
    exportAbort = ctl
    const onEnd = () => ctl.abort()
    s.signal.addEventListener('abort', onEnd, { once: true })
    exporting.value = true
    exportBytes.value = 0
    exportError.value = null
    try {
      const got = await s.client.exportAudit((n) => {
        if (exportAbort === ctl) exportBytes.value = n
      }, ctl.signal)
      if (ctl.signal.aborted || bound !== s) return
      saveBlob(got.blob, got.filename)
    } catch (e) {
      if (ctl.signal.aborted || bound !== s || isAbortError(e)) return
      if (e instanceof ApiError && e.status === 429) {
        exportError.value =
          e.retryAfter !== undefined
            ? s.gettext('An export is already running. Try again in %{time}.').replace('%{time}', fmtSeconds(e.retryAfter))
            : s.gettext('An export is already running. Try again in a moment.')
      } else if (e instanceof ApiError && e.status === 0 && e.code === 'unavailable') {
        exportError.value = s.gettext('The export was interrupted, so no file was saved. Try again.')
      } else {
        exportError.value = describe(s, e)
      }
    } finally {
      s.signal.removeEventListener('abort', onEnd)
      if (exportAbort === ctl) {
        exportAbort = null
        exporting.value = false
      }
    }
  }

  // abortExport stops a running export (leaving the screen does).
  const abortExport = () => {
    const ctl = exportAbort
    if (!ctl) return
    exportAbort = null
    exporting.value = false
    ctl.abort()
  }

  // --- live updates

  const onEvent = async (e: StoreEvent) => {
    if (e.type === 'reset') {
      reset()
      return
    }
    if (!session() || !loaded.value) return
    if (e.type === 'refresh' || e.kinds.includes('activity')) await sync()
  }

  // attach subscribes to the agent store's events while the activity
  // screen is shown; it returns the function that detaches again.
  const attach = (): (() => void) => {
    session()
    return agent.subscribe(onEvent)
  }

  const reset = () => {
    gen++
    epoch++
    sync.clear()
    olderInflight = null
    verifyInflight = null
    abortExport()
    events.value = []
    newest = 0
    floor.value = 0
    loading.value = false
    loaded.value = false
    error.value = null
    olderLoading.value = false
    kind.value = ''
    verifying.value = false
    verifyResult.value = null
    verifyError.value = null
    exportBytes.value = 0
    exportError.value = null
    bound = agent.session()
  }

  return {
    events,
    shown,
    loading,
    loaded,
    error,
    olderLoading,
    hasOlder,
    kind,
    verifying,
    verifyResult,
    verifyError,
    exporting,
    exportBytes,
    exportError,
    attach,
    syncSession,
    refresh,
    loadOlder,
    setKind,
    verify,
    exportLog,
    abortExport,
    reset
  }
})
