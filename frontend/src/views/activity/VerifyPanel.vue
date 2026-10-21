<template>
  <div class="agent-verify">
    <oc-button size="small" appearance="outline" :disabled="audit.verifying" @click="audit.verify()">
      <oc-icon name="shield-check" size="small" />
      <span v-text="$gettext('Check integrity')" />
    </oc-button>
    <oc-spinner v-if="audit.verifying" size="small" :aria-label="$gettext('Checking the log')" />
    <p
      v-if="audit.verifyResult"
      role="status"
      :class="[
        'agent-verify-result',
        'agent-chip',
        audit.verifyResult === 'ok' ? 'agent-chip--success' : 'agent-chip--danger',
        `agent-verify-${audit.verifyResult}`
      ]"
    >
      <oc-icon :name="audit.verifyResult === 'ok' ? 'checkbox-circle' : 'error-warning'" size="small" />
      <span v-text="resultText" />
    </p>
    <p v-if="audit.verifyError" role="alert" class="agent-verify-error agent-chip agent-chip--danger" v-text="audit.verifyError" />
    <p class="agent-verify-note" v-text="$gettext('This check cannot detect removal of the newest events.')" />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAuditStore } from '../../stores/audit'

// VerifyPanel lets the user have the server check the hash chain of the
// log. It shows fixed texts only and always says what the check cannot
// detect.
const audit = useAuditStore()
const { $gettext, $ngettext, interpolate } = useGettext()

const resultText = computed(() => {
  const n = audit.verifyChecked ?? 0
  if (audit.verifyResult === 'ok') {
    return interpolate(
      $ngettext('Verified %{n} event: it was not changed.', 'Verified %{n} events: no recorded event was changed.', n),
      { n },
      true
    )
  }
  const at = audit.verifyBrokenAt
  if (at === null) return $gettext('The chain is broken: events in the log were changed or removed.')
  const where = interpolate(
    $gettext('The chain breaks at event %{seq}: that event was changed, or events before it were removed.'),
    { seq: at },
    true
  )
  if (n === 0) return where
  const before = interpolate(
    $ngettext('The %{n} event before it is intact.', 'The %{n} events before it are intact.', n),
    { n },
    true
  )
  return where + ' ' + before
})
</script>

<style>
.agent-activity .agent-verify {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: flex-end;
  gap: var(--agent-space-2);
  max-width: 100%;
  min-width: 0;
}
.agent-activity .agent-verify p {
  margin: 0;
}
.agent-activity .agent-verify-result {
  padding-top: var(--agent-space-1);
  padding-bottom: var(--agent-space-1);
}
.agent-activity .agent-verify-note {
  flex-basis: 100%;
  text-align: right;
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
</style>
