<template>
  <section class="agent-approvals oc-p-m">
    <h1 v-text="title" />

    <section v-if="verify.length > 0" class="agent-verify" aria-labelledby="agent-verify-h">
      <h2 id="agent-verify-h" v-text="$gettext('Outcome unknown — please verify')" />
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
      <h2 id="agent-pending-h" v-text="$gettext('Waiting for your decision')" />
      <div v-if="store.errors.approvals" class="agent-approvals-error">
        <p role="alert" v-text="store.errors.approvals" />
        <oc-button
          :disabled="store.loading.approvals"
          @click="store.refreshApprovals()"
          v-text="$gettext('Try again')"
        />
      </div>
      <oc-spinner v-if="!loaded" :aria-label="$gettext('Loading the approvals')" />
      <template v-else>
        <p
          v-if="groups.length === 0 && !store.errors.approvals"
          class="agent-approvals-empty"
          v-text="
            verify.length > 0
              ? $gettext('No other actions are waiting for your decision')
              : $gettext('Nothing needs your approval')
          "
        />
        <JobGroup v-for="g in groups" :key="g.jobId" :job-id="g.jobId" :items="g.items" :goal="goals[g.jobId]" />
        <p
          v-if="!store.approvals.complete"
          class="agent-approvals-more"
          v-text="$gettext('More approvals are waiting than are shown here. Decide these to see the rest.')"
        />
      </template>
    </section>

    <section class="agent-history" aria-labelledby="agent-history-toggle">
      <h2 class="agent-history-head">
        <oc-button
          id="agent-history-toggle"
          appearance="raw"
          :aria-expanded="historyOpen ? 'true' : 'false'"
          aria-controls="agent-history-body"
          @click="toggleHistory"
          v-text="$gettext('History')"
        />
      </h2>
      <div v-if="historyOpen" id="agent-history-body">
        <ApprovalCard v-for="a in history" :key="a.id" :approval="a" />
        <p
          v-if="historyDone && !historyCursor && history.length === 0"
          v-text="$gettext('No decided approvals yet')"
        />
        <div v-if="historyError">
          <p role="alert" v-text="historyError" />
          <oc-button :disabled="historyLoading" @click="loadHistory" v-text="$gettext('Try again')" />
        </div>
        <oc-spinner v-if="historyLoading" :aria-label="$gettext('Loading the history')" />
        <oc-button
          v-else-if="historyCursor && !historyError"
          appearance="outline"
          @click="loadHistory"
          v-text="$gettext('Load more')"
        />
      </div>
    </section>
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
import ApprovalCard from './approvals/ApprovalCard.vue'
import JobGroup from './approvals/JobGroup.vue'
import { provideDecisions } from './approvals/decisions'

// HISTORY_PAGE is the page size of the history and of the scan for
// settled actions that need verification.
const HISTORY_PAGE = 50
const VERIFY_SCAN_PAGE = 100
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

const groups = computed(() => {
  const byJob = new Map<string, Approval[]>()
  for (const a of store.approvals.pending) {
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

// settled: executed or failed approvals whose outcome is unknown too (the
// stored result was unreadable); the store lists state unknown only. The
// newest page of each is scanned once per mount, and history pages add
// what they find.
const settled = ref<Approval[]>([])
const addSettled = (list: Approval[]) => {
  const seen = new Set(settled.value.map((a) => a.id))
  const add = list.filter(
    (a) => a.needs_verification && a.state !== ('unknown' satisfies ApprovalState) && !seen.has(a.id)
  )
  if (add.length > 0) settled.value = [...settled.value, ...add]
}
for (const state of ['executed', 'failed']) {
  client.listApprovals({ state, limit: VERIFY_SCAN_PAGE }, abort.signal).then(
    (page) => addSettled(Array.isArray(page?.approvals) ? page.approvals : []),
    () => {} // best effort: the history shows them too
  )
}
const verify = computed(() => {
  const ids = new Set(store.approvals.verify.map((a) => a.id))
  return [...store.approvals.verify, ...settled.value.filter((a) => !ids.has(a.id))]
})

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
    addSettled(list)
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
.agent-approvals {
  max-width: 64rem;
  display: flex;
  flex-direction: column;
  gap: 1.5rem;
}
.agent-verify {
  border: 2px solid var(--oc-color-swatch-danger-default, #b00020);
  border-radius: 6px;
  padding: var(--oc-space-medium, 1rem);
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
.agent-history-head {
  margin: 0;
}
</style>
