<template>
  <article :class="['agent-msg', `agent-msg-${kind}`]">
    <header class="agent-msg-head">
      <oc-icon :name="roleIcon" size="small" />
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
      <TextBlock v-else :text="message.content" />
    </div>

    <div v-if="kind === 'user' && message.job" class="agent-msg-status">
      <oc-icon :name="state.icon" size="small" />
      <span class="agent-msg-state" v-text="state.label" />
      <TextBlock v-if="!state.known" :text="message.job.state" class="agent-msg-raw-state" />
      <router-link
        v-if="waiting"
        :to="{ name: `${APPID}-approvals`, query: { job: message.job.id } }"
        v-text="$gettext('Open the approvals inbox')"
      />
    </div>
    <p v-if="failed && message.job?.error" class="agent-msg-error">
      <span v-if="jobErrorText" v-text="jobErrorText" />
      <TextBlock v-else :text="message.job.error" />
    </p>
    <p v-if="noticeText" class="agent-msg-notice" v-text="noticeText" />

    <div v-if="waiting && approvals" class="agent-msg-approvals">
      <p v-if="approvals.error" role="alert" v-text="approvals.error" />
      <ul v-if="approvals.items.length > 0">
        <li v-for="a in approvals.items" :key="a.id">
          <TextBlock :text="a.tool" class="agent-msg-tool" />
          <RiskBadge :risk="a.risk" />
          <router-link
            :to="{ name: `${APPID}-approvals`, query: { job: a.job_id } }"
            v-text="$gettext('Review')"
          />
        </li>
      </ul>
      <p v-if="approvals.more" v-text="$gettext('More actions are waiting; see them all in the inbox.')" />
    </div>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import RiskBadge from '../../components/RiskBadge.vue'
import { APPID } from '../../appid'
import { formatTime } from '../../util/format'
import { errorText } from '../../util/reasons'
import type { AgentNotice, JobApprovals } from '../../stores/chat'
import type { JobState, Message } from '../../api/types'

// MessageBubble shows one message as plain text (no markup, no links): a
// question with the state of the job that answers it, an answer, or a
// system notice. Every state has a text label and an icon.
const props = defineProps<{
  message: Message
  notice?: AgentNotice
  approvals?: JobApprovals
}>()
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

const STATES: Record<string, { icon: string; label: () => string }> = {
  ['queued' satisfies JobState]: { icon: 'time', label: () => $gettext('Waiting') },
  ['running' satisfies JobState]: { icon: 'loader-4', label: () => $gettext('Working') },
  ['waiting_approval' satisfies JobState]: { icon: 'hourglass', label: () => $gettext('Needs your approval') },
  ['succeeded' satisfies JobState]: { icon: 'checkbox-circle', label: () => $gettext('Answered') },
  ['failed' satisfies JobState]: { icon: 'error-warning', label: () => $gettext('Could not answer') },
  ['cancelled' satisfies JobState]: { icon: 'close-circle', label: () => $gettext('Cancelled') }
}

const state = computed(() => {
  const s = props.message.job?.state ?? ''
  const known = Object.hasOwn(STATES, s)
  return known
    ? { known, icon: STATES[s].icon, label: STATES[s].label() }
    : { known, icon: 'question', label: $gettext('Unknown state:') }
})

// the runner's fixed texts are translated (as on the tasks screen); any
// other text is shown as it is, as untrusted text
const systemText = computed(() =>
  kind.value === 'system' ? errorText(String(props.message.content ?? ''), $gettext) : null
)
const jobErrorText = computed(() => errorText(String(props.message.job?.error ?? ''), $gettext))

const waiting = computed(() => props.message.job?.state === ('waiting_approval' satisfies JobState))
const failed = computed(() => props.message.job?.state === ('failed' satisfies JobState))

// the 202's reason why nothing runs, while the question is still waiting
const noticeText = computed(() => {
  if (!props.notice || kind.value !== 'user') return ''
  const st = props.message.job?.state
  if (st !== undefined && st !== ('queued' satisfies JobState)) return ''
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
.agent-msg {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: 0.5rem 0.75rem;
  border-radius: 6px;
  max-width: min(48rem, 90%);
  min-width: 0;
  overflow-wrap: anywhere;
  border: 1px solid var(--oc-color-border, #ddd);
}
.agent-msg-user {
  align-self: flex-end;
  background: var(--oc-color-background-highlight, #eef3fd);
}
.agent-msg-assistant {
  align-self: flex-start;
  background: var(--oc-color-background-default, #fff);
}
.agent-msg-system,
.agent-msg-other {
  align-self: center;
  border-style: dashed;
  font-style: italic;
  background: var(--oc-color-background-muted, #f5f5f5);
}
.agent-msg-head {
  display: flex;
  align-items: center;
  gap: 0.35rem;
  font-size: 0.85em;
  color: var(--oc-color-text-muted, #555);
}
.agent-msg-role {
  font-weight: bold;
}
.agent-msg-status,
.agent-msg-notice,
.agent-msg-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.35rem;
  font-size: 0.85em;
  margin: 0;
}
.agent-msg-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-msg-approvals ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.agent-msg-approvals li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-msg-tool,
.agent-msg-raw-state,
.agent-msg-raw-role {
  font-family: var(--oc-font-family-mono, monospace);
}
</style>
