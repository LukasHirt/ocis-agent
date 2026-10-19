<template>
  <section class="agent-approvals oc-p-m">
    <h1 v-text="title" />
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'

// The approvals inbox (a skeleton until the inbox lands).
const store = useAgentStore()
const { $gettext, interpolate } = useGettext()
const title = computed(() =>
  interpolate($gettext('Approvals (%{count})'), { count: store.approvals.counts.pending })
)
</script>
