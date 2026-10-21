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
        <TextBlock v-else :text="message.content" />
      </div>

      <div
        v-if="kind === 'user' && message.job"
        :class="['agent-msg-status', 'agent-chip', `agent-chip--${state.tone}`, { 'agent-chip--dashed': !state.known }]"
      >
        <oc-icon :name="state.icon" size="small" :class="{ 'agent-spin': state.spin }" />
        <span class="agent-msg-state" v-text="state.label" />
        <TextBlock v-if="!state.known" :text="message.job.state" class="agent-msg-raw-state" />
        <router-link
          v-if="waiting"
          :to="{ name: `${APPID}-approvals`, query: { job: message.job.id } }"
          v-text="$gettext('Open the approvals inbox')"
        />
      </div>
      <p v-if="failed && message.job?.error" class="agent-msg-error agent-notice agent-notice--danger">
        <span v-if="jobErrorText" v-text="jobErrorText" />
        <TextBlock v-else :text="message.job.error" />
      </p>
      <p v-if="noticeText" class="agent-msg-notice agent-notice agent-notice--warning" v-text="noticeText" />

      <div v-if="waiting && approvals" class="agent-msg-approvals agent-surface">
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

const STATES: Record<string, { icon: string; tone: string; label: () => string }> = {
  ['queued' satisfies JobState]: { tone: 'neutral', icon: 'time', label: () => $gettext('Waiting') },
  ['running' satisfies JobState]: { tone: 'info', icon: 'loader-4', label: () => $gettext('Working') },
  ['waiting_approval' satisfies JobState]: { tone: 'warning', icon: 'hourglass', label: () => $gettext('Needs your approval') },
  ['succeeded' satisfies JobState]: { tone: 'success', icon: 'checkbox-circle', label: () => $gettext('Answered') },
  ['failed' satisfies JobState]: { tone: 'danger', icon: 'error-warning', label: () => $gettext('Could not answer') },
  ['cancelled' satisfies JobState]: { tone: 'neutral', icon: 'close-circle', label: () => $gettext('Cancelled') }
}

const state = computed(() => {
  const s = props.message.job?.state ?? ''
  const known = Object.hasOwn(STATES, s)
  return known
    ? { known, tone: STATES[s].tone, spin: s === ('running' satisfies JobState), icon: STATES[s].icon, label: STATES[s].label() }
    : { known, tone: 'danger', spin: false, icon: 'question', label: $gettext('Unknown state:') }
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
.agent-msg-status,
.agent-msg-notice,
.agent-msg-error {
  margin: 0;
}
.agent-msg-status {
  flex-wrap: wrap;
}
.agent-msg-approvals {
  padding: var(--agent-space-3);
}
.agent-msg-approvals p {
  margin: 0;
}
.agent-msg-approvals ul {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-1);
}
.agent-msg-approvals li {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
}
.agent-msg-tool,
.agent-msg-raw-state,
.agent-msg-raw-role {
  font-family: var(--agent-font-mono);
}
</style>
