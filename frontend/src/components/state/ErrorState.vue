<template>
  <div :class="['agent-state', 'agent-state--error', { 'agent-state--compact': compact }]" role="alert">
    <span class="agent-state-icon" aria-hidden="true"><oc-icon :name="icon" size="large" /></span>
    <p class="agent-state-title" v-text="title" />
    <p v-if="text" class="agent-state-text" v-text="text" />
    <div class="agent-state-actions">
      <oc-button appearance="outline" size="small" :disabled="busy" @click="emit('retry')" v-text="$gettext('Try again')" />
      <oc-spinner v-if="busy" size="small" :aria-label="$gettext('Trying again')" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { useGettext } from 'vue3-gettext'

// ErrorState: the one error of a screen area, with a retry. The title is
// already a fixed, described message (never raw server text).
withDefaults(defineProps<{ title: string; text?: string; icon?: string; busy?: boolean; compact?: boolean }>(), {
  icon: 'error-warning'
})
const emit = defineEmits<{ retry: [] }>()
useGettext()
</script>
