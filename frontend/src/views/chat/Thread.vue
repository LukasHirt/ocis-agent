<template>
  <div ref="scroller" class="agent-thread-scroll" @scroll.passive="onScroll">
    <div v-if="olderCursor || olderError" class="agent-thread-older">
      <p v-if="olderError" role="alert" v-text="olderError" />
      <oc-spinner v-if="olderLoading" size="small" :aria-label="$gettext('Loading older messages')" />
      <oc-button
        v-else-if="olderCursor"
        appearance="outline"
        size="small"
        @click="emit('load-older')"
        v-text="$gettext('Show older messages')"
      />
    </div>
    <div class="agent-thread" role="log" aria-live="polite" :aria-label="$gettext('Messages')">
      <MessageBubble
        v-for="m in messages"
        :key="m.id"
        :message="m"
        :notice="notices[m.id]"
        :approvals="m.job ? jobApprovals[m.job.id] : undefined"
      />
    </div>
    <p
      v-if="loaded && messages.length === 0"
      class="agent-thread-empty"
      v-text="$gettext('No messages yet. Ask the agent something.')"
    />
  </div>
</template>

<script setup lang="ts">
import { nextTick, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import MessageBubble from './MessageBubble.vue'
import type { Message } from '../../api/types'
import type { AgentNotice, JobApprovals } from '../../stores/chat'

// Thread shows the messages oldest first in a scroll area. Older messages
// added above keep the reading position; new ones below are followed only
// when the reader is at the bottom.
const props = defineProps<{
  messages: Message[] // chronological
  notices: Record<string, AgentNotice>
  jobApprovals: Record<string, JobApprovals>
  olderCursor: string | null
  olderLoading: boolean
  olderError: string | null
  loaded: boolean
}>()
const emit = defineEmits<{ 'load-older': [] }>()
useGettext()

// within this many pixels of the end counts as at the bottom
const BOTTOM_SLACK = 24

const scroller = ref<HTMLElement | null>(null)
let atBottom = true

const isAtBottom = (el: HTMLElement) => el.scrollHeight - el.scrollTop - el.clientHeight <= BOTTOM_SLACK

const onScroll = () => {
  if (scroller.value) atBottom = isAtBottom(scroller.value)
}

const toBottom = () => {
  const el = scroller.value
  if (el) el.scrollTop = el.scrollHeight
}

watch(
  () => props.messages,
  (next, prev) => {
    const el = scroller.value
    if (!el) return
    // atBottom is the reader's position from the last scroll
    const before = { height: el.scrollHeight, top: el.scrollTop, bottom: atBottom }
    const firstBefore = prev?.[0]?.id
    const lastBefore = prev?.[prev.length - 1]?.id
    void nextTick(() => {
      const first = next[0]?.id
      const last = next[next.length - 1]?.id
      if (!prev || prev.length === 0) {
        toBottom() // first page: show the newest
      } else if (last === lastBefore && first !== firstBefore) {
        el.scrollTop = before.top + (el.scrollHeight - before.height) // older added above
      } else if (last !== lastBefore && before.bottom) {
        toBottom()
      }
      atBottom = isAtBottom(el)
    })
  },
  { flush: 'pre' }
)

// mounted with a loaded page (an open, a switch): show the newest
onMounted(() => {
  toBottom()
  atBottom = true
})

defineExpose({ toBottom })
</script>

<style>
.agent-thread-scroll {
  flex: 1 1 auto;
  min-height: 12rem;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  padding: 0.5rem;
}
.agent-thread {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
}
.agent-thread-older {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.25rem;
}
.agent-thread-empty {
  color: var(--oc-color-text-muted, #555);
  text-align: center;
}
</style>
