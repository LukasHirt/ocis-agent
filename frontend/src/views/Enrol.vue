<template>
  <section class="agent-enrol oc-p-m">
    <h1 v-text="$gettext('Set up your agent')" />
    <p
      v-text="
        $gettext(
          'The agent works for you in the background: it answers your questions about your files, keeps notes and prepares changes you ask for.'
        )
      "
    />
    <h2 v-text="$gettext('What it can access')" />
    <ul>
      <li
        v-text="
          $gettext(
            'It gets its own app token with full access to your own space. You can renew or revoke that token in Settings at any time.'
          )
        "
      />
      <li v-text="$gettext('It works only in your personal space, never in other spaces or shares.')" />
      <li
        v-text="
          $gettext(
            'Every change, deletion or sharing waits until you approve it here. Reading your files and keeping its own notes does not.'
          )
        "
      />
    </ul>
    <p v-if="error" class="agent-enrol-error" role="alert" v-text="error" />
    <div class="agent-enrol-actions">
      <oc-button appearance="filled" :disabled="enrolling" @click="enable" v-text="$gettext('Enable agent')" />
      <oc-spinner v-if="enrolling" :aria-label="$gettext('Enabling the agent')" />
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
.agent-enrol {
  max-width: 48rem;
}
.agent-enrol-actions {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}
.agent-enrol-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
