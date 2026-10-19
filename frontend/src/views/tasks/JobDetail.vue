<template>
  <section class="agent-job-detail">
    <router-link class="agent-job-detail-back" :to="{ name: `${APPID}-tasks` }">
      <oc-icon name="arrow-left" size="small" />
      <span v-text="$gettext('All tasks')" />
    </router-link>

    <p v-if="jobs.actionError" class="agent-job-detail-alert" role="alert" v-text="jobs.actionError" />

    <p v-if="jobs.detailMissing" class="agent-job-detail-missing" v-text="$gettext('This task no longer exists.')" />
    <div v-else-if="jobs.detailError" class="agent-job-detail-loaderror">
      <p role="alert" v-text="jobs.detailError" />
      <oc-button size="small" :disabled="jobs.detailLoading" @click="jobs.refreshDetail()" v-text="$gettext('Try again')" />
    </div>
    <oc-spinner v-else-if="!job" :aria-label="$gettext('Loading the task')" />

    <template v-if="job && !jobs.detailMissing">
      <h2 v-text="$gettext('Task')" />
      <div class="agent-job-detail-goal">
        <TextBlock v-if="job.goal" :text="job.goal" />
        <span v-else v-text="$gettext('(no description)')" />
      </div>

      <dl class="agent-job-detail-facts">
        <dt v-text="$gettext('State')" />
        <dd class="agent-job-detail-state">
          <oc-icon :name="state.icon" size="small" />
          <span v-text="state.label" />
          <TextBlock v-if="!state.known" :text="job.state" class="agent-job-raw" />
        </dd>
        <dt v-text="$gettext('Started by')" />
        <dd class="agent-job-detail-trigger">
          <span v-if="trigger" v-text="trigger" />
          <TextBlock v-else :text="job.trigger_kind" class="agent-job-raw" />
        </dd>
        <template v-if="job.created_at">
          <dt v-text="$gettext('Created')" />
          <dd class="agent-job-detail-created">
            <time :datetime="job.created_at" v-text="formatTime(job.created_at)" />
          </dd>
        </template>
        <template v-if="job.updated_at">
          <dt class="agent-job-detail-changed" v-text="ended ? $gettext('Ended') : $gettext('Last change')" />
          <dd><time :datetime="job.updated_at" v-text="formatTime(job.updated_at)" /></dd>
        </template>
        <dt v-text="$gettext('Runs')" />
        <dd class="agent-job-detail-runs" v-text="String(job.runs ?? 0)" />
        <dt v-text="$gettext('Tokens used')" />
        <dd class="agent-job-detail-tokens" v-text="tokensText(jobs.tokens.in, jobs.tokens.out)" />
      </dl>

      <p v-if="job.error" class="agent-job-detail-error">
        <span v-if="reason" v-text="reason" />
        <TextBlock v-else :text="job.error" />
      </p>

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
        <h3 id="agent-job-approvals-h" v-text="$gettext('Waiting for your decision')" />
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

      <section v-if="detail?.answer" class="agent-job-detail-answer-section" aria-labelledby="agent-job-answer-h">
        <h3 id="agent-job-answer-h" v-text="$gettext('Answer')" />
        <div class="agent-job-detail-answer"><TextBlock :text="detail.answer" /></div>
      </section>

      <section class="agent-job-detail-steps" aria-labelledby="agent-job-steps-h">
        <h3 id="agent-job-steps-h" v-text="$gettext('Steps')" />
        <p v-if="steps.length === 0" class="agent-job-detail-nosteps" v-text="$gettext('No steps yet.')" />
        <ol v-else>
          <li v-for="s in steps" :key="s.n" class="agent-step">
            <div class="agent-step-head">
              <span class="agent-step-n" v-text="'#' + s.n" />
              <span class="agent-step-kind">
                <span v-if="stepKindLabel(s.kind, $gettext)" v-text="stepKindLabel(s.kind, $gettext)" />
                <TextBlock v-else :text="s.kind" class="agent-job-raw" />
              </span>
              <TextBlock v-if="s.tool" :text="s.tool" class="agent-step-tool agent-job-raw" />
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
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import RiskBadge from '../../components/RiskBadge.vue'
import { APPID } from '../../appid'
import { formatTime } from '../../util/format'
import { isLiveState, useJobsStore } from '../../stores/jobs'
import { errorText, stateLabel, stepKindLabel, triggerLabel } from './labels'
import type { JobState } from '../../api/types'

// JobDetail shows the open job of the jobs store: goal, facts, error, the
// pending approvals (decided in the inbox only), the answer and the steps.
// Goal, answer, tools, statuses and summaries are untrusted text.
const jobs = useJobsStore()
const { $gettext, interpolate } = useGettext()

const detail = computed(() => jobs.detail)
const job = computed(() => jobs.detail)
const steps = computed(() => jobs.detail?.steps ?? [])
const approvals = computed(() => jobs.detailApprovals)

const state = computed(() => stateLabel(String(job.value?.state ?? ''), $gettext))
const trigger = computed(() => triggerLabel(String(job.value?.trigger_kind ?? ''), $gettext))
const live = computed(() => isLiveState(job.value?.state))
const ended = computed(() => !live.value && state.value.known)
const waiting = computed(() => job.value?.state === ('waiting_approval' satisfies JobState))
const reason = computed(() => errorText(String(job.value?.error ?? ''), $gettext))

const tokensText = (i: number, o: number) =>
  interpolate($gettext('%{in} in, %{out} out'), { in: String(i ?? 0), out: String(o ?? 0) })
</script>

<style>
.agent-job-detail {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-job-detail h2,
.agent-job-detail h3 {
  margin: 0;
}
.agent-job-detail-back {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
}
.agent-job-detail-goal,
.agent-job-detail-answer,
.agent-step-summary {
  overflow-wrap: anywhere;
  min-width: 0;
}
.agent-job-detail-answer {
  padding: 0.5rem 0.75rem;
  border: 1px solid var(--oc-color-border, #ddd);
  border-radius: 6px;
}
.agent-job-detail-facts {
  display: grid;
  grid-template-columns: max-content minmax(0, 1fr);
  gap: 0.25rem 1rem;
  margin: 0;
}
.agent-job-detail-facts dt {
  font-weight: bold;
}
.agent-job-detail-facts dd {
  margin: 0;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem;
  overflow-wrap: anywhere;
}
.agent-job-detail-error,
.agent-job-detail-alert,
.agent-job-detail-loaderror {
  margin: 0;
  overflow-wrap: anywhere;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-job-detail-approvals ul,
.agent-job-detail-steps ol {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.agent-job-detail-approvals li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-step {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: 0.5rem 0.75rem;
  border-left: 3px solid var(--oc-color-border, #ddd);
  min-width: 0;
}
.agent-step-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem 0.75rem;
  font-size: 0.85em;
  color: var(--oc-color-text-muted, #555);
  overflow-wrap: anywhere;
}
.agent-step-kind {
  font-weight: bold;
}
</style>
