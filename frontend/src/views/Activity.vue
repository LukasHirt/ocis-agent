<template>
  <section class="agent-activity agent-page">
    <h1 v-text="$gettext('Activity')" />
    <p
      class="agent-activity-intro"
      v-text="$gettext('What the agent did and the changes you made, as recorded in the audit log.')"
    />

    <div class="agent-activity-tools">
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
      <oc-button size="small" :disabled="audit.loading" @click="audit.refresh()">
        <oc-icon name="refresh" size="small" />
        <span v-text="$gettext('Refresh')" />
      </oc-button>
      <template v-if="audit.exporting">
        <span class="agent-activity-export-progress" role="status" v-text="progress" />
        <oc-button size="small" @click="audit.abortExport()">
          <oc-icon name="close" size="small" />
          <span v-text="$gettext('Cancel export')" />
        </oc-button>
      </template>
      <oc-button v-else size="small" @click="audit.exportLog()">
        <oc-icon name="download" size="small" />
        <span v-text="$gettext('Export')" />
      </oc-button>
    </div>
    <p v-if="audit.exportError" role="alert" class="agent-activity-export-error" v-text="audit.exportError" />

    <VerifyPanel />

    <div v-if="audit.error" class="agent-activity-error">
      <p role="alert" v-text="audit.error" />
      <oc-button size="small" :disabled="audit.loading" @click="audit.refresh()" v-text="$gettext('Try again')" />
    </div>
    <oc-spinner v-if="!audit.loaded && !audit.error" :aria-label="$gettext('Loading the activity')" />

    <template v-if="audit.loaded">
      <ul class="agent-activity-list">
        <li v-for="e in audit.events" :key="e.seq"><EventRow :event="e" /></li>
      </ul>
      <p
        v-if="audit.events.length === 0"
        class="agent-activity-empty"
        v-text="audit.kind === '' ? $gettext('No activity yet.') : $gettext('No events of this kind.')"
      />
      <oc-spinner v-if="audit.olderLoading" size="small" :aria-label="$gettext('Loading older events')" />
      <oc-button
        v-else-if="audit.hasOlder"
        appearance="outline"
        size="small"
        @click="audit.loadOlder()"
        v-text="$gettext('Load older')"
      />
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted } from 'vue'
import { useGettext } from 'vue3-gettext'
import EventRow from './activity/EventRow.vue'
import VerifyPanel from './activity/VerifyPanel.vue'
import { useAuditStore } from '../stores/audit'
import { KNOWN_KINDS, kindLabel } from './activity/kinds'
import { formatBytes } from '../util/format'

// The activity screen: the audit log newest first with older pages on
// request, a kind filter (applied by the server), the chain check and the
// export. It listens for changes only while shown; leaving it stops a
// running export.
const audit = useAuditStore()
audit.syncSession() // the first render never shows an earlier session's events
const { $gettext, interpolate } = useGettext()

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
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-activity h1,
.agent-activity p {
  margin: 0;
}
.agent-activity-tools {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: 0.75rem;
}
.agent-activity-kind {
  min-width: 16rem;
}
.agent-activity-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  min-width: 0;
}
.agent-activity-error,
.agent-activity-export-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
