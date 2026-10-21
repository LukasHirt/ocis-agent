<template>
  <article class="agent-msg agent-msg-assistant agent-msg-partial" aria-live="off" aria-busy="true">
    <span class="agent-msg-avatar" aria-hidden="true"><oc-icon name="robot" size="small" /></span>
    <div class="agent-msg-body">
      <header class="agent-msg-head">
        <span class="agent-msg-role" v-text="$gettext('Agent')" />
      </header>
      <div v-if="partial.text" class="agent-msg-content">
        <TextBlock :text="partial.text" />
      </div>
      <p class="agent-msg-status agent-msg-partial-state agent-chip agent-chip--info">
        <oc-icon name="loader-4" size="small" class="agent-spin" />
        <span v-text="label" />
      </p>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import type { PartialAnswer } from '../../stores/chat'

// PartialBubble shows the unfinished answer of a running question as plain
// text, with what the model is doing. It is not announced (aria-live off):
// the thread announces only the final answer, not every update.
const props = defineProps<{ partial: PartialAnswer }>()
const { $gettext } = useGettext()

const label = computed(() => (props.partial.state === 'thinking' ? $gettext('Thinking…') : $gettext('Writing…')))
</script>

<style>
.agent-msg-partial .agent-msg-content {
  border: 1px dashed var(--oc-color-border);
}
.agent-msg-partial-state {
  align-self: flex-start;
}
</style>
