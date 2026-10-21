<template>
  <section class="agent-approvals agent-page">
    <header class="agent-page-head agent-page-head--sticky">
      <div class="agent-page-titles">
        <h1 class="agent-page-title" v-text="title" />
      </div>
    </header>
    <div class="agent-page-body">

      <section v-if="verify.length > 0" class="agent-verify" aria-labelledby="agent-verify-h">
        <h2 id="agent-verify-h" class="agent-section-title" v-text="$gettext('Outcome unknown — please verify')" />
        <p
          v-text="
            $gettext(
              'The agent may or may not have carried out these actions: the outcome was not recorded. Check your files to see whether each one happened. The agent never repeats them on its own.'
            )
          "
        />
        <ApprovalCard v-for="a in verify" :key="a.id" :approval="a" />
      </section>

      <section class="agent-pending" aria-labelledby="agent-pending-h">
        <h2 id="agent-pending-h" class="agent-section-title" v-text="$gettext('Waiting for your decision')" />
        <p v-if="job" class="agent-approvals-filter agent-notice agent-notice--info">
          <span v-text="$gettext('Showing the approvals of one task.')" />
          <router-link :to="{ name: `${APPID}-approvals` }" v-text="$gettext('Show all approvals')" />
        </p>
        <ErrorState
          v-if="store.errors.approvals && (!hasData || !store.serviceDown)"
          class="agent-approvals-error"
          :title="store.errors.approvals"
          :icon="store.serviceDown ? 'wifi-off' : 'error-warning'"
          :compact="hasData"
          :busy="store.loading.approvals"
          @retry="retryInbox"
        />
        <ErrorState
          v-if="job && jobError && !(store.errors.approvals && !hasData) && (jobList === null || !store.serviceDown)"
          class="agent-approvals-job-error"
          :title="jobError"
          :icon="store.serviceDown ? 'wifi-off' : 'error-warning'"
          :compact="jobList !== null"
          :busy="jobLoading"
          @retry="retryJob"
        />
        <LoadingState v-if="!shownLoaded" variant="cards" :count="2" :label="$gettext('Loading the approvals')" />
        <template v-else>
          <EmptyState
            v-if="groups.length === 0 && !store.errors.approvals && !(job && (jobError || !jobComplete))"
            class="agent-approvals-empty"
            icon="checkbox-circle"
            :title="
              job
                ? $gettext('Nothing of this task needs your approval')
                : verify.length > 0
                  ? $gettext('No other actions are waiting for your decision')
                  : $gettext('Nothing needs your approval')
            "
          />
          <JobGroup
            v-for="g in groups"
            :key="g.jobId"
            :job-id="g.jobId"
            :items="g.items"
            :goal="goals[g.jobId]"
            :trigger="triggers[g.jobId]"
          />
          <p
            v-if="job ? !jobComplete : !store.approvals.complete"
            class="agent-approvals-more agent-notice agent-notice--info"
            v-text="$gettext('More approvals are waiting than are shown here. Decide these to see the rest.')"
          />
        </template>
      </section>

      <section class="agent-history" aria-labelledby="agent-history-toggle">
        <h2 class="agent-history-head agent-section-title">
          <oc-button
            id="agent-history-toggle"
            appearance="raw"
            class="agent-history-toggle"
            :aria-expanded="historyOpen ? 'true' : 'false'"
            aria-controls="agent-history-body"
            @click="toggleHistory"
          >
            <oc-icon :name="historyOpen ? 'arrow-down-s' : 'arrow-right-s'" size="small" />
            <span v-text="$gettext('History')" />
          </oc-button>
        </h2>
        <div v-if="historyOpen" id="agent-history-body">
          <ApprovalCard v-for="a in history" :key="a.id" :approval="a" />
          <p
            v-if="historyDone && !historyCursor && history.length === 0"
            v-text="$gettext('No decided approvals yet')"
          />
          <ErrorState
            v-if="historyError && (history.length === 0 || !store.serviceDown)"
            class="agent-history-error"
            :compact="history.length > 0"
            :title="historyError"
            :busy="historyLoading"
            @retry="loadHistory"
          />
          <LoadingState v-if="historyLoading" variant="spinner" :label="$gettext('Loading the history')" />
          <oc-button
            v-else-if="historyCursor && !historyError"
            appearance="outline"
            @click="loadHistory"
            v-text="$gettext('Load more')"
          />
        </div>
      </section>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, reactive, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import { createClient } from '../api/client'
