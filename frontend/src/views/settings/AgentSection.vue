<template>
  <section v-if="status" class="agent-settings-section agent-settings-agent" aria-labelledby="agent-settings-agent-h">
    <h2 id="agent-settings-agent-h" class="agent-section-title" tabindex="-1" v-text="$gettext('Agent')" />
    <dl class="agent-settings-facts">
      <dt v-text="$gettext('Name')" />
      <dd><TextBlock :text="status.user.name" /></dd>
      <dt v-text="$gettext('Login')" />
      <dd><TextBlock :text="status.user.login" mono /></dd>
    </dl>

    <p class="agent-settings-state" role="status" v-text="stateText" />
    <div class="agent-settings-actions">
      <oc-button v-if="!status.paused" :disabled="busy" @click="run(() => store.pause())" v-text="$gettext('Pause')" />
      <oc-button
        v-else-if="status.pause_reason === 'user'"
        :disabled="busy"
        @click="run(() => store.resume('user'))"
        v-text="$gettext('Resume')"
      />
      <template v-else-if="status.pause_reason === 'token_anomaly'">
        <oc-button
          :disabled="busy"
          @click="run(() => store.resume('token_anomaly'))"
          v-text="$gettext('I have reviewed my app tokens — resume')"
        />
      </template>
      <oc-button
        v-else-if="isCredentialPause"
        :disabled="busy"
        @click="run(() => store.enrol())"
        v-text="$gettext('Reconnect')"
      />
    </div>

    <p class="agent-settings-killswitch agent-settings-hint" v-text="killSwitchText" />
    <p
      v-if="store.settings"
      v-text="
        interpolate(
          $gettext('The agent pauses itself when you have not visited for %{days} days.'),
          { days: store.settings.dormancy_days },
          true
        )
      "
    />

    <div class="agent-settings-danger">
    <h3 class="agent-card-title" v-text="$gettext('Danger zone')" />
    <oc-button
      v-if="!dialog"
      appearance="outline"
      variation="danger"
      @click="openDialog"
      v-text="$gettext('Disable agent')"
    />
    <div
      v-else
      class="agent-settings-dialog"
      role="alertdialog"
      aria-labelledby="agent-settings-disable-h"
      aria-describedby="agent-settings-disable-d"
    >
      <h3 id="agent-settings-disable-h" class="agent-card-title" v-text="$gettext('Disable the agent?')" />
      <p
        id="agent-settings-disable-d"
        v-text="
          $gettext(
            'The agent stops all its work and its app tokens are deleted, together with its settings and tasks. The audit log is kept. Your files stay as they are.'
          )
        "
      />
      <oc-checkbox v-model="deleteMemory" :disabled="disabling" :label="$gettext('Also delete the agent\'s memory folder')" />
      <p v-if="dialogError" role="alert" class="agent-settings-error">
        <span v-text="dialogError.message" />
        <span v-if="dialogError.detail" v-text="' ' + dialogError.detail" />
      </p>
      <div class="agent-settings-actions">
        <oc-button :disabled="disabling" @click="closeDialog" v-text="$gettext('Cancel')" />
        <oc-button
          appearance="filled"
          variation="danger"
          :disabled="disabling"
          @click="disable"
          v-text="$gettext('Disable agent')"
        />
      </div>
    </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../../stores/agent'
import { usePlatform } from '../../platform'
import TextBlock from '../../components/TextBlock.vue'
import { useReport, type ErrorText } from './report'
import type { PauseReason } from '../../api/types'

// AgentSection: who the agent works for, pause and resume, the operator's
// kill switch and disabling (unenrolling) the agent.
const store = useAgentStore()
const platform = usePlatform()
const { $gettext, interpolate } = useGettext()
const { describe, notifyError } = useReport()

const status = computed(() => store.status)
const busy = ref(false)

const isCredentialPause = computed(() => (status.value?.pause_reason ?? '').startsWith('credential_'))

const stateText = computed(() => {
  const s = status.value
  if (!s?.paused) return $gettext('The agent is running.')
  switch (s.pause_reason) {
    case 'user' satisfies PauseReason:
      return $gettext('You paused the agent.')
    case 'dormant' satisfies PauseReason:
      return $gettext('Paused because you were away; it resumes when you visit.')
    case 'token_anomaly' satisfies PauseReason:
      return $gettext(
        'Paused because an unknown app token of the agent was found. Check your app tokens below (Incident revoke lists them all) before you resume.'
      )
    case 'credential_invalid' satisfies PauseReason:
    case 'credential_revoked' satisfies PauseReason:
    case 'credential_expired' satisfies PauseReason:
      return $gettext('Paused because the agent lost access to your files. Reconnect it to continue.')
  }
  return $gettext('The agent is paused.')
})

const killSwitchText = computed(() => {
  const k = status.value?.kill_switch
  return k?.database || k?.environment
    ? $gettext('The operator has stopped the agent for everyone. Nothing runs until they turn it on again.')
    : $gettext("The operator's emergency stop is off.")
})

const run = async (action: () => Promise<unknown>) => {
  if (busy.value) return
  busy.value = true
  try {
    await action()
  } catch (e) {
    notifyError(e)
  } finally {
    busy.value = false
  }
}

const dialog = ref(false)
const deleteMemory = ref(false)
const disabling = ref(false)
const dialogError = ref<ErrorText | null>(null)

const openDialog = () => {
  deleteMemory.value = false
  dialogError.value = null
  dialog.value = true
}

const closeDialog = () => {
  if (!disabling.value) dialog.value = false
}

const disable = async () => {
  if (disabling.value) return
  disabling.value = true
  dialogError.value = null
  try {
    await store.unenrol(deleteMemory.value)
    dialog.value = false
    platform.notify('success', $gettext('The agent is disabled'))
  } catch (e) {
    dialogError.value = describe(e)
  } finally {
    disabling.value = false
  }
}
</script>
