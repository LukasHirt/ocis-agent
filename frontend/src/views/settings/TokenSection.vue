<template>
  <section class="agent-settings-section agent-settings-tokens" aria-labelledby="agent-settings-tokens-h">
    <h2 id="agent-settings-tokens-h" v-text="$gettext('App token')" />
    <p v-if="status" v-text="tokenText" />
    <p
      v-if="status?.token.expires_at"
      v-text="
        $gettextInterpolate($gettext('It expires on %{time}.'), { time: formatTime(status.token.expires_at) }, true)
      "
    />
    <p
      class="agent-settings-hint"
      v-text="$gettext('The agent renews its token by itself. Renew it now if you want a fresh one at once.')"
    />
    <div class="agent-settings-actions">
      <oc-button :disabled="busy" @click="renew" v-text="$gettext('Renew now')" />
      <oc-button :disabled="busy" variation="danger" @click="revoke" v-text="$gettext('Revoke the agent\'s tokens')" />
    </div>

    <div v-if="revokedAll" class="agent-settings-revoked" role="status" data-testid="agent-revoked-all">
      <p v-text="$gettext('Agent disabled; enable it again to continue.')" />
      <p
        v-text="
          $gettext(
            'All your app tokens were deleted. Apps and devices that used an app password must be set up again.'
          )
        "
      />
      <oc-button appearance="filled" :disabled="busy" @click="enableAgain" v-text="$gettext('Enable agent')" />
    </div>

    <h3 v-text="$gettext('Incident revoke')" />
    <p
      v-text="
        $gettext(
          'If you think someone else got hold of the agent\'s token, revoke ALL your app tokens. A stolen token can create new tokens with any name, so only deleting all of them is safe.'
        )
      "
    />
    <oc-button
      v-if="!incident"
      variation="danger"
      :disabled="busy"
      @click="openIncident"
      v-text="$gettext('I suspect compromise')"
    />
    <div
      v-else
      class="agent-settings-dialog"
      role="alertdialog"
      aria-labelledby="agent-settings-incident-h"
      aria-describedby="agent-settings-incident-d"
    >
      <h4 id="agent-settings-incident-h" v-text="$gettext('Revoke all your app tokens?')" />
      <p
        id="agent-settings-incident-d"
        class="agent-settings-warning"
        v-text="
          $gettext(
            'This deletes ALL your app tokens, including the app passwords of your other apps and devices, and disables the agent.'
          )
        "
      />
      <oc-spinner v-if="loadingList" :aria-label="$gettext('Loading your app tokens')" />
      <template v-else-if="listError">
        <p role="alert" class="agent-settings-error" v-text="listError.message" />
        <oc-button @click="loadList" v-text="$gettext('Try again')" />
      </template>
      <template v-else-if="list">
        <p v-if="list.length === 0" v-text="$gettext('You have no app tokens.')" />
        <table v-else class="agent-settings-token-table">
          <thead>
            <tr>
              <th scope="col" v-text="$gettext('Label')" />
              <th scope="col" v-text="$gettext('Created')" />
              <th scope="col" v-text="$gettext('Expires')" />
            </tr>
          </thead>
          <tbody>
            <tr v-for="(t, i) in list" :key="i" data-testid="agent-token-row">
              <td>
                <TextBlock :text="t.label" mono />
                <span v-if="t.ours" class="agent-settings-badge" v-text="$gettext('agent token')" />
              </td>
              <td v-text="t.created_at ? formatTime(t.created_at) : '—'" />
              <td v-text="t.expires_at ? formatTime(t.expires_at) : '—'" />
            </tr>
          </tbody>
        </table>
        <oc-text-input
          v-model="typed"
          data-testid="agent-revoke-confirm"
          autocomplete="off"
          :label="$gettext('Type REVOKE to confirm')"
        />
        <p v-if="revokeError" role="alert" class="agent-settings-error">
          <span v-text="revokeError.message" />
          <span v-if="revokeError.detail" v-text="' ' + revokeError.detail" />
        </p>
      </template>
      <div class="agent-settings-actions">
        <oc-button :disabled="revoking" @click="closeIncident" v-text="$gettext('Cancel')" />
        <oc-button
          v-if="list"
          appearance="filled"
          variation="danger"
          :disabled="!confirmed || revoking"
          @click="revokeAll"
          v-text="$gettext('Revoke all my app tokens')"
        />
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../../stores/agent'
import { usePlatform } from '../../platform'
import { isAbortError } from '../../api/errors'
import type { TokenStatus, TokenSummary } from '../../api/types'
import TextBlock from '../../components/TextBlock.vue'
import { formatTime } from '../../util/format'
import { useReport, type ErrorText } from './report'

