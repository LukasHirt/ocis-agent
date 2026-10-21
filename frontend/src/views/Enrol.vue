<template>
  <section class="agent-enrol agent-page">
    <div class="agent-enrol-card agent-surface">
    <span class="agent-state-icon agent-enrol-icon"><oc-icon name="robot" /></span>
    <h1 class="agent-page-title" v-text="$gettext('Set up your agent')" />
    <p
      class="agent-page-sub"
      v-text="
        $gettext(
          'The agent works for you in the background: it answers your questions about your files, keeps notes and prepares changes you ask for.'
        )
      "
    />
    <h2 class="agent-card-title" v-text="$gettext('What it can access')" />
    <ul class="agent-enrol-list">
      <li>
        <oc-icon name="shield-check" size="small" />
        <span
        v-text="
          $gettext(
            'It gets its own app token, which can technically do whatever you can do in ownCloud. You can renew or revoke that token in Settings at any time.'
          )
        "
        />
      </li>
      <li>
        <oc-icon name="shield-check" size="small" />
        <span v-text="$gettext('The agent is built to work only in your personal space.')" />
      </li>
      <li>
        <oc-icon name="shield-check" size="small" />
        <span
        v-text="
          $gettext(
            'Every change, deletion or sharing waits until you approve it here. Reading your files and keeping its own notes does not.'
          )
        "
        />
      </li>
    </ul>
    <p v-if="error" class="agent-enrol-error agent-notice agent-notice--danger" role="alert" v-text="error" />
    <div class="agent-enrol-actions">
      <oc-button appearance="filled" variation="primary" :disabled="enrolling" @click="enable" v-text="$gettext('Enable agent')" />
      <oc-spinner v-if="enrolling" :aria-label="$gettext('Enabling the agent')" />
    </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { describeError } from '../api/errors'

// Enrol is shown instead of the app while the user has not enabled the
// agent; enabling it creates the agent's app token.
const store = useAgentStore()
const { $gettext } = useGettext()
const enrolling = ref(false)
const error = ref<string | null>(null)

const enable = async () => {
  if (enrolling.value) return
  enrolling.value = true
  error.value = null
  try {
    await store.enrol()
  } catch (e) {
    error.value = describeError(e, $gettext)
  } finally {
    enrolling.value = false
  }
}
</script>

<style>
.agent-enrol-card {
  width: 100%;
  max-width: 40rem;
  margin: var(--agent-space-6) auto 0;
  gap: var(--agent-space-3);
  padding: var(--agent-space-5);
}
.agent-enrol-card h1,
.agent-enrol-card h2,
.agent-enrol-card p {
  margin: 0;
}
.agent-enrol-icon {
  align-self: flex-start;
}
.agent-enrol-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-3);
}
.agent-enrol-list li {
  display: flex;
  gap: var(--agent-space-2);
}
.agent-enrol-list .oc-icon {
  flex: none;
  margin-top: 2px;
  color: var(--oc-color-swatch-success-default);
}
.agent-enrol-list .oc-icon svg {
  fill: currentColor;
}
.agent-enrol-actions {
  display: flex;
  align-items: center;
  gap: var(--agent-space-2);
}
</style>