import { describeError, isAbortError } from '../api/errors'
import type { Approval, ApprovalState } from '../api/types'
import EmptyState from '../components/state/EmptyState.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import ApprovalCard from './approvals/ApprovalCard.vue'
import JobGroup from './approvals/JobGroup.vue'
import { provideDecisions } from './approvals/decisions'
import { APPID } from '../appid'

// job (the route's ?job=) shows only that job's pending approvals.
const props = defineProps<{ job?: string }>()

// HISTORY_PAGE is the page size of the history.
const HISTORY_PAGE = 50
// GOAL_RETRY_MS: a failed goal lookup is tried again on a later change of
// the list, at most this often.
const GOAL_RETRY_MS = 60_000

// The approvals inbox: actions whose outcome must be verified first, then
// the pending approvals grouped by job (newest first), then the decided
// ones (history), loaded only when opened. The pending lists come from the
// store; goals and history are read here and never block the inbox.
const store = useAgentStore()
const platform = usePlatform()
const client = createClient(platform)
const { $gettext, interpolate } = useGettext()
provideDecisions()

const abort = new AbortController()
onBeforeUnmount(() => abort.abort())

// The title counts the approvals that can still be decided: the loaded
// ones, or the status's exact count when the list is cut off (never fewer
// than are shown, since the status may be older than the list).
const liveCount = computed(() => {
  const loaded = store.approvals.counts.live
  const exact = store.status?.pending_approvals
  if (store.approvals.complete || typeof exact !== 'number') return loaded
  return Math.max(loaded, exact)
})
const title = computed(() => interpolate($gettext('Approvals (%{count})'), { count: liveCount.value }, true))

// loaded: the pending list has been read (or failed) at least once in
// this session. A reset store (no status) has not loaded anything yet.
const loaded = ref(store.status !== null && !store.loading.approvals)
watch(
  () => [store.status === null, store.loading.approvals] as const,
  ([reset, busy]) => {
    if (reset) loaded.value = false
    else if (!busy) loaded.value = true
  }
)

// With a job filter the job's approvals are read with the API's job_id
// filter (the inbox list may be cut off), again whenever the inbox list
// changes; null until the first read answered.
const JOB_PAGE = 100
const JOB_MAX_PAGES = 10
const jobList = ref<Approval[] | null>(null)
const jobComplete = ref(false)
const jobError = ref<string | null>(null)
const jobLoading = ref(false)
let jobAgain = false

const loadJob = async () => {
  const job = props.job
  if (!job) return
  if (jobLoading.value) {
    jobAgain = true
    return
  }
  jobLoading.value = true
  try {
    const out: Approval[] = []
    let cursor: string | undefined
    let complete = false
    for (let i = 0; i < JOB_MAX_PAGES; i++) {
      const page = await client.listApprovals({ jobId: job, limit: JOB_PAGE, cursor }, abort.signal)
      out.push(...(Array.isArray(page?.approvals) ? page.approvals : []))
      if (typeof page?.next_cursor !== 'string' || page.next_cursor === '') {
        complete = true
        break
      }
      cursor = page.next_cursor
    }
    if (props.job === job) {
      jobList.value = out.filter((a) => a.job_id === job)
      jobComplete.value = complete
      jobError.value = null
    }
  } catch (e) {
    if (!isAbortError(e) && props.job === job) jobError.value = describeError(e, $gettext)
  } finally {
    jobLoading.value = false
  }
  if (jobAgain) {
    jobAgain = false
    void loadJob()
  }
}

watch(
  () => props.job,
  () => {
    jobList.value = null
    jobComplete.value = false
    jobError.value = null
    void loadJob()
  },
  { immediate: true }
)
watch(
  () => store.approvals,
  () => {
    if (props.job) void loadJob()
  }
)

// hasData: the inbox list has been read successfully at least once in this
// session (a failed first read leaves an empty list that is not data).
const hasData = ref(store.status !== null && !store.loading.approvals && !store.errors.approvals)
watch(
  () => [store.status === null, store.loading.approvals, store.errors.approvals] as const,
  ([reset, busy, err]) => {
    if (reset) hasData.value = false
    else if (!busy && !err) hasData.value = true
  }
)

