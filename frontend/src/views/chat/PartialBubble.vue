<template>
  <article class="agent-msg agent-msg-assistant agent-msg-partial" aria-live="off" aria-busy="true">
    <span class="agent-msg-avatar" aria-hidden="true"><oc-icon name="robot" size="small" /></span>
    <div class="agent-msg-body">
      <header class="agent-msg-head">
        <span class="agent-msg-role" v-text="$gettext('Agent')" />
      </header>
      <div class="agent-msg-content">
        <MarkdownBlock v-if="showText" :text="partial.text" partial caret />
        <p v-else class="agent-msg-busy">
          <span class="agent-msg-state" aria-hidden="true" v-text="$gettext('Thinking')" />
          <span class="agent-dots" aria-hidden="true"><i /><i /><i /></span>
          <span class="agent-sr-only" v-text="$gettext('The agent is thinking.')" />
        </p>
      </div>
      <span v-if="showText" class="agent-sr-only" v-text="$gettext('The agent is writing an answer.')" />
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import MarkdownBlock from '../../components/MarkdownBlock.vue'
import type { PartialAnswer } from '../../stores/chat'

// PartialBubble shows the unfinished answer of a running question like a
// final answer (safe Markdown), or three dots while the model is thinking.
// It is not announced (aria-live off): the thread announces only the final
// answer, not every update.
const props = defineProps<{ partial: PartialAnswer }>()
useGettext()

const showText = computed(() => props.partial.state !== 'thinking' && props.partial.text !== '')
</script>
