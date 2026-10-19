<template>
  <section class="agent-chat oc-p-m">
    <aside class="agent-chat-side">
      <div class="agent-chat-side-head">
        <h1 v-text="$gettext('Chat')" />
        <oc-button appearance="filled" size="small" :disabled="creating" @click="create">
          <oc-icon name="chat-new" size="small" />
          <span v-text="$gettext('New conversation')" />
        </oc-button>
      </div>
      <ConversationList @deleted="onDeleted" />
    </aside>

    <div class="agent-chat-main">
      <div v-if="chat.needsEnrol" class="agent-chat-enrol" role="alert">
        <span v-text="$gettext('The agent is not set up for you yet. Set it up first.')" />
        <oc-button appearance="filled" size="small" :disabled="enrolling" @click="enable" v-text="$gettext('Enable agent')" />
      </div>

      <p v-if="!id" class="agent-chat-pick" v-text="$gettext('Choose a conversation or start a new one.')" />
      <template v-else-if="chat.missing">
        <p class="agent-chat-missing" role="alert" v-text="$gettext('This conversation no longer exists.')" />
      </template>
      <template v-else>
        <h2 class="agent-chat-title">
          <TextBlock v-if="title" :text="title" />
          <span v-else v-text="$gettext('New conversation')" />
        </h2>
        <div v-if="chat.messagesError" class="agent-chat-error">
          <p role="alert" v-text="chat.messagesError" />
          <oc-button size="small" :disabled="chat.messagesLoading" @click="chat.refreshOpen()" v-text="$gettext('Try again')" />
        </div>
        <oc-spinner
          v-if="!chat.messagesLoaded && !chat.messagesError"
          :aria-label="$gettext('Loading the messages')"
        />
        <Thread
          v-if="chat.messagesLoaded"
          :key="chat.current ?? ''"
          ref="threadView"
          :messages="chat.thread"
          :notices="chat.notices"
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
  grid-template-columns: minmax(12rem, 18rem) minmax(0, 1fr);
  gap: 1rem;
  align-items: start;
  min-height: 0;
}
@media (max-width: 640px) {
  .agent-chat {
    grid-template-columns: minmax(0, 1fr);
  }
}
.agent-chat-side {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-chat-side-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
}
.agent-chat-side-head h1 {
  margin: 0;
}
.agent-chat-main {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
  max-height: calc(100vh - 10rem);
}
.agent-chat-title {
  margin: 0;
  overflow-wrap: anywhere;
}
.agent-chat-enrol,
.agent-chat-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
.agent-chat-error,
.agent-chat-missing {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