const retryInbox = () => {
  void store.refreshApprovals()
  if (store.serviceDown) void store.retryNow()
}
const retryJob = () => {
  void loadJob()
  if (store.serviceDown) void store.retryNow()
}

const shownLoaded = computed(() => (props.job ? jobList.value !== null || jobError.value !== null : loaded.value))

const groups = computed(() => {
  const byJob = new Map<string, Approval[]>()
  for (const a of props.job ? (jobList.value ?? []) : store.approvals.pending) {
    const list = byJob.get(a.job_id)
    if (list) list.push(a)
    else byJob.set(a.job_id, [a])
  }
  return [...byJob].map(([jobId, items]) => ({ jobId, items }))
})

// goals: a job's goal, null when it could not be read; absent while
// unknown. Each job is looked up once (the API has no lighter view of one
// job than its detail); a failed lookup is tried again on a later change
// of the list, at most once per GOAL_RETRY_MS.
const goals = reactive<Record<string, string | null>>({})
// triggers: a job's trigger kind, read with its goal.
const triggers = reactive<Record<string, string | null>>({})
const asked = new Map<string, number | 'ok'>() // failure time, or 'ok'
watch(
  groups,
  (gs) => {
    for (const { jobId } of gs) {
      const prev = asked.get(jobId)
      if (prev === 'ok' || (prev !== undefined && platform.now() - prev < GOAL_RETRY_MS)) continue
      asked.set(jobId, 'ok')
      client.getJob(jobId, abort.signal).then(
        (j) => {
          goals[jobId] = typeof j?.goal === 'string' && j.goal !== '' ? j.goal : null
          triggers[jobId] = typeof j?.trigger_kind === 'string' && j.trigger_kind !== '' ? j.trigger_kind : null
        },
        () => {
          asked.set(jobId, platform.now())
          if (!(jobId in goals)) goals[jobId] = null
        }
      )
    }
  },
  { immediate: true }
)

// verify: the actions whose outcome the user should check, as the server
// lists them (needs_verification), until the user confirms they checked.
const verify = computed(() => store.approvals.verify)

const historyOpen = ref(false)
const history = ref<Approval[]>([])
const historyCursor = ref<string | null>(null)
const historyLoading = ref(false)
const historyError = ref<string | null>(null)
const historyDone = ref(false)

const loadHistory = async () => {
  if (historyLoading.value) return
  historyLoading.value = true
  historyError.value = null
  try {
    const page = await client.listApprovals(
      { state: 'all', limit: HISTORY_PAGE, cursor: historyCursor.value ?? undefined },
      abort.signal
    )
    const list = Array.isArray(page?.approvals) ? page.approvals : []
    const seen = new Set(history.value.map((a) => a.id))
    const decided = list.filter((a) => a.state !== ('pending' satisfies ApprovalState) && !seen.has(a.id))
    history.value = [...history.value, ...decided]
    historyCursor.value = typeof page?.next_cursor === 'string' ? page.next_cursor : null
    historyDone.value = true
  } catch (e) {
    if (!isAbortError(e)) historyError.value = describeError(e, $gettext)
  } finally {
    historyLoading.value = false
  }
}

const toggleHistory = () => {
  historyOpen.value = !historyOpen.value
  if (historyOpen.value && !historyDone.value) void loadHistory()
}
</script>

<style>
.agent-approvals .agent-page-body {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-5);
}
.agent-approvals .agent-verify {
  border-color: var(--oc-color-swatch-danger-default);
  border-width: 2px;
  background: color-mix(in srgb, var(--oc-color-swatch-danger-default) 5%, var(--oc-color-background-default));
}
.agent-approvals .agent-verify > p {
  margin: 0;
}
.agent-pending,
.agent-history,
#agent-history-body {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
}
.agent-history-head {
  margin: 0;
}
.agent-history-toggle {
  display: inline-flex;
  align-items: center;
  gap: var(--agent-space-1);
  font: inherit;
  font-weight: 600;
}
.agent-history-toggle .oc-icon svg {
  fill: currentColor;
}
</style>
