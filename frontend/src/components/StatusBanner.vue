<template>
  <div class="agent-banners">
    <div
      v-if="view"
      :class="['agent-banner', `agent-banner-${view.severity}`, `agent-banner-${kind}`]"
      role="status"
    >
      <oc-icon :name="view.icon" :fill-type="view.fillType ?? 'fill'" />
      <div class="agent-banner-text">
        <span v-text="view.text" />
        <span v-if="view.sub" class="agent-banner-sub">
          <oc-spinner v-if="view.spinner" size="small" :aria-label="view.sub" />
          <span v-text="view.sub" />
        </span>
      </div>
      <oc-button
        v-if="kind === 'unreachable'"
        appearance="outline"
        size="small"
        :disabled="store.reconnecting"
        @click="store.retryNow()"
        v-text="$gettext('Retry')"
      />
      <oc-button
        v-else-if="kind === 'status_error'"
        appearance="outline"
        size="small"
        :disabled="store.loading.status"
        @click="store.refreshStatus()"
        v-text="$gettext('Try again')"
      />
      <oc-button
        v-else-if="kind === 'user'"
        appearance="outline"
        size="small"
        :disabled="busy"
        @click="act(() => store.resume('user'))"
        v-text="$gettext('Resume')"
      />
      <oc-button
        v-else-if="kind === 'reconnect'"
        appearance="outline"
        size="small"
        :disabled="busy"
        @click="act(() => store.enrol())"
        v-text="$gettext('Reconnect')"
      />
      <oc-button
        v-else-if="kind === 'token_anomaly'"
        appearance="outline"
        size="small"
        @click="$router.push({ name: 'agent-settings' })"
        v-text="$gettext('Open settings')"
      />
    </div>
    <span
      v-if="chip"
      class="agent-connection agent-chip agent-chip--neutral"
      role="status"
      :title="chip === 'polling' ? $gettext('Live updates are unavailable; refreshing regularly.') : undefined"
    >
      <oc-icon
        :name="chip === 'offline' ? 'refresh' : 'pause-circle'"
        :class="{ 'agent-spin': chip === 'offline' }"
        size="small"
      />
      <span v-text="chip === 'offline' ? $gettext('Reconnecting…') : $gettext('Live updates paused')" />
    </span>
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import { describeError } from '../api/errors'
import { connectionChipOf, topBannerOf, utcTime, type TopBanner } from '../util/banner'
import { formatTime } from '../util/format'

// StatusBanner shows the single most important state of the agent (text and
// icon, never colour alone) and a small chip while live updates are down.
// An unreachable service is shown here once; screens keep their data.
const store = useAgentStore()
const platform = usePlatform()
const { $gettext, interpolate } = useGettext()
const busy = ref(false)

const kind = computed(() =>
  topBannerOf({ serviceDown: store.serviceDown, statusError: store.errors.status, status: store.status })
)
const chip = computed(() => connectionChipOf(store.connection, store.serviceDown))

type Severity = 'danger' | 'warning' | 'info'
interface View {
  severity: Severity
  icon: string
  fillType?: 'none'
  text: string
  sub?: string
  spinner?: boolean
}

// fillType 'none' for icons the design system ships without a -fill variant
const views: Record<TopBanner, () => View> = {
  unreachable: () => ({
    severity: 'warning',
    icon: 'wifi-off',
    text: $gettext("The agent service can't be reached."),
    sub: store.reconnecting
      ? $gettext('Reconnecting…')
      : $gettext('Showing what was loaded before. Retrying automatically.'),
    spinner: store.reconnecting
  }),
  status_error: () => ({
    severity: 'danger',
    icon: 'error-warning',
    text: store.errors.status ?? '',
    sub: $gettext('What is shown may be out of date.')
  }),
  killswitch: () => ({
    severity: 'danger',
    icon: 'stop-circle',
    text: $gettext('The agent is stopped by the operator')
  }),
  reconnect: () => ({
    severity: 'danger',
    icon: 'link-unlink',
    fillType: 'none',
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
.agent-banners {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: var(--agent-space-3);
}
.agent-banners > .agent-chip {
  flex: none;
  margin: var(--agent-space-1) var(--agent-space-5) var(--agent-space-1) 0;
}
.agent-banner {
  --agent-tone: var(--oc-color-swatch-primary-default);
  flex: 1 1 auto;
  min-width: 0;
  min-height: 2.75rem;
  display: flex;
  align-items: center;
  gap: var(--agent-space-3);
  padding: var(--agent-space-2) var(--agent-space-5);
  border-bottom: var(--agent-line);
  border-left: 4px solid var(--agent-tone);
  background: color-mix(in srgb, var(--agent-tone) 10%, var(--oc-color-background-default));
  color: var(--oc-color-text-default);
  box-sizing: border-box;
}
.agent-banner > .oc-icon {
  flex: none;
}
.agent-banner > .oc-icon svg {
  fill: var(--agent-tone);
}
.agent-banner-danger { --agent-tone: var(--oc-color-swatch-danger-default); }
.agent-banner-warning { --agent-tone: var(--oc-color-swatch-warning-default); }
.agent-banner-info { --agent-tone: var(--oc-color-swatch-primary-default); }
.agent-banner-text {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  overflow-wrap: anywhere;
}
.agent-banner-sub {
  display: inline-flex;
  align-items: center;
  gap: var(--agent-space-1);
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
.agent-banner > .oc-button {
  flex: none;
}
</style>
