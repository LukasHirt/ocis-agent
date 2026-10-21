<template>
  <span :class="['agent-risk', 'agent-chip', `agent-risk-${known ? risk : 'unknown'}`, toneClass]">
    <oc-icon :name="icon" size="small" />
    <span v-if="known" class="agent-risk-label" v-text="label" />
    <span v-else class="agent-risk-label">
      <span v-text="$gettext('Unknown risk:')" />
      <TextBlock :text="risk" class="agent-risk-raw" />
    </span>
  </span>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from './TextBlock.vue'

// RiskBadge shows a tool risk with a text label and an icon, so colour is
// never the only signal. A value outside the known set is shown raw.
const props = defineProps<{ risk: string }>()
const { $gettext } = useGettext()

const RISKS: Record<string, { icon: string; label: () => string }> = {
  read: { icon: 'eye', label: () => $gettext('Read') },
  write: { icon: 'pencil', label: () => $gettext('Write') },
  destructive: { icon: 'delete-bin', label: () => $gettext('Destructive') },
  outward: { icon: 'share-forward', label: () => $gettext('Outward') }
}

const known = computed(() => Object.hasOwn(RISKS, props.risk))
const icon = computed(() => (known.value ? RISKS[props.risk].icon : 'question'))
const toneClass = computed<string | string[]>(() => {
  if (!known.value) return ['agent-chip--danger', 'agent-chip--dashed']
  if (props.risk === 'read') return 'agent-chip--neutral'
  if (props.risk === 'write') return 'agent-chip--warning'
  return 'agent-chip--danger'
})
const label = computed(() => (known.value ? RISKS[props.risk].label() : ''))
</script>

<style>
.agent-risk-raw {
  font-family: var(--oc-font-family-mono, monospace);
}
</style>
