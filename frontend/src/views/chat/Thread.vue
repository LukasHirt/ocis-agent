<template>
  <div ref="scroller" class="agent-thread-scroll" @scroll.passive="onScroll">
    <div v-if="olderCursor || olderError" class="agent-thread-older">
      <p v-if="olderError" class="agent-notice agent-notice--danger" role="alert" v-text="olderError" />
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
      <template v-for="m in messages" :key="m.id">
        <ReplyState
          v-if="m.role === 'system' && m.job_id"
          :reply="replyForSystem(m, stateByJob)"
          :announce="announced(m)"
        />
        <MessageBubble v-else :message="m" />
        <PartialBubble v-if="partials[m.id]" :partial="partials[m.id]" />
        <ReplyState
          v-else-if="questionReply(m)"
          :reply="questionReply(m)!"
          :notice="notices[m.id]"
          :approvals="m.job ? jobApprovals[m.job.id] : undefined"
          :announce="announced(m)"
          @settled="emit('approvals-settled')"
        />
      </template>
    </div>
    <EmptyState
      v-if="loaded && messages.length === 0"
      class="agent-thread-empty"
      icon="chat-3"
      :title="$gettext('No messages yet. Ask the agent something.')"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import MessageBubble from './MessageBubble.vue'
import EmptyState from '../../components/state/EmptyState.vue'
import PartialBubble from './PartialBubble.vue'
import ReplyState from './ReplyState.vue'
import { replyForQuestion, replyForSystem } from './replyState'
import type { Message } from '../../api/types'
import type { AgentNotice, JobApprovals, PartialAnswer } from '../../stores/chat'

// Thread shows the messages oldest first in a scroll area. Older messages
// added above keep the reading position; new ones below are followed only
// when the reader is at the bottom, and so is a growing partial answer
// (shown under its question while its job runs).
const props = defineProps<{
  messages: Message[] // chronological
  notices: Record<string, AgentNotice>
  partials: Record<string, PartialAnswer> // by question id
  jobApprovals: Record<string, JobApprovals>
  olderCursor: string | null
  olderLoading: boolean
  olderError: string | null
  loaded: boolean
}>()
const emit = defineEmits<{ 'load-older': []; 'approvals-settled': [] }>()
useGettext()

// the jobs that have a reply (any message that is not a question) and the job
// states by id, once per change of the messages
const replied = computed(
  () => new Set(props.messages.filter((m) => m.role !== 'user' && m.job_id).map((m) => m.job_id))
)
const stateByJob = computed(() => {
  const out = new Map<string, string>()
  for (const m of props.messages) if (m.role === 'user' && m.job) out.set(m.job.id, m.job.state)
  return out
})
const questionReply = (m: Message) =>
  m.role === 'user' ? replyForQuestion(m, replied.value, Boolean(props.partials[m.id])) : null

// Errors that were already there when the conversation was opened are not
// announced; only those that appear afterwards are.
const initial = new Set<string>()
let initialDone = false
const markInitial = (ms: Message[]) => {
  if (initialDone || ms.length === 0) return
  initialDone = true
  for (const m of ms) initial.add(m.id)
}
markInitial(props.messages)

// A question that was shown running and then fails is announced (its reply
// carries role=alert) even though it was listed at the start; the runner's
// message that replaces that reply is then not announced a second time.
const liveSeen = new Set<string>()
const alertedJobs = new Set<string>()
const LIVE = ['queued', 'running', 'waiting_approval']
watch(
  () => props.messages,
  (ms) => {
    for (const m of ms) {
      const j = m.role === 'user' ? m.job : undefined
      if (!j) continue
      if (LIVE.includes(j.state)) liveSeen.add(j.id)
      else if (j.state === 'failed' && liveSeen.has(j.id) && !replied.value.has(j.id)) alertedJobs.add(j.id)
    }
  },
  { immediate: true }
)
const announced = (m: Message) =>
  m.role === 'user'
    ? !initial.has(m.id) || Boolean(m.job && alertedJobs.has(m.job.id))
    : !initial.has(m.id) && !alertedJobs.has(m.job_id ?? '')

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
  () => [props.messages, props.partials] as const,
  ([next], old) => {
    const prev = old?.[0]
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
      } else if (before.bottom) {
        toBottom() // a new message or a longer partial answer

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
  min-height: 8rem;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  padding: var(--agent-space-4) var(--agent-space-5);
}
.agent-thread,
.agent-thread-older {
  width: 100%;
  max-width: 52rem;
  margin: 0 auto;
  box-sizing: border-box;
}
.agent-thread {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
}
.agent-thread-older {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: var(--agent-space-1);
}
</style>
