<template>
  <section :class="['agent-chat', 'agent-page', 'agent-page--fill', { 'agent-chat--open': !!id }]">
    <aside class="agent-chat-side">
      <div class="agent-chat-side-head">
        <h1 class="agent-page-title" v-text="$gettext('Chat')" />
        <oc-button appearance="filled" variation="primary" size="small" :disabled="creating" @click="create">
          <oc-icon name="chat-new" size="small" />
          <span v-text="$gettext('New conversation')" />
        </oc-button>
      </div>
      <ConversationList @deleted="onDeleted" />
    </aside>

    <div class="agent-chat-main">
      <div v-if="chat.needsEnrol" class="agent-chat-enrol" role="alert">
        <span v-text="$gettext('The agent is not set up for you yet. Set it up first.')" />
        <oc-button appearance="filled" variation="primary" size="small" :disabled="enrolling" @click="enable" v-text="$gettext('Enable agent')" />
      </div>

      <EmptyState
        v-if="!id"
        class="agent-chat-pick"
        icon="chat-3"
        :title="$gettext('Choose a conversation or start a new one.')"
      />
      <template v-else-if="chat.missing">
        <EmptyState
          class="agent-chat-missing"
          role="alert"
          icon="chat-delete"
          :title="$gettext('This conversation no longer exists.')"
        />
      </template>
      <template v-else>
        <div class="agent-chat-head">
          <router-link class="agent-chat-back" :to="{ name: `${APPID}-chat` }">
            <oc-icon name="arrow-left" size="small" />
            <span v-text="$gettext('All conversations')" />
          </router-link>
          <h2 class="agent-chat-title agent-section-title">
            <TextBlock v-if="title" :text="title" />
            <span v-else v-text="$gettext('New conversation')" />
          </h2>
        </div>
        <ErrorState
          v-if="chat.messagesError && (!chat.messagesLoaded || !agent.serviceDown)"
          class="agent-chat-error"
          :title="chat.messagesError"
          :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
          :compact="chat.messagesLoaded"
          :busy="chat.messagesLoading || chat.conversationsLoading"
          @retry="retry"
        />
        <LoadingState
          v-if="!chat.messagesLoaded && !chat.messagesError"
          variant="bubbles"
          :count="3"
          :label="$gettext('Loading the messages')"
        />
        <Thread
          v-if="chat.messagesLoaded"
          :key="chat.current ?? ''"
          ref="threadView"
          :messages="chat.thread"
          :notices="chat.notices"
          :partials="chat.partials"
          :job-approvals="chat.jobApprovals"
          :older-cursor="chat.olderCursor"
          :older-loading="chat.olderLoading"
          :older-error="chat.olderError"
          :loaded="chat.messagesLoaded"
          @load-older="chat.loadOlder()"
        />
        <Composer
          ref="composer"
          v-model="draft"
          :sending="chat.sending"
          :disabled="notEnrolled"
          :hint="notEnrolled ? $gettext('Set up the agent to ask it questions.') : ''"
          :error="chat.sendError"
          @send="send"
        />
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, getCurrentInstance, nextTick, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import type { RouteLocationRaw } from 'vue-router'
import TextBlock from '../components/TextBlock.vue'
import EmptyState from '../components/state/EmptyState.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import ConversationList from './chat/ConversationList.vue'
import Thread from './chat/Thread.vue'
import Composer from './chat/Composer.vue'
import { APPID } from '../appid'
import { useAgentStore } from '../stores/agent'
import { useChatStore } from '../stores/chat'

// The chat screen: the conversations on the side, the open one (the
// route's id) with its thread and the send box. Messages appear when the
// server lists them (nothing optimistic); the drafts are kept per
// conversation in memory only.
const props = defineProps<{ id?: string }>()
const agent = useAgentStore()
const chat = useChatStore()
useGettext()

// navigate uses the host's router (vue-router is never bundled).
const instance = getCurrentInstance()
const navigate = (to: RouteLocationRaw) => {
  void instance?.proxy?.$router?.push(to)
}

