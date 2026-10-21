<template>
  <section :class="['agent-chat', 'agent-page', 'agent-page--fill', { 'agent-chat--open': !!id, 'agent-chat--solo': solo }]">
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

      <Landing
        v-if="!id"
        ref="landing"
        :name="agent.status?.user?.name ?? ''"
        :model-value="chat.landingDraft"
        :sending="chat.starting"
        :disabled="notEnrolled"
        :hint="notEnrolled ? $gettext('Set up the agent to ask it questions.') : ''"
        :error="chat.landingError"
        @update:model-value="onLandingInput"
        @send="startFromLanding"
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
            <TextBlock v-if="title" :text="title" wrap="nowrap" />
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
          @approvals-settled="chat.reloadJobApprovals()"
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
import Landing from './chat/Landing.vue'
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
const landing = ref<{ focus: () => void } | null>(null)

// with no conversation at all the landing is the whole screen
const solo = computed(
  () => chat.conversationsLoaded && chat.conversations.length === 0 && !chat.conversationsError
)

const onLandingInput = (v: string) => {
  chat.landingDraft = v
  chat.landingError = null
}

// startFromLanding creates the conversation, opens it and sends the text
// there; the conversation's box takes the focus afterwards (the landing's
// box when the landing is still shown).
const startFromLanding = async () => {
  const sent = await chat.startConversation(chat.landingDraft, (cid) =>
    navigate({ name: `${APPID}-chat-conversation`, params: { id: cid } })
  )
  await nextTick()
  if (sent) threadView.value?.toBottom()
  if (props.id) {
    composer.value?.focus()
    return
  }
  // the route never landed or the create failed: the landing is still the
  // screen, so it shows a refused send and keeps the focus
  await nextTick()
  landing.value?.focus()
}
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
  .agent-chat:not(.agent-chat--open) {
    grid-template-rows: auto minmax(0, 1fr);
  }
  .agent-chat:not(.agent-chat--open) .agent-chat-main {
    order: -1;
    border-bottom: var(--agent-line);
  }
  .agent-chat:not(.agent-chat--open) .agent-landing {
    padding: var(--agent-space-5) var(--agent-space-4);
    justify-content: flex-start;
  }
}
.agent-chat--solo {
  grid-template-columns: minmax(0, 1fr);
}
.agent-chat--solo .agent-chat-side {
  display: none;
}
.agent-chat.agent-chat--solo:not(.agent-chat--open) {
  grid-template-rows: minmax(0, 1fr);
}
.agent-chat.agent-chat--solo:not(.agent-chat--open) .agent-chat-main {
  border-bottom: 0;
}
.agent-chat-side {
  display: flex;
  flex-direction: column;
  min-width: 0;
  min-height: 0;
  border-right: var(--agent-line);
  background: var(--oc-color-background-default);
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
.agent-chat-enrol,
.agent-chat-error {
  flex: none;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
  margin: var(--agent-space-3) var(--agent-space-5) 0;
}
.agent-chat-missing {
  margin: auto;
}
</style>
