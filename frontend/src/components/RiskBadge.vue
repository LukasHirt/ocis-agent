<template>
  <span :class="['agent-risk', `agent-risk-${known ? risk : 'unknown'}`]">
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
const label = computed(() => (known.value ? RISKS[props.risk].label() : ''))
</script>

<style>
.agent-risk {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  padding: 0 0.4rem;
  border: 1px solid currentColor;
  border-radius: 999px;
  font-size: 0.85em;
}
.agent-risk-read {
  color: var(--oc-color-swatch-passive-default, #555);
}
.agent-risk-write {
  color: var(--oc-color-swatch-warning-default, #8a5a00);
}
.agent-risk-destructive,
.agent-risk-outward {
  color: var(--oc-color-swatch-danger-default, #b00020);
  font-weight: bold;
}
.agent-risk-unknown {
  color: var(--oc-color-swatch-danger-default, #b00020);
  border-style: dashed;
}
.agent-risk-raw {
  font-family: var(--oc-font-family-mono, monospace);
}
</style>
