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
// The most pages of new events read after a change; with more the newest
// page is read afresh.
const MAX_NEW_PAGES = 10
// The most events kept while new ones arrive; the oldest are dropped (and
// can be paged back to as older events).
export const MAX_KEPT_EVENTS = 1000

export type VerifyResult = 'ok' | 'broken'

interface Page {
  events: AuditEvent[]
  next: string | null // the server's cursor of the next page
}

const isEvent = (v: unknown): v is AuditEvent =>
  typeof v === 'object' &&
  v !== null &&
  Number.isSafeInteger((v as { seq?: unknown }).seq) &&
  (v as { seq: number }).seq > 0 &&
  typeof (v as { kind?: unknown }).kind === 'string'

// The cursors the server issues: the last seq of an ascending page, "d"
// and the last seq of a newest-first one.
const ASC_CURSOR = /^[1-9]\d{0,17}$/
const DESC_CURSOR = /^d[1-9]\d{0,17}$/

const pageOf = (v: unknown, desc: boolean): Page => {
  const doc = v as Record<string, unknown> | null
  const items = doc?.events
  if (!Array.isArray(items) || !items.every(isEvent)) throw new ApiError(502, 'internal')
  const n = doc?.next_cursor
  if (n === null || n === undefined) return { events: items, next: null }
  if (typeof n !== 'string' || !(desc ? DESC_CURSOR : ASC_CURSOR).test(n)) throw new ApiError(502, 'internal')
  return { events: items, next: n }
}

const isCount = (v: unknown): v is number => Number.isSafeInteger(v) && (v as number) >= 0

// merge adds events to a list (newest first), by seq, without repeats.
const merge = (list: AuditEvent[], add: AuditEvent[]): AuditEvent[] => {
  const bySeq = new Map<number, AuditEvent>()
  for (const e of list) bySeq.set(e.seq, e)
  for (const e of add) if (!bySeq.has(e.seq)) bySeq.set(e.seq, e)
  return [...bySeq.values()].sort((a, b) => b.seq - a.seq)
}

