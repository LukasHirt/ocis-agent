<template>
  <div v-if="live.length > 1" class="agent-group-bulk">
    <oc-button
      appearance="outline"
      size="small"
      :disabled="bulkDisabled"
      :aria-label="bulkAria('approve')"
      @click="run('approve')"
      v-text="bulkLabel($gettext('Approve all %{count}'))"
    />
    <oc-button
      appearance="outline"
      size="small"
      :disabled="bulkDisabled"
      :aria-label="bulkAria('reject')"
      @click="run('reject')"
      v-text="bulkLabel($gettext('Reject all %{count}'))"
    />
    <span
      v-if="tooMany"
      class="agent-group-limit"
      v-text="
        interpolate(
          $gettext('More than %{max} actions cannot be decided together; decide them one by one.'),
          { max: MAX_BULK }
        )
      "
    />
  </div>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { MAX_BULK, useDecisions } from './decisions'
import { shortId } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import { quote } from './targets'
import type { Approval, Decision } from '../../api/types'

// BulkDecide approves or rejects all the given pending approvals of one job
// at once (through the shared decisions, with their confirmation). goal is
// the job's goal (untrusted), used only in the buttons' accessible names.
const props = defineProps<{ live: Approval[]; goal?: string | null }>()
const emit = defineEmits<{ decided: [Decision, Approval[]]; attempted: [] }>()
const { $gettext, interpolate } = useGettext()
const decisions = useDecisions()

const tooMany = computed(() => props.live.length > MAX_BULK)
const bulkDisabled = computed(() => tooMany.value || props.live.some((a) => decisions.busy(a.id)))

const bulkLabel = (msgid: string) => interpolate(msgid, { count: props.live.length }, true)

const run = async (d: Decision) => {
  const sent = props.live
  if (await decisions.decideAll(sent, d)) emit('decided', d, sent)
  emit('attempted')
}

// GOAL_LABEL_CHARS caps the goal in the bulk buttons' labels.
const GOAL_LABEL_CHARS = 80

// bulkAria names the task, so the bulk buttons of different groups differ.
const bulkAria = (d: Decision) => {
  const count = props.live.length
  if (props.goal) {
    const chars = Array.from(props.goal)
    const task = quote(chars.length > GOAL_LABEL_CHARS ? chars.slice(0, GOAL_LABEL_CHARS).join('') + '…' : props.goal)
    const msgid =
      d === 'approve'
        ? $gettext('Approve all %{count} actions of the task %{task}')
        : $gettext('Reject all %{count} actions of the task %{task}')
    return interpolate(msgid, { count, task }, true)
  }
  const jobId = props.live[0]?.job_id ?? ''
  const msgid =
    d === 'approve'
      ? $gettext('Approve all %{count} actions of task %{id}')
      : $gettext('Reject all %{count} actions of task %{id}')
  return interpolate(msgid, { count, id: visibleText(shortId(jobId)).text }, true)
}
</script>

<style>
.agent-group-bulk {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: flex-end;
  gap: var(--agent-space-2);
}
@container agent (max-width: 699px) {
  .agent-group-bulk {
    justify-content: flex-start;
  }
}
</style>
