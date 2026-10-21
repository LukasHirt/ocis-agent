<template>
  <article class="agent-job">
    <router-link class="agent-job-link" :to="{ name: `${APPID}-tasks-job`, params: { id: job.id } }">
      <span class="agent-job-goal">
        <TextBlock v-if="job.goal" :text="job.goal" />
        <span v-else v-text="$gettext('(no description)')" />
      </span>
    </router-link>
    <div class="agent-job-meta">
      <span class="agent-job-state">
        <oc-icon :name="state.icon" size="small" />
        <span v-text="state.label" />
        <TextBlock v-if="!state.known" :text="job.state" class="agent-job-raw" />
      </span>
      <span class="agent-job-trigger">
        <span v-if="trigger" v-text="trigger" />
        <TextBlock v-else :text="job.trigger_kind" class="agent-job-raw" />
      </span>
      <span v-if="job.created_at" class="agent-job-created">
        <span v-text="$gettext('Created')" />
        <time :datetime="job.created_at" v-text="formatTime(job.created_at)" />
      </span>
      <span v-if="job.ended_at" class="agent-job-changed">
        <span v-text="$gettext('Ended')" />
        <time :datetime="job.ended_at" v-text="formatTime(job.ended_at)" />
      </span>
      <span v-else-if="job.updated_at" class="agent-job-changed">
        <span v-text="$gettext('Updated')" />
        <time :datetime="job.updated_at" v-text="formatTime(job.updated_at)" />
      </span>
      <span class="agent-job-steps" v-text="steps" />
      <span v-if="tokensIn > 0 || tokensOut > 0" class="agent-job-tokens" v-text="tokens" />
      <router-link
        v-if="conversation"
        class="agent-job-conversation"
        :to="{ name: `${APPID}-chat-conversation`, params: { id: conversation } }"
        v-text="$gettext('Open the conversation')"
      />
    </div>
    <p v-if="job.error" class="agent-job-error">
      <span v-if="reason" v-text="reason" />
      <TextBlock v-else :text="job.error" />
    </p>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import { APPID } from '../../appid'
import { formatTime } from '../../util/format'
import { conversationOf, errorText, stateLabel, tokenCount, triggerLabel } from './labels'
import type { Job } from '../../api/types'

// JobRow is one job in the list: its goal (untrusted text, cut to two
// lines by CSS only; the detail has it all), state, trigger, times, steps,
// tokens and error. It links to the job's detail and a chat job to its
// conversation.
const props = defineProps<{ job: Job }>()
const { $gettext, $ngettext, interpolate } = useGettext()

const state = computed(() => stateLabel(String(props.job.state ?? ''), $gettext))
const trigger = computed(() => triggerLabel(String(props.job.trigger_kind ?? ''), $gettext))
const conversation = computed(() => conversationOf(props.job))
const tokensIn = computed(() => tokenCount(props.job.tokens_in))
const tokensOut = computed(() => tokenCount(props.job.tokens_out))
const tokens = computed(() =>
  interpolate($gettext('%{in} in, %{out} out'), { in: String(tokensIn.value), out: String(tokensOut.value) })
)
const reason = computed(() => errorText(String(props.job.error ?? ''), $gettext))
const steps = computed(() => {
  const n = typeof props.job.steps_count === 'number' ? props.job.steps_count : 0
  return interpolate($ngettext('%{n} step', '%{n} steps', n), { n })
})
</script>

<style>
.agent-job {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: 0.5rem 0.75rem;
  border: 1px solid var(--oc-color-border, #ddd);
  border-radius: 6px;
  min-width: 0;
}
.agent-job-link {
  min-width: 0;
}
.agent-job-goal {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 2;
  line-clamp: 2;
  overflow: hidden;
  overflow-wrap: anywhere;
}
.agent-job-goal .agent-text {
  white-space: normal !important;
}
.agent-job-meta {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem 1rem;
  font-size: 0.85em;
  color: var(--oc-color-text-muted, #555);
}
.agent-job-meta > span {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  min-width: 0;
  overflow-wrap: anywhere;
}
.agent-job-raw {
  font-family: var(--oc-font-family-mono, monospace);
}
.agent-job-error {
  margin: 0;
  font-size: 0.85em;
  overflow-wrap: anywhere;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
