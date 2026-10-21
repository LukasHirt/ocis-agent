<template>
  <article :class="['agent-msg', `agent-msg-${kind}`]">
    <span v-if="kind !== 'user'" class="agent-msg-avatar" aria-hidden="true">
      <oc-icon :name="roleIcon" size="small" />
    </span>
    <div class="agent-msg-body">
      <header class="agent-msg-head">
        <span class="agent-msg-role" v-text="roleLabel" />
        <TextBlock v-if="kind === 'other'" :text="message.role" class="agent-msg-raw-role" />
        <time
          v-if="message.created_at"
          class="agent-msg-time"
          :datetime="message.created_at"
          v-text="formatTime(message.created_at)"
        />
      </header>
      <div class="agent-msg-content">
        <span v-if="systemText" v-text="systemText" />
        <MarkdownBlock v-else-if="kind === 'assistant'" :text="String(message.content ?? '')" />
        <TextBlock v-else :text="message.content" />
      </div>

    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import MarkdownBlock from '../../components/MarkdownBlock.vue'
import { formatTime } from '../../util/format'
import { errorText } from '../../util/reasons'
import type { Message } from '../../api/types'

// MessageBubble shows one message: a question, an answer or a system notice.
// Answers render a safe Markdown subset as text-only elements; everything
// else is plain text. The state of a question's job is shown by ReplyState.
const props = defineProps<{ message: Message }>()
const { $gettext } = useGettext()

const kind = computed(() => {
  const r = props.message.role
  if (r === 'user' || r === 'assistant' || r === 'system') return r
  return 'other'
})

const roleLabel = computed(() => {
  switch (kind.value) {
    case 'user':
      return $gettext('You')
    case 'assistant':
      return $gettext('Agent')
    case 'system':
      return $gettext('Notice')
  }
  return $gettext('Unknown sender')
})

const roleIcon = computed(() => {
  switch (kind.value) {
    case 'user':
      return 'user'
    case 'assistant':
      return 'robot'
  }
  return 'information'
})

// the runner's fixed texts are translated (as on the tasks screen); any
// other text is shown as it is, as untrusted text
const systemText = computed(() =>
  kind.value === 'system' ? errorText(String(props.message.content ?? ''), $gettext) : null
)
</script>

<style>
.agent-msg {
  display: grid;
  grid-template-columns: 2rem minmax(0, 1fr);
  column-gap: var(--agent-space-2);
  align-self: stretch;
  min-width: 0;
  overflow-wrap: anywhere;
}
.agent-msg-user {
  display: block;
  align-self: flex-end;
  max-width: min(40rem, 85%);
}
.agent-msg-system,
.agent-msg-other {
  justify-self: center;
}
.agent-msg-avatar {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  border-radius: 50%;
  background: var(--oc-color-background-highlight);
  color: var(--oc-color-swatch-primary-default);
}
.agent-msg-avatar svg {
  fill: currentColor;
}
.agent-msg-body {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-1);
  min-width: 0;
}
.agent-msg-user .agent-msg-body {
  align-items: flex-end;
}
.agent-msg-head {
  display: flex;
  align-items: center;
  gap: var(--agent-space-1);
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
.agent-msg-user .agent-msg-head {
  justify-content: flex-end;
}
.agent-msg-role {
  font-weight: 600;
}
.agent-msg-content {
  padding: var(--agent-space-2) var(--agent-space-3);
  border-radius: var(--agent-radius-m);
  line-height: 1.5;
  min-width: 0;
  max-width: 100%;
  box-sizing: border-box;
}
.agent-msg-assistant .agent-msg-content {
  background: var(--oc-color-background-muted);
}
.agent-msg-user .agent-msg-content {
  background: var(--oc-color-background-highlight);
}
.agent-msg-system .agent-msg-content,
.agent-msg-other .agent-msg-content {
  padding: 0;
  background: none;
  color: var(--oc-color-text-muted);
  font-style: italic;
}
.agent-msg-raw-role {
  font-family: var(--agent-font-mono);
}
</style>
