<template>
  <section class="agent-group agent-surface agent-surface--muted" :aria-labelledby="headingId">
    <header class="agent-group-head">
      <div class="agent-group-main">
        <h3 :id="headingId" class="agent-group-title">
          <span v-if="goal" class="agent-group-goal">
            <span class="agent-label" v-text="$gettext('Task')" />
            <TextBlock :text="goal" :max-lines="4" />
          </span>
          <span v-else v-text="fallbackTitle" />
        </h3>
        <p class="agent-group-chips">
          <span v-if="trigger" class="agent-group-trigger agent-chip agent-chip--neutral">
            <span v-if="triggerText" v-text="triggerText" />
            <TextBlock v-else :text="trigger" class="agent-group-raw" />
          </span>
          <span class="agent-group-count agent-chip agent-chip--neutral" v-text="countText" />
        </p>
      </div>
      <BulkDecide :live="live" :goal="goal" />
    </header>
    <p
      v-if="proactive"
      class="agent-group-proposal agent-notice agent-notice--info"
      v-text="
        $gettext(
          'The agent proposes these actions on its own. Nothing has been done yet; each one runs only if you approve it.'
        )
      "
    />
    <ApprovalCard v-for="a in items" :key="a.id" :approval="a" />
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import ApprovalCard from './ApprovalCard.vue'
import BulkDecide from './BulkDecide.vue'
import { shortId } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import { isProactiveTrigger, triggerLabel } from '../tasks/labels'
import type { Approval, ApprovalState, TriggerKind } from '../../api/types'

// JobGroup shows the pending approvals of one job with the job's goal
// (untrusted text; absent when it could not be read) and a bulk decision
// over the approvals that can still be decided, all of the same job.
// trigger is the job's trigger kind (absent while unknown): a proactive
// job is labelled as such and its approvals as proposals; an unknown kind
// other than chat is shown as text.
const props = defineProps<{ jobId: string; items: Approval[]; goal?: string | null; trigger?: string | null }>()
const { $gettext, $ngettext, interpolate } = useGettext()

const trigger = computed(() =>
  typeof props.trigger === 'string' && props.trigger !== '' && props.trigger !== ('chat' satisfies TriggerKind)
    ? props.trigger
    : null
)
const triggerText = computed(() => (trigger.value ? triggerLabel(trigger.value, $gettext) : null))
const proactive = computed(() => trigger.value !== null && isProactiveTrigger(trigger.value))

const headingId = computed(() => `agent-group-${props.jobId.replace(/[^A-Za-z0-9_-]/g, '_')}`)
const live = computed(() => props.items.filter((a) => a.state === ('pending' satisfies ApprovalState) && !a.job_ended))
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
.agent-group-head {
  display: grid;
  grid-template-columns: minmax(0, 1fr) auto;
  gap: var(--agent-space-2) var(--agent-space-4);
  align-items: start;
}
.agent-group-main {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  min-width: 0;
}
.agent-group-title {
  margin: 0;
  font-size: var(--agent-font-m);
  font-weight: 600;
  min-width: 0;
}
.agent-group-chips {
  margin: 0;
  display: flex;
  flex-wrap: wrap;
  gap: var(--agent-space-2);
}
.agent-group-raw {
  font-family: var(--agent-font-mono);
}
.agent-group-proposal {
  margin: 0;
}
@container agent (max-width: 699px) {
  .agent-group-head {
    grid-template-columns: minmax(0, 1fr);
  }
}
</style>
