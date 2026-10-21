<template>
  <article class="agent-event agent-row">
    <span :class="['agent-event-icon', tone === 'danger' && 'agent-event-icon--danger']" aria-hidden="true">
      <oc-icon :name="icon" size="small" />
    </span>
    <div class="agent-event-head">
      <span class="agent-event-kind">
        <span v-if="label" v-text="label" />
        <template v-else>
          <span v-text="$gettext('Unknown event:')" />
          <TextBlock :text="event.kind" class="agent-event-raw" />
        </template>
      </span>
    </div>
    <time class="agent-event-time agent-meta" :datetime="event.ts" v-text="formatTime(event.ts)" />
    <div class="agent-event-meta agent-meta">
      <span v-if="event.cred" class="agent-event-by">
        <span v-text="$gettext('By')" />
        <span class="agent-event-actor">
          <span v-if="actor" v-text="actor" />
          <TextBlock v-else :text="event.cred" />
        </span>
      </span>
      <TextBlock v-if="event.tool" class="agent-event-tool agent-chip agent-chip--neutral agent-mono" :text="event.tool" />
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
        <div class="agent-diff agent-code">
          <div v-for="(line, i) in diff" :key="i" :class="['agent-diff-line', `agent-diff-${line.kind}`]">
            <TextBlock :text="line.text" mono />
          </div>
        </div>
      </template>
      <TextBlock v-else class="agent-code" :text="event.detail" mono />
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
import { credLabel, diffLines, kindIcon, kindLabel, kindTone, memoryEdit, type DiffLine } from './kinds'
import type { AuditEvent } from '../../api/types'

// EventRow is one audit event: its kind, time, who acted, the tool, risk,
// outcome and task, and the detail collapsed. Every field is shown as text;
// the detail of a memory change is a line diff, classed by the first
// character of each line (which stays visible, so colour is never the only
// signal).
const props = defineProps<{ event: AuditEvent }>()
const { $gettext } = useGettext()

const label = computed(() => kindLabel(String(props.event.kind ?? ''), $gettext))
const icon = computed(() => kindIcon(String(props.event.kind ?? '')))
const tone = computed(() => kindTone(String(props.event.kind ?? '')))
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
.agent-event.agent-row {
  display: grid;
  grid-template-columns: 2rem minmax(0, 1fr) auto;
  grid-template-areas:
    'icon head time'
    'icon meta meta'
    'icon detail detail';
  gap: var(--agent-space-1) var(--agent-space-3);
  align-items: start;
  min-width: 0;
  overflow-wrap: anywhere;
}
.agent-event-icon {
  grid-area: icon;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 2rem;
  height: 2rem;
  border-radius: 50%;
  background: var(--oc-color-background-muted);
  color: var(--oc-color-swatch-passive-default);
}
.agent-event-icon--danger {
  color: var(--oc-color-swatch-danger-default);
}
.agent-event-icon .oc-icon svg {
  fill: currentColor;
}
.agent-event-head {
  grid-area: head;
  align-self: center;
  min-width: 0;
}
.agent-event-kind {
  font-weight: 600;
  display: inline-flex;
  flex-wrap: wrap;
  gap: var(--agent-space-1);
  min-width: 0;
}
.agent-event-time {
  grid-area: time;
  align-self: center;
  white-space: nowrap;
}
.agent-event-meta {
  grid-area: meta;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-1) var(--agent-space-3);
  min-width: 0;
}
.agent-event-by,
.agent-event-outcome {
  display: inline-flex;
  gap: var(--agent-space-1);
  min-width: 0;
}
.agent-event-detail {
  grid-area: detail;
  min-width: 0;
}
.agent-event-detail summary {
  cursor: pointer;
  font-size: var(--agent-font-xs);
  color: var(--oc-color-swatch-primary-default);
}
.agent-event-detail[open] summary {
  margin-bottom: var(--agent-space-2);
}
.agent-event-detail > .agent-text-mono,
.agent-diff {
  max-height: 24rem;
  overflow: auto;
}
.agent-event-file-line {
  display: flex;
  gap: var(--agent-space-1);
  margin: var(--agent-space-1) 0;
  font-size: var(--agent-font-xs);
}
.agent-diff-line {
  padding: 0 var(--agent-space-1);
}
.agent-diff-add {
  background: color-mix(in srgb, var(--oc-color-swatch-success-default) 12%, var(--oc-color-background-default));
}
.agent-diff-del {
  background: color-mix(in srgb, var(--oc-color-swatch-danger-default) 12%, var(--oc-color-background-default));
}
.agent-diff-note {
  font-style: italic;
}
</style>
