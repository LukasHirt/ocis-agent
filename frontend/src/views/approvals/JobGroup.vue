<template>
  <section class="agent-group" :aria-labelledby="headingId">
    <header class="agent-group-head">
      <h3 :id="headingId" class="agent-group-title">
        <span v-if="goal" class="agent-group-goal">
          <span class="agent-label" v-text="$gettext('Task')" />
          <TextBlock :text="goal" :max-lines="4" />
        </span>
        <span v-else v-text="fallbackTitle" />
      </h3>
      <span class="agent-group-count" v-text="countText" />
    </header>
    <div v-if="live.length > 1" class="agent-group-bulk">
      <oc-button
        appearance="outline"
        :disabled="bulkDisabled"
        @click="decisions.decideAll(live, 'approve')"
        v-text="bulkLabel($gettext('Approve all %{count}'))"
      />
      <oc-button
        appearance="outline"
        :disabled="bulkDisabled"
        @click="decisions.decideAll(live, 'reject')"
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
    <ApprovalCard v-for="a in items" :key="a.id" :approval="a" />
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import ApprovalCard from './ApprovalCard.vue'
import { MAX_BULK, useDecisions } from './decisions'
import { shortId } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import type { Approval } from '../../api/types'

// JobGroup shows the pending approvals of one job with the job's goal
// (untrusted text; absent when it could not be read) and a bulk decision
// over the approvals that can still be decided, all of the same job.
const props = defineProps<{ jobId: string; items: Approval[]; goal?: string | null }>()
const { $gettext, $ngettext, interpolate } = useGettext()
const decisions = useDecisions()

const headingId = computed(() => `agent-group-${props.jobId.replace(/[^A-Za-z0-9_-]/g, '_')}`)
const live = computed(() => props.items.filter((a) => a.state === 'pending' && !a.job_ended))
const tooMany = computed(() => live.value.length > MAX_BULK)
const bulkDisabled = computed(() => tooMany.value || live.value.some((a) => decisions.busy(a.id)))

const bulkLabel = (msgid: string) => interpolate(msgid, { count: live.value.length }, true)

const fallbackTitle = computed(() =>
  interpolate($gettext('Task %{id}'), { id: visibleText(shortId(props.jobId)).text }, true)
)
const countText = computed(() =>
  interpolate(
    $ngettext('%{count} approval', '%{count} approvals', props.items.length),
    { count: props.items.length },
    true
  )
)
</script>

<style>
.agent-group {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  margin-bottom: 1.5rem;
}
.agent-group-head {
  display: flex;
  flex-wrap: wrap;
  align-items: baseline;
  gap: 0.5rem;
}
.agent-group-title {
  margin: 0;
  font-size: 1.1em;
}
.agent-group-count {
  color: var(--oc-color-text-muted, #666);
}
.agent-group-bulk {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
</style>
