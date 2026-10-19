<template>
  <article :class="['agent-card', { 'agent-card-danger': danger, 'agent-card-ended': ended }]">
    <p v-if="warning" class="agent-card-warning">
      <oc-icon name="alert" size="small" />
      <strong v-text="warning" />
    </p>
    <header class="agent-card-head">
      <code class="agent-card-tool"><TextBlock :text="approval.tool" /></code>
      <RiskBadge :risk="approval.risk" />
      <span v-if="expiry" class="agent-card-expiry" v-text="expiry" />
      <span v-if="decidedAt" class="agent-card-decided" v-text="decidedAt" />
    </header>
    <p v-if="stateLabel" class="agent-card-state" v-text="stateLabel" />
    <p
      v-if="approval.needs_verification"
      class="agent-card-verify"
      v-text="
        $gettext(
          'The agent could not confirm whether this action ran. Please check your files to see whether it happened, using the exact arguments below.'
        )
      "
    />
    <div class="agent-card-args">
      <span class="agent-label" v-text="$gettext('Exact arguments')" />
      <JsonBlock :value="approval.args" />
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
      <JsonBlock :value="approval.preview" />
    </div>
    <div v-if="approval.rationale" class="agent-card-rationale">
      <span class="agent-label" v-text="$gettext('Agent\'s explanation (unverified)')" />
      <TextBlock :text="approval.rationale" />
    </div>
    <OutcomeBlock v-if="approval.outcome" :outcome="approval.outcome" />
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
          @click="decisions.decide(approval, 'approve')"
          v-text="$gettext('Approve')"
        />
        <oc-button
          appearance="outline"
          :disabled="disabled"
          :aria-label="ariaLabel($gettext('Reject %{tool}'))"
          @click="decisions.decide(approval, 'reject')"
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
import { formatDuration, formatTime } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import { needsConfirm, useDecisions } from './decisions'
import type { Approval } from '../../api/types'

// ApprovalCard shows one approval: exactly what will execute (the stored
// arguments, always in full), the server's preview, and the model's
// explanation clearly marked as unverified. Every API string is text.
const props = defineProps<{ approval: Approval }>()
const { $gettext, interpolate } = useGettext()
const platform = usePlatform()
const decisions = useDecisions()

const pending = computed(() => props.approval.state === 'pending')
const ended = computed(() => props.approval.job_ended)
const danger = computed(() => needsConfirm(props.approval.risk))
const disabled = computed(() => ended.value || decisions.busy(props.approval.id))

const warning = computed(() => {
  switch (props.approval.risk) {
    case 'write':
    case 'read':
      return ''
    case 'destructive':
      return $gettext('This action is irreversible')
    case 'outward':
      return $gettext('This action is visible to other people')
  }
  return $gettext('The risk of this action is unknown')
})

const ariaLabel = (msgid: string) => interpolate(msgid, { tool: visibleText(props.approval.tool).text }, true)

const now = ref(platform.now())
const tick = setInterval(() => (now.value = platform.now()), 30_000)
onBeforeUnmount(() => clearInterval(tick))

const expiry = computed(() => {
  if (!pending.value || ended.value || !props.approval.expires_at) return ''
  const t = Date.parse(props.approval.expires_at)
  if (Number.isNaN(t)) return ''
  const left = t - now.value
  if (left <= 0) return $gettext('expired')
  return interpolate($gettext('expires in %{duration}'), { duration: formatDuration(left) }, true)
})

// stateLabel names the state of a decided approval that has no outcome
// yet (approved, executing).
const stateLabel = computed(() => {
  const a = props.approval
  if (pending.value || a.outcome) return ''
  switch (a.state) {
    case 'approved':
      return $gettext('Approved, waiting to run')
    case 'executing':
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
const lossy = computed(() => hasUnsafeNumber(props.approval.args))

const decidedAt = computed(() => {
  const at = props.approval.executed_at ?? props.approval.decided_at
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
.agent-label {
  display: block;
  font-weight: bold;
  font-size: 0.9em;
}
</style>
