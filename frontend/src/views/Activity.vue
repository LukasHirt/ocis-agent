<template>
  <section class="agent-activity agent-page">
    <header class="agent-page-head agent-page-head--sticky">
      <div class="agent-page-titles">
        <h1 class="agent-page-title" v-text="$gettext('Activity')" />
        <p
          class="agent-page-sub agent-activity-intro"
          v-text="$gettext('What the agent did and the changes you made, as recorded in the audit log.')"
        />
      </div>
      <div class="agent-page-actions">
        <oc-button size="small" appearance="outline" :disabled="audit.loading" @click="audit.refresh()">
          <oc-icon name="refresh" size="small" />
          <span v-text="$gettext('Refresh')" />
        </oc-button>
        <template v-if="audit.exporting">
          <span class="agent-activity-export-progress agent-meta" role="status" v-text="progress" />
          <oc-button size="small" appearance="outline" @click="audit.abortExport()">
            <oc-icon name="close" size="small" />
            <span v-text="$gettext('Cancel export')" />
          </oc-button>
        </template>
        <oc-button v-else size="small" appearance="outline" @click="audit.exportLog()">
          <oc-icon name="download" size="small" />
          <span v-text="$gettext('Export')" />
        </oc-button>
      </div>
    </header>
    <div class="agent-page-body">
      <div class="agent-toolbar agent-activity-tools">
        <oc-select
          class="agent-activity-kind"
          :model-value="selected"
          :options="options"
          option-label="label"
          :clearable="false"
          :searchable="false"
          data-testid="agent-activity-kind"
          :label="$gettext('Show')"
          @update:model-value="(o: KindOption | null) => o && void audit.setKind(o.value)"
        />
        <VerifyPanel class="agent-activity-verify" />
      </div>
      <p v-if="audit.exportError" role="alert" class="agent-activity-export-error agent-notice agent-notice--danger" v-text="audit.exportError" />

      <ErrorState
        v-if="audit.error && (!audit.loaded || !agent.serviceDown)"
        class="agent-activity-error"
        :title="audit.error"
        :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
        :compact="audit.loaded"
        :busy="audit.loading"
        @retry="retry"
      />
      <LoadingState
        v-if="!audit.loaded && !audit.error"
        variant="rows"
        :count="6"
        :label="$gettext('Loading the activity')"
      />

      <template v-if="audit.loaded">
        <section v-for="g in days" :key="g.key" class="agent-day">
          <h2 class="agent-day-head" v-text="g.label" />
          <ul class="agent-activity-list agent-rows">
            <li v-for="e in g.items" :key="e.seq"><EventRow :event="e" /></li>
          </ul>
        </section>
        <EmptyState
          v-if="audit.events.length === 0"
          class="agent-activity-empty"
          icon="history"
          :title="audit.kind === '' ? $gettext('No activity yet.') : $gettext('No events of this kind.')"
        />
        <div v-if="audit.olderLoading || audit.hasOlder" class="agent-activity-more">
          <oc-spinner v-if="audit.olderLoading" size="small" :aria-label="$gettext('Loading older events')" />
          <oc-button
            v-else
            appearance="outline"
            size="small"
            @click="audit.loadOlder()"
            v-text="$gettext('Load older')"
          />
        </div>
      </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useGettext } from 'vue3-gettext'
import EventRow from './activity/EventRow.vue'
import VerifyPanel from './activity/VerifyPanel.vue'
import EmptyState from '../components/state/EmptyState.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import { useAgentStore } from '../stores/agent'
import { useAuditStore } from '../stores/audit'
import { KNOWN_KINDS, kindLabel } from './activity/kinds'
import { formatBytes } from '../util/format'
import { groupByDay } from '../util/days'

// The activity screen: the audit log newest first with older pages on
// request, a kind filter (applied by the server), the chain check and the
// export. It listens for changes only while shown; leaving it stops a
// running export.
const audit = useAuditStore()
const agent = useAgentStore()
audit.syncSession() // the first render never shows an earlier session's events
const { $gettext, interpolate } = useGettext()

const retry = () => {
  void audit.refresh()
  if (agent.serviceDown) void agent.retryNow()
}

interface KindOption {
  value: string
  label: string
}

const options = computed<KindOption[]>(() => [
  { value: '', label: $gettext('All events') },
  ...KNOWN_KINDS.map((value) => ({ value, label: kindLabel(value, $gettext) ?? value })).sort((a, b) =>
    a.label.localeCompare(b.label)
  )
])
const selected = computed(() => options.value.find((o) => o.value === audit.kind) ?? options.value[0])

const days = computed(() => groupByDay(audit.events, $gettext('Unknown date')))

const progress = computed(() =>
  interpolate($gettext('Downloading… %{size}'), { size: formatBytes(audit.exportBytes) })
)

let detach: (() => void) | null = null
onMounted(() => {
  detach = audit.attach()
  void audit.refresh()
})
onBeforeUnmount(() => {
  detach?.()
  detach = null
  audit.abortExport()
})
</script>

<style>
.agent-activity {
  min-width: 0;
}
.agent-activity-kind {
  min-width: 14rem;
}
.agent-activity-tools {
  justify-content: space-between;
  align-items: flex-end;
}
.agent-activity-verify {
  margin-left: auto;
}
.agent-day {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  min-width: 0;
}
.agent-day + .agent-day {
  margin-top: var(--agent-space-3);
}
.agent-day-head {
  display: flex;
  align-items: center;
  gap: var(--agent-space-2);
  margin: 0;
  font-size: var(--agent-font-xs);
  font-weight: 600;
  color: var(--oc-color-text-muted);
}
.agent-day-head::after {
  content: '';
  flex: 1;
  border-top: var(--agent-line);
}
.agent-activity-more {
  display: flex;
  justify-content: center;
}
.agent-activity-error,
.agent-activity-export-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
}
</style>
