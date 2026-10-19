import { defineStore } from 'pinia'
import { computed, ref, shallowRef } from 'vue'
import { useAgentStore, type SessionHandle, type StoreEvent } from './agent'
import { ApiError, describeError, isAbortError } from '../api/errors'
import { coalesce } from '../util/coalesce'
import { byId, listOf, mergeHead } from '../util/lists'
import type { Approval, Job, JobDetail, JobState } from '../api/types'

// Page sizes (the server caps job pages at 200, approval pages at 100).
export const JOB_PAGE = 50
export const JOB_APPROVALS_LIMIT = 20

// The list filters: every job state the API filters by (one per request),
// after '' for all of them.
export const JOB_FILTERS: readonly string[] = [
  '',
  'queued' satisfies JobState,
  'running' satisfies JobState,
  'waiting_approval' satisfies JobState,
  'succeeded' satisfies JobState,
  'failed' satisfies JobState,
  'cancelled' satisfies JobState
]

// The states in which a job can still change and be cancelled.
const LIVE_STATES: readonly string[] = [
  'queued' satisfies JobState,
  'running' satisfies JobState,
  'waiting_approval' satisfies JobState
]

export const isLiveState = (state: string | undefined): boolean => state !== undefined && LIVE_STATES.includes(state)

export interface DetailApprovals {
  items: Approval[]
  more: boolean // more pending approvals than were read
  error: string | null
}

const isJob = (v: unknown): v is Job =>
  typeof v === 'object' && v !== null && typeof (v as { id?: unknown }).id === 'string'

const count = (n: unknown): number => (typeof n === 'number' && Number.isFinite(n) && n > 0 ? n : 0)

