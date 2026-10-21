<template>
  <div class="agent-layout">
    <div class="agent-layout-top">
      <StatusBanner />
      <div v-if="store.status && store.errors.status" class="agent-layout-stale" role="alert">
        <oc-icon name="error-warning" size="small" />
        <span v-text="store.errors.status" />
        <span v-text="$gettext('What is shown may be out of date.')" />
        <oc-button
          size="small"
          :disabled="store.loading.status"
          @click="store.refreshStatus()"
          v-text="$gettext('Try again')"
        />
      </div>
    </div>
    <main class="agent-main">
      <div v-if="!store.status" class="agent-layout-loading">
        <template v-if="store.errors.status">
          <p role="alert" v-text="store.errors.status" />
          <oc-button :disabled="store.loading.status" @click="store.refreshStatus()" v-text="$gettext('Try again')" />
        </template>
        <oc-spinner v-else :aria-label="$gettext('Loading the agent')" />
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
import { onBeforeUnmount, onMounted } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import StatusBanner from '../components/StatusBanner.vue'
import Enrol from './Enrol.vue'

// Layout is the frame of every agent page: it keeps the store running while
// the app is open, shows the status banner, and the enrolment until the
// user has enabled the agent. <router-view> is the host's globally
// registered one (vue-router is not externalised, so it is never imported).
const store = useAgentStore()
const platform = usePlatform()
const { $gettext } = useGettext()

onMounted(() => store.start({ platform, gettext: $gettext }))
onBeforeUnmount(() => store.stop())
</script>

<style>
.agent-layout-stale {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem 0.5rem;
  font-size: 0.85em;
  margin: 0 0 0.5rem;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-layout-loading {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 0.5rem;
  padding: 2rem;
}
</style>
