<template>
  <div class="agent-verify">
    <oc-button size="small" :disabled="audit.verifying" @click="audit.verify()">
      <oc-icon name="shield-check" size="small" />
      <span v-text="$gettext('Check integrity')" />
    </oc-button>
    <oc-spinner v-if="audit.verifying" size="small" :aria-label="$gettext('Checking the log')" />
    <p
      v-if="audit.verifyResult"
      role="status"
      :class="['agent-verify-result', `agent-verify-${audit.verifyResult}`]"
    >
      <oc-icon :name="audit.verifyResult === 'ok' ? 'checkbox-circle' : 'error-warning'" size="small" />
      <span v-text="resultText" />
    </p>
    <p v-if="audit.verifyError" role="alert" class="agent-verify-error" v-text="audit.verifyError" />
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
const { $gettext } = useGettext()

const resultText = computed(() =>
  audit.verifyResult === 'ok'
    ? $gettext('The log is intact: no recorded event was changed.')
    : $gettext('The chain is broken: events in the log were changed or removed.')
)
</script>

<style>
.agent-verify {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-verify p {
  margin: 0;
}
.agent-verify-result {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
}
.agent-verify-broken,
.agent-verify-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-verify-note {
  flex-basis: 100%;
  font-size: 0.875rem;
}
</style>