// The word the user must type before the incident revoke is sent.
const CONFIRM_WORD = 'REVOKE'

// TokenSection: renew or revoke the agent's app token, and the incident
// revoke of ALL the user's app tokens (listed first, confirmed by typing).
const store = useAgentStore()
const platform = usePlatform()
const { $gettext, $ngettext, interpolate } = useGettext()
const { describe, notifyError } = useReport()

const status = computed(() => store.status)
const busy = ref(false)
let alive = true
onBeforeUnmount(() => {
  alive = false
})

const tokenText = computed(() => {
  switch (status.value?.token.status) {
    case 'valid' satisfies TokenStatus:
      return $gettext("The agent's token is valid.")
    case 'invalid' satisfies TokenStatus:
      return $gettext("The agent's token no longer works. Reconnect the agent.")
    case 'revoked' satisfies TokenStatus:
      return $gettext("The agent's token was revoked.")
    case 'none' satisfies TokenStatus:
      return $gettext('The agent has no token.')
  }
  return $gettext("The state of the agent's token is unknown.")
})

const run = async (action: () => Promise<void>) => {
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

const renew = () =>
  run(async () => {
    await store.renewToken()
    platform.notify('success', $gettext("The agent's token was renewed"))
  })

const revoke = () =>
  run(async () => {
    const ok = await platform.confirm({
      title: $gettext("Revoke the agent's tokens?"),
      message: $gettext(
        'The agent loses access to your files and stops working until you reconnect it. Your other app tokens stay.'
      ),
      confirmText: $gettext('Revoke'),
      danger: true
    })
    if (!ok) return
    const r = await store.revokeToken()
    platform.notify(
      'success',
      interpolate($ngettext('Revoked %{n} token', 'Revoked %{n} tokens', r.revoked), { n: r.revoked }, true)
    )
  })

const incident = ref(false)
const list = ref<TokenSummary[] | null>(null)
const loadingList = ref(false)
const listError = ref<ErrorText | null>(null)
const typed = ref('')
const revoking = ref(false)
const revokeError = ref<ErrorText | null>(null)
const revokedAll = ref(false)
const confirmed = computed(() => typed.value === CONFIRM_WORD)

// listSeq numbers the preview requests: only the newest one of the open
// dialog may fill it (a dialog closed and reopened waits for its own).
let listSeq = 0

const loadList = async () => {
  const seq = ++listSeq
  loadingList.value = true
  listError.value = null
  try {
    const r = await store.previewRevokeAll()
    if (!alive || seq !== listSeq) return
    list.value = Array.isArray(r?.tokens) ? r.tokens : []
  } catch (e) {
    if (!alive || seq !== listSeq || isAbortError(e)) return
    listError.value = describe(e)
  } finally {
    if (seq === listSeq) loadingList.value = false
  }
}

const openIncident = () => {
  incident.value = true
  list.value = null
  typed.value = ''
  revokeError.value = null
  void loadList()
}

const closeIncident = () => {
  if (revoking.value) return
  incident.value = false
  listSeq++ // a preview still in flight is for nobody now
  loadingList.value = false
  list.value = null
  typed.value = ''
}

const revokeAll = async () => {
  if (revoking.value || !confirmed.value) return
  revoking.value = true
  revokeError.value = null
  try {
    await store.revokeAll()
    if (!alive) return
    incident.value = false
    list.value = null
    typed.value = ''
    revokedAll.value = true
  } catch (e) {
    revokeError.value = describe(e)
  } finally {
    revoking.value = false
  }
}

const enableAgain = () =>
  run(async () => {
    await store.enrol()
    revokedAll.value = false
  })
</script>