// useJobsStore holds the tasks screen's data: the job list (newest first,
// one state filter) and the open job with its steps and pending
// approvals. It refreshes on the agent store's events only while attached
// (the tasks screen is mounted), and only what was read before.
export const useJobsStore = defineStore('ocis-agent-jobs', () => {
  const agent = useAgentStore()

  const jobs = shallowRef<Job[]>([])
  const cursor = ref<string | null>(null)
  const filter = ref('')
  const loading = ref(false)
  const loaded = ref(false)
  const error = ref<string | null>(null)
  const moreLoading = ref(false)

  const detailId = ref<string | null>(null)
  const detail = shallowRef<JobDetail | null>(null)
  const detailLoading = ref(false)
  const detailLoaded = ref(false)
  const detailError = ref<string | null>(null)
  const detailMissing = ref(false)
  const detailApprovals = shallowRef<DetailApprovals | null>(null)

  const cancelling = ref(false)
  const actionError = ref<string | null>(null)

  const tokens = computed(() => {
    const out = { in: 0, out: 0 }
    for (const s of detail.value?.steps ?? []) {
      out.in += count(s.tokens_in)
      out.out += count(s.tokens_out)
    }
    return out
  })

  let bound: SessionHandle | null = null
  let listGen = 0 // grows on reset and filter change: older list answers are dropped
  let detailGen = 0 // grows on reset and every open
  let listed = false // the list was asked for: events refresh it
  let moreGen = 0 // grows when a refresh replaced the list: older pages read meanwhile are dropped

  const session = (): SessionHandle | null => {
    const s = agent.session()
    if (s !== bound) {
      if (bound !== null) reset()
      bound = s
    }
    return s
  }

  // syncSession drops the data of an earlier session (call it before
  // relying on what the store holds, such as the open job).
  const syncSession = (): void => {
    session()
  }

  const describe = (s: SessionHandle, e: unknown) => describeError(e, s.gettext)

  const params = (c?: string) => ({
    limit: JOB_PAGE,
    ...(filter.value !== '' ? { state: filter.value } : {}),
    ...(c !== undefined ? { cursor: c } : {})
  })

  // --- the list

  const listRefresh = coalesce(async () => {
    const s = session()
    if (!s) return
    listed = true
    const g = listGen
    loading.value = true
    try {
      const v = await s.client.listJobs(params(), s.signal)
      if (g !== listGen) return
      const r = mergeHead(jobs.value, listOf<Job>(v, 'jobs'), cursor.value)
      if (r.replaced) {
        moreGen++
        moreInflight = null
        moreLoading.value = false
      }
      jobs.value = r.items
      cursor.value = r.cursor
      error.value = null
      loaded.value = true
    } catch (e) {
      if (g !== listGen || isAbortError(e)) return
      error.value = describe(s, e)
    } finally {
      if (g === listGen) loading.value = false
    }
  })

  // refresh reads the first page (merged with the pages already read).
  const refresh = (): Promise<void> => listRefresh()

  let moreInflight: Promise<void> | null = null
  const loadMore = (): Promise<void> => {
    if (moreInflight) return moreInflight
    const s = session()
    const c = cursor.value
    if (!s || c === null) return Promise.resolve()
    const g = listGen
    const mg = moreGen
    moreLoading.value = true
    const p = (async () => {
      try {
        const v = await s.client.listJobs(params(c), s.signal)
        if (g !== listGen || mg !== moreGen) return
        const page = listOf<Job>(v, 'jobs')
        const seen = byId(jobs.value)
        jobs.value = [...jobs.value, ...page.items.filter((j) => !seen.has(j.id))]
        cursor.value = page.next
        error.value = null
      } catch (e) {
        if (g !== listGen || mg !== moreGen || isAbortError(e)) return
        error.value = describe(s, e)
      } finally {
        if (g === listGen && mg === moreGen) {
          moreLoading.value = false
          moreInflight = null
        }
      }
    })()
    moreInflight = p
    return p
  }

  const clearList = () => {
    listGen++
    listRefresh.clear()
    moreInflight = null
    jobs.value = []
    cursor.value = null
    loading.value = false
    loaded.value = false
    moreLoading.value = false
    error.value = null
  }

  // setFilter shows only the jobs in one state ('' for all); a value the
  // API does not know is ignored.
  const setFilter = async (state: string): Promise<void> => {
    if (!JOB_FILTERS.includes(state)) return
    if (!session()) return
    clearList()
    filter.value = state
    await listRefresh()
  }

  const replaceJob = (j: Job) => {
    jobs.value = jobs.value.map((x) => (x.id === j.id ? j : x))
    if (detail.value?.id === j.id) detail.value = { ...detail.value, ...j }
  }

  const dropJob = (id: string) => {
    jobs.value = jobs.value.filter((x) => x.id !== id)
  }

  // --- the open job

  const clearDetail = () => {
    detail.value = null
    detailLoading.value = false
    detailLoaded.value = false
    detailError.value = null
    detailMissing.value = false
    detailApprovals.value = null
    actionError.value = null
  }

  const loadApprovals = async (s: SessionHandle, g: number, jobId: string) => {
    try {
      const v = await s.client.listApprovals({ jobId, limit: JOB_APPROVALS_LIMIT }, s.signal)
      if (g !== detailGen) return
      const page = listOf<Approval>(v, 'approvals')
      detailApprovals.value = { items: page.items, more: page.next !== null, error: null }
    } catch (e) {
      if (g !== detailGen || isAbortError(e)) return
      const prev = detailApprovals.value
      detailApprovals.value = { items: prev?.items ?? [], more: prev?.more ?? false, error: describe(s, e) }
    }
  }

  const detailRefresh = coalesce(async () => {
    const s = session()
    const id = detailId.value
    if (!s || id === null) return
    const g = detailGen
    detailLoading.value = true
    let live = false
    try {
      const d = await s.client.getJob(id, s.signal)
      if (g !== detailGen) return
      if (!isJob(d)) throw new ApiError(502, 'internal')
      detail.value = { ...d, steps: Array.isArray(d.steps) ? d.steps : [] }
      detailError.value = null
      detailMissing.value = false
      detailLoaded.value = true
      live = isLiveState(d.state)
      const { steps: _steps, answer: _answer, ...job } = d
      if (jobs.value.some((x) => x.id === id)) jobs.value = jobs.value.map((x) => (x.id === id ? job : x))
    } catch (e) {
      if (g !== detailGen || isAbortError(e)) return
      if (e instanceof ApiError && e.status === 404) {
        detail.value = null
        detailMissing.value = true
        detailError.value = null
        detailApprovals.value = null
        dropJob(id)
      } else {
        detailError.value = describe(s, e)
      }
      return
    } finally {
      if (g === detailGen) detailLoading.value = false
    }
    if (live) await loadApprovals(s, g, id)
    else detailApprovals.value = null
  })

  // refreshDetail reads the open job again (and the pending approvals of a
  // live one).
  const refreshDetail = (): Promise<void> => detailRefresh()

  // openJob shows the job with this id; answers for the one shown before
  // are dropped.
  const openJob = async (id: string): Promise<void> => {
    if (!session()) return
    detailGen++
    detailRefresh.clear()
    clearDetail()
    detailId.value = id
    await detailRefresh()
  }

  const closeJob = () => {
    detailGen++
    detailRefresh.clear()
    clearDetail()
    detailId.value = null
  }

  // --- cancel

  // cancel cancels a live job after the user confirmed it. A job that ended
  // or vanished meanwhile is read again and the reason shown.
  const cancel = async (id: string): Promise<boolean> => {
    const s = session()
    if (!s || cancelling.value) return false
    cancelling.value = true
    actionError.value = null
    try {
      const ok = await s.platform.confirm({
        title: s.gettext('Cancel this task?'),
        message: s.gettext(
          'The agent stops working on it. Actions that wait for your approval are not carried out. An action that is already running is not interrupted.'
        ),
        confirmText: s.gettext('Cancel task'),
        danger: true
      })
      if (!ok || bound !== s) return false
      try {
        const j = await s.client.cancelJob(id, s.signal)
        if (bound !== s) return false
        if (isJob(j)) replaceJob(j)
      } catch (e) {
        if (bound !== s || isAbortError(e)) return false
        if (e instanceof ApiError && e.status === 404) {
          actionError.value = s.gettext('This task no longer exists.')
          dropJob(id)
          if (detailId.value === id) {
            detail.value = null
            detailApprovals.value = null
            detailMissing.value = true
          }
        } else if (e instanceof ApiError && e.status === 409) {
          actionError.value = s.gettext('This task has already ended, so it cannot be cancelled.')
          await Promise.all([detailId.value === id ? detailRefresh() : undefined, listed ? listRefresh() : undefined])
        } else {
          actionError.value = describe(s, e)
        }
        return false
      }
      // its approvals can no longer be decided
      void agent.refreshApprovals()
      void agent.refreshStatus()
      // under a state filter the job may no longer belong to the list
      await Promise.all([
        detailId.value === id ? detailRefresh() : undefined,
        listed && filter.value !== '' ? listRefresh() : undefined
      ])
      return true
    } finally {
      if (bound === s) cancelling.value = false
    }
  }

  // --- live updates

  const onEvent = async (e: StoreEvent) => {
    if (e.type === 'reset') {
      reset()
      return
    }
    if (!session()) return
    const work: Promise<void>[] = []
    if (e.type === 'refresh') {
      if (listed) work.push(listRefresh())
      if (detailId.value !== null) work.push(detailRefresh())
    } else if (e.kinds.includes('jobs')) {
      if (listed) work.push(listRefresh())
      // an ended job does not change any more (a missing or unread one may)
      if (detailId.value !== null && (detail.value === null || isLiveState(detail.value.state))) {
        work.push(detailRefresh())
      }
    }
    await Promise.all(work)
  }

  // attach subscribes to the agent store's events while the tasks screen
  // is shown; it returns the function that detaches again.
  const attach = (): (() => void) => {
    session()
    return agent.subscribe(onEvent)
  }

  const reset = () => {
    clearList()
    filter.value = ''
    listed = false
    detailGen++
    detailRefresh.clear()
    clearDetail()
    detailId.value = null
    cancelling.value = false
    bound = agent.session()
  }

  return {
    jobs,
    cursor,
    filter,
    loading,
    loaded,
    error,
    moreLoading,
    detailId,
    detail,
    detailLoading,
    detailLoaded,
    detailError,
    detailMissing,
    detailApprovals,
    tokens,
    cancelling,
    actionError,
    attach,
    syncSession,
    refresh,
    loadMore,
    setFilter,
    openJob,
    closeJob,
    refreshDetail,
    cancel,
    reset
  }
})
