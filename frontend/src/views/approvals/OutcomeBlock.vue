<template>
  <div :class="['agent-outcome', 'agent-notice', `agent-notice--${tone}`, `agent-outcome-${known ? outcome.state : 'other'}`]">
    <p class="agent-outcome-head">
      <oc-icon :name="icon" size="small" />
      <strong class="agent-outcome-state" v-text="label" />
    </p>
    <p v-if="outcome.reason" class="agent-outcome-reason"><TextBlock :text="outcome.reason" /></p>
    <div v-if="outcome.summary" class="agent-outcome-summary">
      <span class="agent-label" v-text="$gettext('Result reported by the tool')" />
      <TextBlock :text="outcome.summary" />
    </div>
    <div v-if="outcome.diff" class="agent-outcome-diff">
      <span class="agent-label" v-text="$gettext('Changes')" />
      <TextBlock :text="outcome.diff" mono :max-lines="20" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import { visibleText } from '../../util/visibleText'
import type { Outcome } from '../../api/types'

// OutcomeBlock shows how a decided approval ended. The summary and the
// diff are tool output (file data): text only.
const props = defineProps<{ outcome: Outcome }>()
const { $gettext, interpolate } = useGettext()

const STATES: Record<string, { icon: string; label: () => string }> = {
  done: { icon: 'checkbox-circle', label: () => $gettext('Done') },
  failed: { icon: 'close-circle', label: () => $gettext('Failed') },
  unknown: { icon: 'question', label: () => $gettext('Outcome unknown') },
  rejected: { icon: 'forbid', label: () => $gettext('Rejected') },
  expired: { icon: 'time', label: () => $gettext('Expired') }
}

const known = computed(() => Object.hasOwn(STATES, props.outcome.state))
const TONES: Record<string, string> = { done: 'success', rejected: 'info', expired: 'info' }
const tone = computed(() => (known.value ? (TONES[props.outcome.state] ?? 'danger') : 'danger'))
const icon = computed(() => (known.value ? STATES[props.outcome.state].icon : 'question'))
const label = computed(() =>
  known.value
    ? STATES[props.outcome.state].label()
    : interpolate($gettext('Unknown outcome (%{state})'), { state: visibleText(String(props.outcome.state)).text }, true)
)
</script>

<style>
.agent-outcome {
  flex-direction: column;
  align-items: stretch;
  gap: var(--agent-space-1);
}
.agent-outcome-head {
  display: flex;
  align-items: center;
  gap: var(--agent-space-1);
  margin: 0;
}
.agent-outcome-head .oc-icon svg {
  fill: var(--agent-tone);
}
.agent-outcome p {
  margin: 0;
}
</style>