// useAuditStore holds the activity screen's data: the user's audit log
// newest first (all of it or one kind, filtered by the server), the chain
// check and the export.
//
// The newest page and older pages are read newest first, following the
// server's cursors; new events are read forward (ascending) after the
// newest seq of the log read so far. With a kind filter that seq is looked
// up once per load (the kind's own newest event may be far back).
export const useAuditStore = defineStore('ocis-agent-audit', () => {
  const agent = useAgentStore()

  const events = shallowRef<AuditEvent[]>([])
  const loading = ref(false)
  const loaded = ref(false)
  const error = ref<string | null>(null)
  const olderLoading = ref(false)
  // the cursor of the page below the oldest event kept (null: none older)
  const older = ref<string | null>(null)
  const hasOlder = computed(() => loaded.value && older.value !== null)

  const kind = ref('')

  const verifying = ref(false)
  const verifyResult = ref<VerifyResult | null>(null)
  const verifyChecked = ref<number | null>(null) // the events verified
  const verifyBrokenAt = ref<number | null>(null) // the seq where the chain breaks
  const verifyError = ref<string | null>(null)

  const exporting = ref(false)
  const exportBytes = ref(0)
  const exportError = ref<string | null>(null)
  let exportAbort: AbortController | null = null

  let bound: SessionHandle | null = null
  let gen = 0 // grows on reset, a kind change and every fresh load: older answers are dropped
  let epoch = 0 // grows on reset only
  let listEpoch = 0 // grows on reset and a kind change
  let newest = 0 // the highest seq of the log read (0: none)

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

  const filterOf = (k: string) => (k !== '' ? { kind: k } : {})

  // newestFirst reads a page newest first, below the cursor if one is given.
  const newestFirst = async (s: SessionHandle, k: string, cursor: string | null, limit = AUDIT_PAGE): Promise<Page> =>
    pageOf(
      await s.client.listAudit(
        { order: 'desc', limit, ...(cursor !== null ? { cursor } : {}), ...filterOf(k) },
        s.signal
      ),
      true
    )

  // forward reads every event of the kind after `after`, following
  // next_cursor; it returns null when more than maxPages pages follow or
  // the server's cursor does not advance (only cursors the server issued
  // are sent: one that names no event of the log is refused).
  const forward = async (s: SessionHandle, k: string, after: number, maxPages: number): Promise<AuditEvent[] | null> => {
    const out: AuditEvent[] = []
    let c = after
    for (let i = 0; i < maxPages; i++) {
      const p = pageOf(
        await s.client.listAudit({ limit: AUDIT_PAGE, ...(c > 0 ? { cursor: String(c) } : {}), ...filterOf(k) }, s.signal),
        false
      )
      out.push(...p.events.filter((e) => e.seq > after))
      if (p.next === null) return out
      const next = Number(p.next)
      if (next <= c) return null
      c = next
    }
    return null
  }

  // load reads the newest page from scratch.
  const load = async (s: SessionHandle) => {
    const g = ++gen
    const k = kind.value
    // a page of older events read meanwhile belongs to the old list
    olderInflight = null
    olderLoading.value = false
    // with a filter, the newest seq of the whole log: new events are read
    // after it (read first, so nothing between the two reads is missed)
    const top = k !== '' ? ((await newestFirst(s, '', null, 1)).events[0]?.seq ?? 0) : 0
    const p = await newestFirst(s, k, null)
    if (g !== gen) return
    events.value = merge([], p.events)
    older.value = p.next
    newest = Math.max(top, events.value[0]?.seq ?? 0)
    loaded.value = true
  }

  // readNew adds the events after the newest one read.
  const readNew = async (s: SessionHandle) => {
    const g = gen
    const got = await forward(s, kind.value, newest, MAX_NEW_PAGES)
    if (g !== gen) return
    if (got === null) {
      await load(s)
      return
    }
    if (got.length === 0) return
    const all = merge(events.value, got)
    newest = Math.max(newest, all[0].seq)
    if (all.length > MAX_KEPT_EVENTS) {
      // the dropped events are older events again: below the oldest kept
      older.value = 'd' + String(all[MAX_KEPT_EVENTS - 1].seq)
      // a page of older events read meanwhile belongs to the old list
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
    const ep = listEpoch
    loading.value = true
    try {
      if (loaded.value) await readNew(s)
      else await load(s)
      if (ep === listEpoch) error.value = null
    } catch (e) {
      if (ep !== listEpoch || isAbortError(e)) return
      error.value = describe(s, e)
      agent.reportError(e)
    } finally {
      if (ep === listEpoch) loading.value = false
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
    const cursor = older.value
    if (!s || !loaded.value || cursor === null) return Promise.resolve()
    const g = gen
    olderLoading.value = true
    const p = (async () => {
      try {
        const page = await newestFirst(s, kind.value, cursor)
        if (g !== gen) return
        events.value = merge(events.value, page.events)
        older.value = page.next
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

  const clearList = () => {
    gen++
    listEpoch++
    sync.clear()
    olderInflight = null
    events.value = []
    newest = 0
    older.value = null
    loading.value = false
    loaded.value = false
    error.value = null
    olderLoading.value = false
  }

  // setKind shows only the events of one kind ('' for all), read afresh
  // from the server.
  const setKind = (k: string): Promise<void> => {
    const next = typeof k === 'string' ? k : ''
    if (next === kind.value && (loaded.value || loading.value)) return Promise.resolve()
    if (!session()) return Promise.resolve()
    clearList()
    kind.value = next
    return sync()
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
    verifyChecked.value = null
    verifyBrokenAt.value = null
    verifyError.value = null
    const run = async () => {
      try {
        const v = (await s.client.verifyAudit(s.signal)) as { ok?: unknown; checked?: unknown; broken_at?: unknown }
        if (ep !== epoch) return
        if (typeof v?.ok !== 'boolean' || !isCount(v.checked)) throw new ApiError(502, 'internal')
        const at = v.ok ? undefined : v.broken_at
        if (at !== undefined && !(isCount(at) && at > 0)) throw new ApiError(502, 'internal')
        verifyResult.value = v.ok ? 'ok' : 'broken'
        verifyChecked.value = v.checked
        verifyBrokenAt.value = at ?? null
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
    epoch++
    clearList()
    verifyInflight = null
    abortExport()
    kind.value = ''
    verifying.value = false
    verifyResult.value = null
    verifyChecked.value = null
    verifyBrokenAt.value = null
    verifyError.value = null
    exportBytes.value = 0
    exportError.value = null
    bound = agent.session()
  }

  return {
    events,
    loading,
    loaded,
    error,
    olderLoading,
    hasOlder,
    kind,
    verifying,
    verifyResult,
    verifyChecked,
    verifyBrokenAt,
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
