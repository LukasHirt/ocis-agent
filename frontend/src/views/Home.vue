<template>
  <div class="agent-home agent-page">
    <oc-spinner :aria-label="$gettext('Loading the agent')" />
  </div>
</template>

<script setup lang="ts">
import { getCurrentInstance, onMounted, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { homeRoute } from '../routes'

// Home is the app root: it replaces itself with the first screen once the
// status is known (the layout shows it only then, but it waits anyway),
// and decides only once. The host's router is used (vue-router is never
// bundled), with replace so that Back does not return here.
const store = useAgentStore()
const instance = getCurrentInstance()
useGettext()

let done = false
const decide = () => {
  const to = homeRoute(store.status)
  if (done || !to) return
  done = true
  void instance?.proxy?.$router?.replace(to)
}
// from mount on: the host's router is reachable on the instance then
onMounted(() => {
  decide()
  watch(() => store.status, decide)
})
</script>
