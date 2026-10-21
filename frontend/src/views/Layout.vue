<template>
  <div class="agent-layout">
    <div class="agent-layout-top">
      <StatusBanner />
    </div>
    <main class="agent-main">
      <div v-if="!store.status" class="agent-layout-loading">
        <ErrorState
          v-if="store.errors.status"
          :title="store.errors.status"
          :icon="store.serviceDown ? 'wifi-off' : 'error-warning'"
          :busy="store.loading.status || store.reconnecting"
          @retry="retry"
        />
        <LoadingState v-else variant="spinner" :label="$gettext('Loading the agent')" />
      </div>
      <template v-else>
        <Enrol v-if="!store.status.enrolled" />
        <router-view v-else />
      </template>
    </main>
  </div>
</template>

<script setup lang="ts">
import '../styles/agent.css'
import { computed, getCurrentInstance, onBeforeUnmount, onMounted } from 'vue'
import { useDocumentTitle } from '@ownclouders/web-pkg'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import StatusBanner from '../components/StatusBanner.vue'
import { documentTitleSegments } from '../util/documentTitle'
import { useChatStore } from '../stores/chat'
import Enrol from './Enrol.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'

// Layout is the frame of every agent page: it keeps the store running while
// the app is open, shows the status banner, and the enrolment until the
// user has enabled the agent. <router-view> is the host's globally
// registered one (vue-router is not externalised, so it is never imported).
const store = useAgentStore()
const platform = usePlatform()
const { $gettext } = useGettext()
const chat = useChatStore()

// The host applies a route's meta.title only once at start, so the layout
// publishes the page title itself. The route is read reactively from the
// host's router through the instance (vue-router is never imported).
const instance = getCurrentInstance()
const titleSegments = computed(() => {
  const route = (instance?.proxy as { $route?: Record<string, unknown> } | null)?.$route
  const params = (route?.params ?? {}) as Record<string, unknown>
  const meta = (route?.meta ?? {}) as Record<string, unknown>
  const c =
    chat.conversations.find((x) => x.id === params.id) ??
    (chat.currentConversation?.id === params.id ? chat.currentConversation : undefined)
  return documentTitleSegments({
    name: route?.name as string | undefined,
    params,
    section: typeof meta.title === 'string' ? $gettext(meta.title) : '',
    app: $gettext('Agent'),
    task: $gettext('Task'),
    enrolled: store.status?.enrolled !== false,
    conversationTitle: c?.title
  })
})
useDocumentTitle({ titleSegments })

const retry = () => {
  void store.retryNow() // one status read, with the delays restarted during an outage
}

onMounted(() => store.start({ platform, gettext: $gettext }))
onBeforeUnmount(() => store.stop())
</script>

<style>
.agent-layout-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--agent-space-2);
  padding: var(--agent-space-5);
}
</style>
