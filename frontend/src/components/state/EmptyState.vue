<template>
  <div class="agent-state agent-state--empty">
    <span class="agent-state-icon" aria-hidden="true"><oc-icon :name="icon" size="large" /></span>
    <p class="agent-state-title" v-text="title" />
    <p v-if="text" class="agent-state-text" v-text="text" />
    <div v-if="$slots.default" class="agent-state-actions"><slot /></div>
  </div>
</template>

<script setup lang="ts">
// EmptyState: a centred icon, title and optional text for a list or pane
// with nothing in it; the default slot holds actions.
defineProps<{ icon: string; title: string; text?: string }>()
</script>