let detach: (() => void) | null = null
onMounted(() => {
  detach = chat.attach()
  void chat.loadConversations()
})
onBeforeUnmount(() => {
  detach?.()
  detach = null
})

watch(
  () => props.id,
  (id) => {
    if (id) {
      chat.syncSession() // a new session forgot the conversation shown before
      // shown again after the screen was left: read what changed meanwhile
      if (id === chat.current && chat.messagesLoaded) void chat.refreshOpen()
      else void chat.openConversation(id)
    } else {
      chat.closeConversation()
    }
  },
  { immediate: true }
)

const title = computed(() => chat.currentConversation?.title ?? '')
const notEnrolled = computed(() => agent.status?.enrolled === false || chat.needsEnrol)

const draft = computed({
  get: () => (props.id ? (chat.draftByConversation[props.id] ?? '') : ''),
  set: (v: string) => {
    if (!props.id) return
    if (v === '') delete chat.draftByConversation[props.id]
    else chat.draftByConversation[props.id] = v
  }
})

const composer = ref<{ focus: () => void } | null>(null)
const threadView = ref<{ toBottom: () => void } | null>(null)

const send = async () => {
  const sent = await chat.send(draft.value)
  await nextTick()
  if (sent) threadView.value?.toBottom() // the new message, even when reading above
  composer.value?.focus() // the box was disabled while sending
}

// retry reads the open conversation again, and the list when that has no
// data either (one failure, one error, one retry).
const retry = () => {
  void chat.refreshOpen()
  if (!chat.conversationsLoaded) void chat.loadConversations()
  if (agent.serviceDown) void agent.retryNow()
}

const creating = ref(false)
const create = async () => {
  if (creating.value) return
  creating.value = true
  try {
    const c = await chat.createConversation()
    if (c) navigate({ name: `${APPID}-chat-conversation`, params: { id: c.id } })
  } finally {
    creating.value = false
  }
}

const onDeleted = (id: string) => {
  if (id === props.id) navigate({ name: `${APPID}-chat` })
}

const enrolling = ref(false)
const enable = async () => {
  if (enrolling.value) return
  enrolling.value = true
  try {
    await chat.enrol()
  } finally {
    enrolling.value = false
  }
}
</script>

<style>
.agent-chat {
  display: grid;
  grid-template-columns: var(--agent-pane) minmax(0, 1fr);
  grid-template-rows: minmax(0, 1fr);
  gap: 0;
  padding: 0;
}
@container agent (max-width: 999px) {
  .agent-chat {
    --agent-pane: 15rem;
  }
}
@container agent (max-width: 699px) {
  .agent-chat {
    grid-template-columns: minmax(0, 1fr);
  }
  .agent-chat--open .agent-chat-side {
    display: none;
  }
  .agent-chat:not(.agent-chat--open) .agent-chat-main {
    display: none;
  }
}
.agent-chat-side {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  border-right: var(--agent-line);
  background: var(--oc-color-background-muted);
}
.agent-chat-side-head {
  flex: none;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--agent-space-2);
  padding: var(--agent-space-4);
}
.agent-chat-side-head h1 {
  margin: 0;
}
.agent-chat-main {
  display: flex;
  flex-direction: column;
  gap: 0;
  min-width: 0;
  min-height: 0;
}
.agent-chat-head {
  flex: none;
  display: flex;
  align-items: center;
  gap: var(--agent-space-2);
  min-width: 0;
  padding: var(--agent-space-3) var(--agent-space-5);
  border-bottom: var(--agent-line);
}
.agent-chat-back {
  display: none;
  flex: none;
  align-items: center;
  gap: var(--agent-space-1);
  white-space: nowrap;
}
@container agent (max-width: 699px) {
  .agent-chat-back {
    display: inline-flex;
  }
}
.agent-chat-title {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.agent-chat-title .agent-text {
  white-space: nowrap !important;
}
.agent-chat-enrol,
.agent-chat-error {
  flex: none;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
  margin: var(--agent-space-3) var(--agent-space-5) 0;
}
.agent-chat-pick,
.agent-chat-missing {
  margin: auto;
}
</style>
