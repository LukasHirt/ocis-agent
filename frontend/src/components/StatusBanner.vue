<template>
  <div class="agent-banners">
    <div
      v-if="view"
      :class="['agent-banner', `agent-banner-${view.severity}`, `agent-banner-${kind}`]"
      role="status"
    >
      <oc-icon :name="view.icon" />
      <span class="agent-banner-text" v-text="view.text" />
      <oc-button
        v-if="kind === 'user'"
        size="small"
        :disabled="busy"
        @click="act(() => store.resume('user'))"
        v-text="$gettext('Resume')"
      />
      <oc-button
        v-else-if="kind === 'reconnect'"
        size="small"
        :disabled="busy"
        @click="act(() => store.enrol())"
        v-text="$gettext('Reconnect')"
      />
      <oc-button
        v-else-if="kind === 'token_anomaly'"
        size="small"
        @click="$router.push({ name: 'agent-settings' })"
        v-text="$gettext('Open settings')"
      />
    </div>
    <p v-if="note" class="agent-connection" role="status">
      <oc-icon :name="note === 'offline' ? 'wifi-off' : 'refresh'" size="small" />
      <span v-text="noteText" />
    </p>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import { describeError } from '../api/errors'
import { bannerOf, connectionNoteOf, utcTime, type BannerKind } from '../util/banner'
import { formatTime } from '../util/format'

// StatusBanner shows the single most severe state of the agent (text and
// icon, never colour alone) and a small note while live updates are down.
const store = useAgentStore()
const platform = usePlatform()
const { $gettext, interpolate } = useGettext()
const busy = ref(false)

const kind = computed(() => bannerOf(store.status))
const note = computed(() => connectionNoteOf(store.connection))

type Severity = 'danger' | 'warning' | 'info'

const views: Record<BannerKind, () => { severity: Severity; icon: string; text: string }> = {
  killswitch: () => ({
    severity: 'danger',
    icon: 'stop-circle',
    text: $gettext('The agent is stopped by the operator')
  }),
  reconnect: () => ({
    severity: 'danger',
    icon: 'link-unlink',
    text: $gettext('Reconnect needed: the agent has lost access to your files')
  }),
  token_anomaly: () => ({
    severity: 'danger',
    icon: 'alarm-warning',
    text: $gettext('Possible token misuse: check your app tokens in Settings')
  }),
  paused: () => ({ severity: 'warning', icon: 'pause-circle', text: $gettext('The agent is paused') }),
  user: () => ({ severity: 'warning', icon: 'pause-circle', text: $gettext('You paused the agent') }),
  dormant: () => ({
    severity: 'info',
    icon: 'time',
    text: $gettext('Paused because you were away; it resumes when you visit')
  }),
  budget: () => {
    // resets_at is always the next UTC midnight
    const at = store.status?.budget?.resets_at ?? null
    const time = utcTime(at)
    return {
      severity: 'warning',
      icon: 'hourglass',
      text: time
        ? interpolate(
            $gettext('Daily token budget used up; the agent continues tomorrow at %{time} UTC (%{local} your time)'),
            { time, local: formatTime(at as string) }
          )
        : $gettext('Daily token budget used up; the agent continues tomorrow')
    }
  }
}

const view = computed(() => (kind.value ? views[kind.value]() : null))

const noteText = computed(() =>
  note.value === 'offline'
    ? $gettext('Connection lost; reconnecting…')
    : $gettext('Live updates are unavailable; refreshing regularly.')
)

const act = async (fn: () => Promise<unknown>) => {
  if (busy.value) return
  busy.value = true
  try {
    await fn()
  } catch (e) {
    platform.notify('error', describeError(e, $gettext))
  } finally {
    busy.value = false
  }
}
</script>

<style>
.agent-banner {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  padding: 0.5rem 1rem;
  border-left: 4px solid currentColor;
  margin-bottom: 0.5rem;
}
.agent-banner-text {
  flex: 1;
}
.agent-banner-danger {
  color: var(--oc-color-swatch-danger-default, #b00020);
  background: var(--oc-color-swatch-danger-muted, #fdecee);
}
.agent-banner-warning {
  color: var(--oc-color-swatch-warning-default, #8a5a00);
  background: var(--oc-color-swatch-warning-muted, #fff4e0);
}
.agent-banner-info {
  color: var(--oc-color-swatch-passive-default, #333);
  background: var(--oc-color-background-muted, #f2f2f2);
}
.agent-connection {
  display: flex;
  align-items: center;
  gap: 0.25rem;
  font-size: 0.85em;
  margin: 0 0 0.5rem;
  color: var(--oc-color-text-muted, #666);
}
</style>
