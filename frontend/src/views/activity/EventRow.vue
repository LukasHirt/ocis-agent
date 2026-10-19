<template>
  <article class="agent-event">
    <div class="agent-event-head">
      <span class="agent-event-kind">
        <span v-if="label" v-text="label" />
        <template v-else>
          <span v-text="$gettext('Unknown event:')" />
          <TextBlock :text="event.kind" class="agent-event-raw" />
        </template>
      </span>
      <time class="agent-event-time" :datetime="event.ts" v-text="formatTime(event.ts)" />
    </div>
    <div class="agent-event-meta">
      <span class="agent-event-by">
        <span v-text="$gettext('By')" />
        <span class="agent-event-actor">
          <span v-if="actor" v-text="actor" />
          <TextBlock v-else :text="event.cred" />
        </span>
      </span>
      <TextBlock v-if="event.tool" class="agent-event-tool" :text="event.tool" />
      <RiskBadge v-if="event.risk" :risk="event.risk" />
      <span v-if="event.outcome" class="agent-event-outcome">
        <span v-text="$gettext('Outcome:')" />
        <TextBlock :text="event.outcome" />
      </span>
      <span v-if="event.job_id" class="agent-event-job">
        <router-link :to="{ name: `${APPID}-tasks-job`, params: { id: event.job_id } }" v-text="$gettext('Open task')" />
      </span>
    </div>
    <details v-if="event.detail" class="agent-event-detail">
      <summary v-text="diff !== null ? $gettext('Changes') : $gettext('Details')" />
      <template v-if="diff !== null">
        <p v-if="file !== null" class="agent-event-file-line">
          <span v-text="$gettext('File:')" />
          <TextBlock class="agent-event-file" :text="file" />
        </p>
        <div class="agent-diff">
          <div v-for="(line, i) in diff" :key="i" :class="['agent-diff-line', `agent-diff-${line.kind}`]">
            <TextBlock :text="line.text" mono />
          </div>
        </div>
      </template>
      <TextBlock v-else :text="event.detail" mono />
    </details>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import RiskBadge from '../../components/RiskBadge.vue'
import { APPID } from '../../appid'
import { formatTime } from '../../util/format'
import { credLabel, diffLines, kindLabel, memoryEdit, type DiffLine } from './kinds'
import type { AuditEvent } from '../../api/types'

// EventRow is one audit event: its kind, time, who acted, the tool, risk,
// outcome and task, and the detail collapsed. Every field is shown as text;
// the detail of a memory change is a line diff, classed by the first
// character of each line (which stays visible, so colour is never the only
// signal).
const props = defineProps<{ event: AuditEvent }>()
const { $gettext } = useGettext()

const label = computed(() => kindLabel(String(props.event.kind ?? ''), $gettext))
const actor = computed(() => credLabel(String(props.event.cred ?? ''), $gettext))

const edit = computed(() => (props.event.kind === 'memory_edited' ? memoryEdit(String(props.event.detail ?? '')) : null))
const file = computed(() => edit.value?.name ?? null)
const diff = computed<DiffLine[] | null>(() => {
  if (props.event.kind === 'memory_changed') return diffLines(String(props.event.detail ?? ''))
  if (edit.value !== null) return diffLines(edit.value.diff)
  return null
})
</script>

<style>
.agent-event {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
  padding: 0.5rem 0.75rem;
  border: 1px solid var(--oc-color-border, #ddd);
  border-radius: 6px;
  min-width: 0;
  overflow-wrap: anywhere;
}
.agent-event-head,
.agent-event-meta {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0.25rem 0.75rem;
  min-width: 0;
}
.agent-event-kind {
  font-weight: 600;
  display: inline-flex;
  flex-wrap: wrap;
  gap: 0.25rem;
  min-width: 0;
}
.agent-event-time,
.agent-event-meta {
  font-size: 0.875rem;
}
.agent-event-by,
.agent-event-outcome {
  display: inline-flex;
  gap: 0.25rem;
  min-width: 0;
}
.agent-event-tool {
  font-family: var(--oc-font-family-mono, monospace);
}
.agent-event-detail summary {
  cursor: pointer;
}
.agent-event-detail > .agent-text-mono,
.agent-diff {
  max-height: 24rem;
  overflow: auto;
}
.agent-event-file-line {
  display: flex;
  gap: 0.25rem;
  margin: 0.25rem 0;
}
.agent-diff-line {
  padding: 0 0.25rem;
}
.agent-diff-add {
  background: var(--oc-color-swatch-success-muted, #e6f4ea);
}
.agent-diff-del {
  background: var(--oc-color-swatch-danger-muted, #fdecea);
}
.agent-diff-note {
  font-style: italic;
}
</style>
