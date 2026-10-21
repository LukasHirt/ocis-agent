<template>
  <article class="agent-job agent-row">
    <router-link class="agent-job-link agent-row-link" :to="{ name: `${APPID}-tasks-job`, params: { id: job.id } }">
      <span class="agent-job-goal">
        <TextBlock v-if="job.goal" :text="job.goal" />
        <span v-else v-text="$gettext('(no description)')" />
      </span>
    </router-link>
    <div class="agent-job-meta">
      <span class="agent-job-state agent-chip" :class="`agent-chip--${tone}`">
        <oc-icon :name="state.icon" size="small" />
        <span v-text="state.label" />
        <TextBlock v-if="!state.known" :text="job.state" class="agent-job-raw" />
      </span>
      <span class="agent-job-trigger agent-chip agent-chip--neutral">
        <span v-if="trigger" v-text="trigger" />
        <TextBlock v-else :text="job.trigger_kind" class="agent-job-raw" />
      </span>
      <router-link
        v-if="conversation"
        class="agent-job-conversation agent-row-aside"
        :to="{ name: `${APPID}-chat-conversation`, params: { id: conversation } }"
        v-text="$gettext('Open the conversation')"
      />
    </div>
    <div class="agent-job-side">
      <span v-if="tokensIn > 0 || tokensOut > 0" class="agent-job-tokens" v-text="tokens" />
      <span class="agent-job-steps" v-text="steps" />
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
    </div>
    <p v-if="job.error" class="agent-job-error agent-notice agent-notice--danger">
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
import { conversationOf, errorText, stateLabel, stateTone, tokenCount, triggerLabel } from './labels'
import type { Job } from '../../api/types'

// JobRow is one job in the list: its goal (untrusted text, cut to two
// lines by CSS only; the detail has it all), state, trigger, times, steps,
// tokens and error. It links to the job's detail and a chat job to its
// conversation.
const props = defineProps<{ job: Job }>()
const { $gettext, $ngettext, interpolate } = useGettext()

const state = computed(() => stateLabel(String(props.job.state ?? ''), $gettext))
const tone = computed(() => stateTone(String(props.job.state ?? '')))
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
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  grid-template-areas:
    'goal side'
    'meta side'
    'error error';
  align-items: start;
  gap: var(--agent-space-1) var(--agent-space-4);
}
.agent-job-link {
  grid-area: goal;
  min-width: 0;
  font-weight: 600;
  color: var(--oc-color-text-default);
  text-decoration: none;
}
.agent-job-goal {
  display: -webkit-box;
  -webkit-box-orient: vertical;
  -webkit-line-clamp: 1;
  line-clamp: 1;
  overflow: hidden;
  overflow-wrap: anywhere;
}
.agent-job-goal .agent-text {
  white-space: normal !important;
}
.agent-job-meta {
  grid-area: meta;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-1) var(--agent-space-2);
  min-width: 0;
}
.agent-job-side {
  grid-area: side;
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  text-align: right;
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
.agent-job-side > span {
  display: inline-flex;
  gap: var(--agent-space-1);
  overflow-wrap: anywhere;
}
.agent-job-raw {
  font-family: var(--agent-font-mono);
}
.agent-job-error {
  grid-area: error;
  font-size: var(--agent-font-xs);
}
@container agent (max-width: 699px) {
  .agent-job {
    grid-template-columns: minmax(0, 1fr);
    grid-template-areas:
      'goal'
      'meta'
      'side'
      'error';
  }
  .agent-job-goal {
    -webkit-line-clamp: 2;
    line-clamp: 2;
  }
  .agent-job-side {
    flex-direction: row;
    flex-wrap: wrap;
    align-items: center;
    gap: 0 var(--agent-space-3);
    text-align: left;
  }
}
</style>
