<template>
  <article :class="['agent-card', { 'agent-card-danger': danger, 'agent-card-ended': ended }]">
    <p v-if="warning" class="agent-card-warning">
      <oc-icon name="alert" size="small" />
      <strong v-text="warning" />
    </p>
    <header class="agent-card-head">
      <code class="agent-card-tool"><TextBlock :text="shown.tool" /></code>
      <RiskBadge :risk="shown.risk" />
      <span v-if="expiry" class="agent-card-expiry" v-text="expiry" />
      <span v-if="decidedAt" class="agent-card-decided" v-text="decidedAt" />
    </header>
    <p v-if="stateLabel" class="agent-card-state" v-text="stateLabel" />
    <div v-if="shown.needs_verification" class="agent-card-verify">
      <p
        v-text="
          $gettext(
            'The agent could not confirm whether this action ran. Please check your files to see whether it happened, using the exact arguments below.'
          )
        "
      />
      <oc-button
        appearance="outline"
        size="small"
        :disabled="decisions.busy(shown.id)"
        :aria-label="ariaLabel($gettext('I checked the outcome of %{tool}'))"
        @click="decisions.acknowledge(shown)"
        v-text="$gettext('I checked')"
      />
    </div>
    <div class="agent-card-args">
      <span class="agent-label" v-text="$gettext('Exact arguments')" />
      <JsonBlock :value="shown.args" />
      <p
        v-if="lossy"
        class="agent-card-lossy"
        v-text="
          $gettext(
            'Some numbers in these arguments are too large to be shown exactly here; the agent would use the exact values.'
          )
        "
      />
    </div>
    <div class="agent-card-preview">
      <span class="agent-label" v-text="pending ? $gettext('What will happen') : $gettext('What was requested')" />
      <JsonBlock :value="shown.preview" />
    </div>
    <div v-if="shown.rationale" class="agent-card-rationale">
      <span class="agent-label" v-text="$gettext('Agent\'s explanation (unverified)')" />
      <TextBlock :text="shown.rationale" />
    </div>
    <OutcomeBlock v-if="shown.outcome" :outcome="shown.outcome" />
    <template v-if="pending">
      <p
        v-if="ended"
        class="agent-card-ended-note"
        v-text="$gettext('The job ended; this can no longer be approved')"
      />
      <div class="agent-card-actions">
        <oc-button
          :appearance="danger ? 'filled' : 'outline'"
          :variation="danger ? 'danger' : 'primary'"
          :disabled="disabled"
          :aria-label="ariaLabel($gettext('Approve %{tool}'))"
          @click="decisions.decide(shown, 'approve')"
          v-text="$gettext('Approve')"
        />
        <oc-button
          appearance="outline"
          :disabled="disabled"
          :aria-label="ariaLabel($gettext('Reject %{tool}'))"
          @click="decisions.decide(shown, 'reject')"
          v-text="$gettext('Reject')"
        />
      </div>
    </template>
  </article>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import JsonBlock from '../../components/JsonBlock.vue'
import RiskBadge from '../../components/RiskBadge.vue'
import OutcomeBlock from './OutcomeBlock.vue'
import { usePlatform } from '../../platform'
import { useAgentStore } from '../../stores/agent'
import { formatDuration, formatTime } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import { needsConfirm, useDecisions } from './decisions'
import type { Approval, ApprovalState, Risk } from '../../api/types'

// ApprovalCard shows one approval: exactly what will execute (the stored
// arguments, always in full), the server's preview, and the model's
// explanation clearly marked as unverified. Every API string is text.
const props = defineProps<{ approval: Approval }>()
const store = useAgentStore()
// shown is the approval as an acknowledgement returned it, if there was one
const shown = computed(() => store.viewOf(props.approval))
const { $gettext, interpolate } = useGettext()
const platform = usePlatform()
const decisions = useDecisions()

const pending = computed(() => shown.value.state === ('pending' satisfies ApprovalState))
const ended = computed(() => shown.value.job_ended)
const danger = computed(() => needsConfirm(shown.value.risk))
const disabled = computed(() => ended.value || decisions.busy(shown.value.id))

const warning = computed(() => {
  switch (shown.value.risk) {
    case 'write' satisfies Risk:
    case 'read' satisfies Risk:
      return ''
    case 'destructive' satisfies Risk:
      return $gettext('This action is irreversible')
    case 'outward' satisfies Risk:
      return $gettext('This action is visible to other people')
  }
  return $gettext('The risk of this action is unknown')
})

const ariaLabel = (msgid: string) => interpolate(msgid, { tool: visibleText(shown.value.tool).text }, true)

const now = ref(platform.now())
const tick = setInterval(() => (now.value = platform.now()), 30_000)
onBeforeUnmount(() => clearInterval(tick))

const expiry = computed(() => {
  if (!pending.value || ended.value || !shown.value.expires_at) return ''
  const t = Date.parse(shown.value.expires_at)
  if (Number.isNaN(t)) return ''
  const left = t - now.value
  if (left <= 0) return $gettext('expired')
  return interpolate($gettext('expires in %{duration}'), { duration: formatDuration(left) }, true)
})

// stateLabel names the state of a decided approval that has no outcome
// yet (approved, executing).
const stateLabel = computed(() => {
  const a = shown.value
  if (pending.value || a.outcome) return ''
  switch (a.state) {
    case 'approved' satisfies ApprovalState:
      return $gettext('Approved, waiting to run')
    case 'executing' satisfies ApprovalState:
      return $gettext('Running')
  }
  return interpolate($gettext('State: %{state}'), { state: visibleText(String(a.state)).text }, true)
})

// lossy: the arguments hold an integer that JSON parsing may have rounded,
// so the display can differ from the stored value.
const hasUnsafeNumber = (v: unknown, depth = 0): boolean => {
  if (depth > 64) return false
  if (typeof v === 'number') return Number.isInteger(v) && !Number.isSafeInteger(v)
  if (Array.isArray(v)) return v.some((x) => hasUnsafeNumber(x, depth + 1))
  if (v !== null && typeof v === 'object') return Object.values(v).some((x) => hasUnsafeNumber(x, depth + 1))
  return false
}
const lossy = computed(() => hasUnsafeNumber(shown.value.args))

const decidedAt = computed(() => {
  const at = shown.value.executed_at ?? shown.value.decided_at
  if (pending.value || !at) return ''
  return interpolate($gettext('decided %{time}'), { time: formatTime(at) }, true)
})
</script>

<style>
.agent-card {
  border: 1px solid var(--oc-color-border, #ccc);
  border-radius: 6px;
  padding: var(--oc-space-medium, 1rem);
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
}
.agent-card-danger {
  border-color: var(--oc-color-swatch-danger-default, #b00020);
  border-width: 2px;
}
.agent-card-ended {
  opacity: 0.7;
}
.agent-card-warning {
  display: flex;
  align-items: center;
  gap: 0.25rem;
  margin: 0;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-card-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-card-tool {
  font-family: var(--oc-font-family-mono, monospace);
  font-weight: bold;
}
.agent-card-expiry,
.agent-card-decided {
  color: var(--oc-color-text-muted, #666);
  font-size: 0.9em;
}
.agent-card-actions {
  display: flex;
  gap: 0.5rem;
}
.agent-card-verify {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-card-verify p {
  margin: 0;
}
</style>
