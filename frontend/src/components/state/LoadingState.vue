<template>
  <div :class="['agent-state-loading', `agent-state-loading--${variant}`]">
    <oc-spinner size="small" :aria-label="label" />
    <div v-if="variant !== 'spinner'" class="agent-skeleton" aria-hidden="true">
      <template v-if="variant === 'rows'">
        <div v-for="i in count" :key="i" class="agent-skeleton-row">
          <span class="agent-skeleton-line" style="width: 60%" />
          <span class="agent-skeleton-line" style="width: 35%" />
        </div>
      </template>
      <template v-else-if="variant === 'cards'">
        <div v-for="i in count" :key="i" class="agent-surface agent-skeleton-card">
          <span class="agent-skeleton-line" style="width: 90%" />
          <span class="agent-skeleton-line" style="width: 70%" />
          <span class="agent-skeleton-line" style="width: 40%" />
        </div>
      </template>
      <template v-else>
        <div v-for="i in count" :key="i" :class="['agent-skeleton-bubble', i % 2 ? '' : 'agent-skeleton-bubble--end']">
          <span class="agent-skeleton-line" style="width: 100%" />
          <span class="agent-skeleton-line" style="width: 70%" />
        </div>
      </template>
    </div>
  </div>
</template>

<script setup lang="ts">
// LoadingState: a small labelled spinner and, unless the variant is
// 'spinner', placeholder blocks (hidden from assistive technology).
withDefaults(defineProps<{ label: string; variant?: 'rows' | 'cards' | 'bubbles' | 'spinner'; count?: number }>(), {
  variant: 'rows',
  count: 3
})
</script>

<style>
.agent-state-loading {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-3);
  padding: var(--agent-space-4) 0;
}
.agent-state-loading > .oc-spinner,
.agent-state-loading > [data-stub='OcSpinner'] {
  align-self: center;
}
.agent-skeleton-row {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  padding: var(--agent-space-3) var(--agent-space-4);
  border-bottom: var(--agent-line);
}
.agent-skeleton-bubble {
  width: min(28rem, 70%);
  padding: var(--agent-space-3);
  border-radius: var(--agent-radius-m);
  background: var(--oc-color-background-muted);
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  box-sizing: border-box;
}
.agent-skeleton-bubble--end {
  align-self: flex-end;
}
</style>
