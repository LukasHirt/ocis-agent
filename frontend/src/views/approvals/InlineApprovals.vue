<template>
  <div class="agent-inline-approvals">
    <p v-if="error" role="alert" v-text="error" />
    <BulkDecide v-if="live.length > 1" :live="live" @decided="markAll" @attempted="emit('settled')" />
    <template v-for="a in items" :key="a.id">
      <p v-if="decidedHere.has(a.id)" class="agent-inline-decided">
        <oc-icon :name="decidedHere.get(a.id) === 'approve' ? 'checkbox-circle' : 'close-circle'" size="small" />
        <span
          class="agent-inline-decided-label"
          v-text="decidedHere.get(a.id) === 'approve' ? $gettext('Approved:') : $gettext('Rejected:')"
        />
        <TextBlock :text="a.tool" wrap="normal" class="agent-mono" />
        <TextBlock v-if="firstTarget(a)" :text="quote(firstTarget(a)!)" wrap="normal" class="agent-mono" />
      </p>
      <ApprovalCard v-else-if="isLive(a)" :approval="a" compact @decided="(d) => decidedHere.set(a.id, d)" @attempted="emit('settled')" />
    </template>
    <p v-if="more" v-text="$gettext('More actions are waiting; see them all in the inbox.')" />
    <router-link
      class="agent-inline-inbox"
      :to="{ name: `${APPID}-approvals`, query: { job: jobId } }"
      v-text="$gettext('Open in approvals')"
    />
  </div>
</template>

<script setup lang="ts">
import { computed, reactive, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import ApprovalCard from './ApprovalCard.vue'
import BulkDecide from './BulkDecide.vue'
import { APPID } from '../../appid'
import { quote, targetsOf } from './targets'
import type { Approval, ApprovalState, Decision } from '../../api/types'

// InlineApprovals decides the pending approvals of one job where the job is
// shown (a chat reply, the task view). Cards and buttons are the inbox's
// (all sending goes through the shared decisions); a decided item shrinks to
// one line until the reread list no longer holds it. settled is emitted
// after every finished attempt, successful or not, so the parent rereads its list.
const props = defineProps<{ jobId: string; items: Approval[]; more: boolean; error: string | null }>()
const emit = defineEmits<{ settled: [] }>()
useGettext()

// the approvals decided here, by id (what the user chose)
const decidedHere = reactive(new Map<string, Decision>())

const isLive = (a: Approval) => a.state === ('pending' satisfies ApprovalState) && !a.job_ended
const live = computed(() => props.items.filter((a) => isLive(a) && !decidedHere.has(a.id)))

const firstTarget = (a: Approval): string | undefined => targetsOf(a.preview, a.args)[0]

const markAll = (d: Decision, sent: Approval[]) => {
  for (const a of sent) decidedHere.set(a.id, d)
}

// a mark ends when the list no longer holds the item
watch(
  () => props.items,
  (items) => {
    const ids = new Set(items.map((a) => a.id))
    for (const id of [...decidedHere.keys()]) if (!ids.has(id)) decidedHere.delete(id)
  }
)
</script>

<style>
.agent-inline-approvals {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  margin-top: var(--agent-space-2);
}
.agent-inline-approvals > p {
  margin: 0;
}
.agent-inline-decided {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-1) var(--agent-space-2);
  color: var(--oc-color-text-muted);
  font-size: var(--agent-font-s);
}
.agent-inline-decided svg {
  fill: currentColor;
}
.agent-inline-inbox {
  align-self: flex-start;
  font-size: var(--agent-font-s);
}
</style>
