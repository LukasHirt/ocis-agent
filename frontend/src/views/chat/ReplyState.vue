<template>
  <article
    :class="['agent-msg', 'agent-msg-assistant', 'agent-msg-reply', `agent-msg-reply--${reply.kind}`]"
    :aria-live="busy ? 'off' : undefined"
    :aria-busy="busy ? 'true' : undefined"
    :role="reply.kind === 'error' && announce ? 'alert' : undefined"
  >
    <span v-if="reply.kind === 'error'" class="agent-msg-avatar agent-msg-avatar--error" aria-hidden="true">
      <oc-icon name="error-warning" size="small" />
    </span>
    <span v-else class="agent-msg-avatar" aria-hidden="true"><oc-icon name="robot" size="small" /></span>
    <div class="agent-msg-body">
      <header class="agent-msg-head">
        <span class="agent-msg-role" v-text="$gettext('Agent')" />
        <time
          v-if="reply.createdAt"
          class="agent-msg-time"
          :datetime="reply.createdAt"
          v-text="formatTime(reply.createdAt)"
        />
      </header>

      <p v-if="reply.kind === 'cancelled'" class="agent-msg-reply-note">
        <oc-icon name="close-circle" size="small" />
        <span class="agent-msg-state" v-text="$gettext('Cancelled')" />
        <template v-if="reply.text">
          <span aria-hidden="true">&ndash;</span>
          <span v-if="reasonText" v-text="reasonText" />
          <TextBlock v-else :text="reply.text" wrap="normal" />
        </template>
      </p>

      <div v-else class="agent-msg-content">
        <p v-if="reply.kind === 'working'" class="agent-msg-busy">
          <span class="agent-msg-state" aria-hidden="true" v-text="$gettext('Working')" />
          <span class="agent-dots" aria-hidden="true"><i /><i /><i /></span>
          <span class="agent-sr-only" v-text="$gettext('The agent is working on it.')" />
        </p>

        <template v-else-if="reply.kind === 'waiting'">
          <p v-if="!noticeText" class="agent-msg-busy">
            <span class="agent-msg-state" aria-hidden="true" v-text="$gettext('Waiting')" />
            <span class="agent-dots" aria-hidden="true"><i /><i /><i /></span>
            <span class="agent-sr-only" v-text="$gettext('The question is waiting to be picked up.')" />
          </p>
          <template v-else>
            <p class="agent-msg-busy">
              <span class="agent-msg-state" v-text="$gettext('Waiting')" />
            </p>
            <p class="agent-msg-notice" v-text="noticeText" />
          </template>
        </template>

        <template v-else-if="reply.kind === 'approval'">
          <p class="agent-msg-reply-title">
            <oc-icon name="hourglass" size="small" />
            <span class="agent-msg-state" v-text="$gettext('Needs your approval')" />
          </p>
          <div class="agent-msg-approvals">
            <InlineApprovals
              :job-id="reply.jobId"
              :items="approvals?.items ?? []"
              :more="approvals?.more ?? false"
              :error="approvals?.error ?? null"
              @settled="emit('settled')"
            />
          </div>
        </template>

        <template v-else-if="reply.kind === 'error'">
          <p class="agent-msg-reply-title"><strong class="agent-msg-state" v-text="$gettext('Could not answer')" /></p>
          <p v-if="reply.text" class="agent-msg-error">
            <span v-if="reasonText" v-text="reasonText" />
            <TextBlock v-else :text="reply.text" wrap="normal" />
          </p>
        </template>

        <p v-else class="agent-msg-busy">
          <oc-icon name="question" size="small" />
          <span class="agent-msg-state" v-text="$gettext('Unknown state:')" />
          <TextBlock :text="reply.rawState" class="agent-msg-raw-state" wrap="normal" />
        </p>
      </div>
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import InlineApprovals from '../approvals/InlineApprovals.vue'
import { formatTime } from '../../util/format'
import { errorText } from '../../util/reasons'
import type { AgentNotice, JobApprovals } from '../../stores/chat'
import type { Reply } from './replyState'

// ReplyState shows the state of a question's job as a reply of the agent:
// calm and neutral while the job is under way, a distinct error reply when it
// failed. Server text is plain text; the runner's fixed reasons are translated.
const props = withDefaults(
  defineProps<{
    reply: Reply
    notice?: AgentNotice
    approvals?: JobApprovals
    announce?: boolean
  }>(),
  { announce: false }
)
const emit = defineEmits<{ settled: [] }>()
const { $gettext } = useGettext()

const busy = computed(() => props.reply.kind === 'working' || props.reply.kind === 'waiting')

// the fixed reasons are translated; any other text is shown as it is
const reasonText = computed(() => errorText(props.reply.text, $gettext))

// the 202's reason why nothing runs, while the question is still waiting
const noticeText = computed(() => {
  if (props.reply.kind !== 'waiting') return ''
  switch (props.notice) {
    case 'paused':
      return $gettext('The agent is paused; it will answer when resumed.')
    case 'killed':
      return $gettext('The agent is switched off by the operator; it will answer when it is switched on again.')
    case 'credentials':
      return $gettext('The agent has lost access to your files; reconnect it in Settings to get an answer.')
  }
  return ''
})
</script>

<style>
.agent-msg-busy {
  display: inline-flex;
  align-items: center;
  gap: var(--agent-space-2);
  margin: 0;
  color: var(--oc-color-text-muted);
}
.agent-msg-reply-title {
  display: flex;
  align-items: center;
  gap: var(--agent-space-1);
  margin: 0;
  font-weight: 600;
}
.agent-msg-reply--approval .agent-msg-reply-title svg {
  fill: var(--oc-color-text-muted);
}
.agent-msg-reply--error .agent-msg-content {
  color: var(--oc-color-text-default);
  background: color-mix(in srgb, var(--oc-color-swatch-danger-default) 8%, var(--oc-color-background-default));
  border: 1px solid color-mix(in srgb, var(--oc-color-swatch-danger-default) 55%, var(--oc-color-background-default));
}
.agent-msg-avatar--error {
  background: color-mix(in srgb, var(--oc-color-swatch-danger-default) 12%, var(--oc-color-background-default));
}
.agent-msg-avatar--error svg {
  fill: var(--oc-color-swatch-danger-default);
}
.agent-msg-reply--error .agent-msg-error {
  margin: var(--agent-space-1) 0 0;
}
.agent-msg-notice {
  margin: var(--agent-space-1) 0 0;
  color: var(--oc-color-text-muted);
}
.agent-msg-reply--cancelled .agent-msg-content {
  padding: 0;
  background: none;
}
.agent-msg-reply-note {
  display: flex;
  align-items: center;
  gap: var(--agent-space-1);
  margin: 0;
  color: var(--oc-color-text-muted);
  font-size: var(--agent-font-s);
}
.agent-msg-reply-note svg {
  fill: currentColor;
}
.agent-msg-reply--unknown .agent-msg-content {
  border: 1px dashed var(--oc-color-border);
}
.agent-msg-approvals {
  margin-top: var(--agent-space-2);
}
.agent-msg-raw-state {
  font-family: var(--agent-font-mono);
}
</style>
