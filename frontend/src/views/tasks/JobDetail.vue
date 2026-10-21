<template>
  <section class="agent-job-detail">
    <router-link class="agent-job-detail-back" :to="{ name: `${APPID}-tasks` }">
      <oc-icon name="arrow-left" size="small" />
      <span v-text="$gettext('All tasks')" />
    </router-link>

    <p v-if="jobs.actionError" class="agent-job-detail-alert agent-notice agent-notice--danger" role="alert" v-text="jobs.actionError" />

    <EmptyState
      v-if="jobs.detailMissing"
      class="agent-job-detail-missing"
      icon="task"
      :title="$gettext('This task no longer exists.')"
    />
    <ErrorState
      v-else-if="jobs.detailError && (!job || !agent.serviceDown)"
      class="agent-job-detail-loaderror"
      :title="jobs.detailError"
      :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
      :compact="!!job"
      :busy="jobs.detailLoading"
      @retry="retry"
    />
    <LoadingState v-else-if="!job" variant="cards" :count="2" :label="$gettext('Loading the task')" />

    <template v-if="job && !jobs.detailMissing">
      <h2 class="agent-section-title agent-job-detail-title" v-text="$gettext('Task')" />
      <div class="agent-job-detail-main">
        <div class="agent-job-detail-goal agent-surface">
          <TextBlock v-if="job.goal" :text="job.goal" />
          <span v-else v-text="$gettext('(no description)')" />
        </div>

        <p v-if="job.error" class="agent-job-detail-error agent-notice agent-notice--danger">
          <span v-if="reason" v-text="reason" />
          <TextBlock v-else :text="job.error" />
        </p>

        <section v-if="partial" class="agent-job-detail-partial-section agent-surface" aria-labelledby="agent-job-partial-h">
          <h3 id="agent-job-partial-h" class="agent-card-title" v-text="$gettext('Partial answer (unfinished)')" />
          <div v-if="partial.text" class="agent-job-detail-partial" aria-live="off">
            <TextBlock :text="partial.text" />
          </div>
          <p class="agent-job-detail-partial-state agent-chip agent-chip--info">
            <oc-icon name="loader-4" size="small" />
            <span v-text="partial.state === 'thinking' ? $gettext('Thinking…') : $gettext('Writing…')" />
          </p>
        </section>

        <section v-if="detail?.answer" class="agent-job-detail-answer-section agent-surface" aria-labelledby="agent-job-answer-h">
          <h3 id="agent-job-answer-h" class="agent-card-title" v-text="$gettext('Answer')" />
          <div class="agent-job-detail-answer"><TextBlock :text="detail.answer" /></div>
        </section>

        <section class="agent-job-detail-steps" aria-labelledby="agent-job-steps-h">
          <h3 id="agent-job-steps-h" class="agent-card-title" v-text="$gettext('Steps')" />
          <p v-if="steps.length === 0" class="agent-job-detail-nosteps" v-text="$gettext('No steps yet.')" />
          <ol v-else>
            <li v-for="s in steps" :key="s.n" class="agent-step">
              <div class="agent-step-head">
                <span class="agent-step-n agent-mono" v-text="'#' + s.n" />
                <span class="agent-step-kind">
                  <span v-if="stepKindLabel(s.kind, $gettext)" v-text="stepKindLabel(s.kind, $gettext)" />
                  <TextBlock v-else :text="s.kind" class="agent-job-raw" />
                </span>
                <TextBlock v-if="s.tool" :text="s.tool" class="agent-step-tool agent-job-raw agent-chip agent-chip--neutral" />
                <TextBlock v-if="s.status" :text="s.status" class="agent-step-status agent-job-raw" />
                <span
                  v-if="s.tokens_in > 0 || s.tokens_out > 0"
                  class="agent-step-tokens"
                  v-text="tokensText(s.tokens_in, s.tokens_out)"
                />
                <time v-if="s.created_at" :datetime="s.created_at" v-text="formatTime(s.created_at)" />
                <router-link
                  v-if="s.approval_id"
                  class="agent-step-approval"
                  :to="{ name: `${APPID}-approvals`, query: { job: job.id } }"
                  v-text="$gettext('Approval')"
                />
              </div>
              <div v-if="s.summary" class="agent-step-summary"><TextBlock :text="s.summary" /></div>
            </li>
          </ol>
        </section>
      </div>

      <aside class="agent-job-detail-side agent-surface">
        <dl class="agent-job-detail-facts">
          <dt v-text="$gettext('State')" />
          <dd class="agent-job-detail-state agent-chip" :class="`agent-chip--${tone}`">
            <oc-icon :name="state.icon" size="small" />
            <span v-text="state.label" />
            <TextBlock v-if="!state.known" :text="job.state" class="agent-job-raw" />
          </dd>
          <dt v-text="$gettext('Started by')" />
          <dd class="agent-job-detail-trigger agent-chip agent-chip--neutral">
            <span v-if="trigger" v-text="trigger" />
            <TextBlock v-else :text="job.trigger_kind" class="agent-job-raw" />
          </dd>
          <template v-if="job.created_at">
            <dt v-text="$gettext('Created')" />
            <dd class="agent-job-detail-created">
              <time :datetime="job.created_at" v-text="formatTime(job.created_at)" />
            </dd>
          </template>
          <template v-if="job.ended_at">
            <dt class="agent-job-detail-changed" v-text="$gettext('Ended')" />
            <dd class="agent-job-detail-ended"><time :datetime="job.ended_at" v-text="formatTime(job.ended_at)" /></dd>
          </template>
          <template v-else-if="job.updated_at">
            <dt class="agent-job-detail-changed" v-text="$gettext('Last change')" />
            <dd><time :datetime="job.updated_at" v-text="formatTime(job.updated_at)" /></dd>
          </template>
          <dt v-text="$gettext('Runs')" />
          <dd class="agent-job-detail-runs" v-text="String(job.runs ?? 0)" />
          <dt v-text="$gettext('Tokens used')" />
          <dd class="agent-job-detail-tokens" v-text="tokensText(tokenCount(job.tokens_in), tokenCount(job.tokens_out))" />
        </dl>
        <router-link
          v-if="conversation"
          class="agent-job-detail-conversation"
          :to="{ name: `${APPID}-chat-conversation`, params: { id: conversation } }"
          v-text="$gettext('Open the conversation')"
        />

        <div v-if="live" class="agent-job-detail-actions">
          <oc-button
            variation="danger"
            appearance="outline"
            size="small"
            :disabled="jobs.cancelling"
            @click="jobs.cancel(job.id)"
            v-text="$gettext('Cancel task')"
          />
        </div>

        <section v-if="waiting || approvals" class="agent-job-detail-approvals" aria-labelledby="agent-job-approvals-h">
          <h3 id="agent-job-approvals-h" class="agent-card-title" v-text="$gettext('Waiting for your decision')" />
          <p v-if="approvals?.error" role="alert" v-text="approvals.error" />
          <ul v-if="approvals && approvals.items.length > 0">
            <li v-for="a in approvals.items" :key="a.id">
              <TextBlock :text="a.tool" class="agent-job-detail-tool agent-job-raw" />
              <RiskBadge :risk="a.risk" />
            </li>
          </ul>
          <p v-if="approvals?.more" v-text="$gettext('More actions are waiting; see them all in the inbox.')" />
          <router-link
            class="agent-job-detail-inbox"
            :to="{ name: `${APPID}-approvals`, query: { job: job.id } }"
            v-text="$gettext('Review in the approvals inbox')"
          />
        </section>
      </aside>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import RiskBadge from '../../components/RiskBadge.vue'
