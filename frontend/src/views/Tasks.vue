<template>
  <section class="agent-tasks agent-page">
    <header class="agent-page-head agent-page-head--sticky">
      <div class="agent-page-titles">
        <h1 class="agent-page-title" v-text="$gettext('Tasks')" />
      </div>
      <div v-if="!id" class="agent-page-actions">
        <oc-button size="small" appearance="outline" :disabled="jobs.loading" @click="jobs.refresh()">
          <oc-icon name="refresh" size="small" />
          <span v-text="$gettext('Refresh')" />
        </oc-button>
        <oc-button size="small" appearance="outline" :disabled="!canReview" @click="reviewNow">
          <oc-icon name="search-eye" size="small" />
          <span v-text="$gettext('Review now')" />
        </oc-button>
      </div>
    </header>
    <div class="agent-page-body">
    <JobDetail v-if="id" />

    <template v-else>
      <div class="agent-toolbar agent-tasks-tools">
        <oc-select
          class="agent-tasks-filter"
          :model-value="selected"
          :options="options"
          option-label="label"
          :clearable="false"
          :searchable="false"
          data-testid="agent-tasks-filter"
          :label="$gettext('Show')"
          @update:model-value="(o: FilterOption | null) => o && jobs.setFilter(o.value)"
        />
      </div>
      <p
        v-if="reviewResult"
        class="agent-tasks-review-result agent-notice agent-notice--info"
        role="status"
        data-testid="agent-review-result"
        v-text="reviewResult"
      />
      <p v-if="reviewError" class="agent-tasks-review-error agent-notice agent-notice--danger" role="alert" data-testid="agent-review-error">
        <span v-text="reviewError.message" />
        <span v-if="reviewError.detail" v-text="' ' + reviewError.detail" />
      </p>

      <ErrorState
        v-if="jobs.error && (!jobs.loaded || !agent.serviceDown)"
        class="agent-tasks-error"
        :title="jobs.error"
        :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
        :compact="jobs.loaded"
        :busy="jobs.loading"
        @retry="retry"
      />
      <LoadingState
        v-if="!jobs.loaded && !jobs.error"
        variant="rows"
        :count="5"
        :label="$gettext('Loading the tasks')"
      />

      <template v-if="jobs.loaded">
        <ul class="agent-tasks-list agent-rows">
          <li v-for="j in jobs.jobs" :key="j.id"><JobRow :job="j" /></li>
        </ul>
        <EmptyState
          v-if="jobs.jobs.length === 0 && !jobs.cursor"
          class="agent-tasks-empty"
          icon="task"
          :title="jobs.filter === '' ? $gettext('No tasks yet.') : $gettext('No tasks in this state.')"
        />
        <oc-spinner v-if="jobs.moreLoading" size="small" :aria-label="$gettext('Loading more tasks')" />
        <oc-button
          v-else-if="jobs.cursor"
          appearance="outline"
          size="small"
          @click="jobs.loadMore()"
          v-text="$gettext('Load more')"
        />
      </template>
    </template>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import JobRow from './tasks/JobRow.vue'
import JobDetail from './tasks/JobDetail.vue'
import EmptyState from '../components/state/EmptyState.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import { JOB_FILTERS, useJobsStore } from '../stores/jobs'
import { useAgentStore } from '../stores/agent'
import { ApiError, describeError } from '../api/errors'
import { filterLabel } from './tasks/labels'

// The tasks screen: the jobs newest first with a state filter, or the
// route's job with its steps. It listens for changes only while shown.
const props = defineProps<{ id?: string }>()
const jobs = useJobsStore()
const agent = useAgentStore()
const { $gettext, interpolate } = useGettext()

const retry = () => {
  void jobs.refresh()
  if (agent.serviceDown) void agent.retryNow()
}

// Review now: start a review (POST /proactive/run). The result and any
// refusal are fixed texts; the server's own text is never shown.
const reviewing = ref(false)
const reviewResult = ref<string | null>(null)
const reviewError = ref<{ message: string; detail?: string } | null>(null)
const canReview = computed(() => !reviewing.value && agent.status?.enrolled === true)

const reviewNow = async () => {
  if (!canReview.value) return
  reviewing.value = true
  reviewResult.value = null
  reviewError.value = null
  try {
    await agent.runProactive()
    reviewResult.value = $gettext(
      'A review has started. The agent only proposes actions: nothing changes until you approve it in the approvals inbox.'
    )
    void jobs.refresh()
  } catch (e) {
    const detail =
      e instanceof ApiError && e.code === 'rate_limited' && e.retryAfter !== undefined
        ? interpolate($gettext('Try again in %{seconds} seconds.'), { seconds: e.retryAfter }, true)
        : undefined
    reviewError.value = { message: describeError(e, $gettext), ...(detail ? { detail } : {}) }
  } finally {
    reviewing.value = false
  }
}

interface FilterOption {
  value: string
  label: string
}

const options = computed<FilterOption[]>(() => JOB_FILTERS.map((value) => ({ value, label: filterLabel(value, $gettext) })))
const selected = computed(() => options.value.find((o) => o.value === jobs.filter) ?? options.value[0])

let detach: (() => void) | null = null
onMounted(() => {
  detach = jobs.attach()
})
onBeforeUnmount(() => {
  detach?.()
  detach = null
})

watch(
  () => props.id,
  (id) => {
    if (id) {
      jobs.syncSession() // a new session forgot the job shown before
      if (id === jobs.detailId && jobs.detailLoaded) void jobs.refreshDetail()
      else void jobs.openJob(id)
    } else {
      jobs.closeJob()
      void jobs.refresh()
    }
  },
  { immediate: true }
)
</script>

<style>
.agent-tasks {
  min-width: 0;
}
.agent-tasks-filter {
  min-width: 14rem;
}
.agent-tasks-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
}
</style>