import EmptyState from '../../components/state/EmptyState.vue'
import ErrorState from '../../components/state/ErrorState.vue'
import LoadingState from '../../components/state/LoadingState.vue'
import { useAgentStore } from '../../stores/agent'
import { APPID } from '../../appid'
import { formatTime } from '../../util/format'
import { isLiveState, useJobsStore } from '../../stores/jobs'
import { conversationOf, errorText, stateLabel, stepKindLabel, stateTone, tokenCount, triggerLabel } from './labels'
import type { JobState, PartialState } from '../../api/types'

// JobDetail shows the open job of the jobs store: goal, facts, error, the
// pending approvals (decided in the inbox only), the answer and the steps;
// a chat job links to its conversation.
// Goal, answer, tools, statuses and summaries are untrusted text.
const jobs = useJobsStore()
const agent = useAgentStore()
const { $gettext, interpolate } = useGettext()

const retry = () => {
  void jobs.refreshDetail()
  if (agent.serviceDown) void agent.retryNow()
}

const detail = computed(() => jobs.detail)
const job = computed(() => jobs.detail)
const steps = computed(() => jobs.detail?.steps ?? [])
const approvals = computed(() => jobs.detailApprovals)

const state = computed(() => stateLabel(String(job.value?.state ?? ''), $gettext))
const tone = computed(() => stateTone(String(job.value?.state ?? '')))
const trigger = computed(() => triggerLabel(String(job.value?.trigger_kind ?? ''), $gettext))
const live = computed(() => isLiveState(job.value?.state))
const conversation = computed(() => (job.value ? conversationOf(job.value) : null))
const waiting = computed(() => job.value?.state === ('waiting_approval' satisfies JobState))
// the unfinished answer, only while the job runs (a stale one of an ended
// or paused job is never shown)
const partial = computed(() => {
  const j = job.value
  if (!j || j.state !== ('running' satisfies JobState)) return null
  const st = String(j.partial_state ?? '')
  if (st !== ('thinking' satisfies PartialState) && st !== ('writing' satisfies PartialState)) return null
  return { text: typeof j.partial === 'string' ? j.partial : '', state: st }
})
const reason = computed(() => errorText(String(job.value?.error ?? ''), $gettext))

const tokensText = (i: number, o: number) =>
  interpolate($gettext('%{in} in, %{out} out'), { in: String(i ?? 0), out: String(o ?? 0) })
</script>

<style>
.agent-job-detail {
  display: grid;
  grid-template-columns: minmax(0, 1fr);
  gap: var(--agent-space-4);
  min-width: 0;
}
.agent-job-detail > * {
  min-width: 0;
}
.agent-job-detail-title {
  margin: 0;
}
.agent-job-detail-back {
  display: inline-flex;
  align-items: center;
  gap: var(--agent-space-1);
}
.agent-job-detail-main {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
  min-width: 0;
}
.agent-job-detail-goal,
.agent-job-detail-answer,
.agent-job-detail-partial,
.agent-step-summary {
  overflow-wrap: anywhere;
  min-width: 0;
}
.agent-job-detail-partial-section {
  border-style: dashed;
}
.agent-job-detail-partial-state {
  margin: 0;
  align-self: flex-start;
}
.agent-job-detail-facts {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  align-items: center;
  gap: var(--agent-space-2) var(--agent-space-4);
  margin: 0;
}
.agent-job-detail-facts dt {
  font-size: var(--agent-font-xs);
  font-weight: 600;
  color: var(--oc-color-text-muted);
}
.agent-job-detail-facts dd {
  margin: 0;
  overflow-wrap: anywhere;
}
.agent-job-detail-facts dd.agent-chip {
  justify-self: start;
}
.agent-job-detail-error,
.agent-job-detail-alert {
  margin: 0;
}
.agent-job-detail-approvals,
.agent-job-detail-steps {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-3);
}
.agent-job-detail-approvals ul,
.agent-job-detail-steps ol {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
}
.agent-job-detail-approvals ul {
  gap: var(--agent-space-2);
}
.agent-job-detail-approvals li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
}
.agent-step {
  position: relative;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  margin-left: var(--agent-space-2);
  padding: 0 0 var(--agent-space-4) var(--agent-space-5);
  border-left: 2px solid var(--oc-color-border);
  min-width: 0;
}
.agent-step:last-child {
  border-left-color: transparent;
  padding-bottom: 0;
}
.agent-step::before {
  content: '';
  position: absolute;
  left: -6px;
  top: 0.35rem;
  width: 10px;
  height: 10px;
  border-radius: 50%;
  box-sizing: border-box;
  background: var(--oc-color-background-default);
  border: 2px solid var(--oc-color-swatch-primary-default);
}
.agent-step-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-1) var(--agent-space-3);
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
  overflow-wrap: anywhere;
}
.agent-step-kind {
  font-weight: 600;
  font-size: var(--agent-font-s);
  color: var(--oc-color-text-default);
}
.agent-step-tokens {
  margin-left: auto;
}
.agent-step-summary {
  padding: var(--agent-space-2) var(--agent-space-3);
  background: var(--oc-color-background-muted);
  border-radius: var(--agent-radius-s);
}
@container agent (min-width: 1000px) {
  .agent-job-detail {
    grid-template-columns: minmax(0, 1fr) 18rem;
    grid-template-areas:
      'back back'
      'alert alert'
      'title title'
      'main side';
  }
  .agent-job-detail-back { grid-area: back; justify-self: start; }
  .agent-job-detail-alert { grid-area: alert; }
  .agent-job-detail-title { grid-area: title; }
  .agent-job-detail-main { grid-area: main; }
  .agent-job-detail-side {
    grid-area: side;
    position: sticky;
    top: var(--agent-space-4);
    align-self: start;
  }
}
</style>
