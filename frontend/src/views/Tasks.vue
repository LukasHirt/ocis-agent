<template>
  <section class="agent-tasks oc-p-m">
    <h1 v-text="$gettext('Tasks')" />

    <JobDetail v-if="id" />

    <template v-else>
      <div class="agent-tasks-tools">
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
        <oc-button size="small" :disabled="jobs.loading" @click="jobs.refresh()">
          <oc-icon name="refresh" size="small" />
          <span v-text="$gettext('Refresh')" />
        </oc-button>
      </div>

      <div v-if="jobs.error" class="agent-tasks-error">
        <p role="alert" v-text="jobs.error" />
        <oc-button size="small" :disabled="jobs.loading" @click="jobs.refresh()" v-text="$gettext('Try again')" />
      </div>
      <oc-spinner v-if="!jobs.loaded && !jobs.error" :aria-label="$gettext('Loading the tasks')" />

      <template v-if="jobs.loaded">
        <ul class="agent-tasks-list">
          <li v-for="j in jobs.jobs" :key="j.id"><JobRow :job="j" /></li>
        </ul>
        <p
          v-if="jobs.jobs.length === 0 && !jobs.cursor"
          class="agent-tasks-empty"
          v-text="jobs.filter === '' ? $gettext('No tasks yet.') : $gettext('No tasks in this state.')"
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
  </section>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import JobRow from './tasks/JobRow.vue'
import JobDetail from './tasks/JobDetail.vue'
import { JOB_FILTERS, useJobsStore } from '../stores/jobs'
import { filterLabel } from './tasks/labels'

// The tasks screen: the jobs newest first with a state filter, or the
// route's job with its steps. It listens for changes only while shown.
const props = defineProps<{ id?: string }>()
const jobs = useJobsStore()
const { $gettext } = useGettext()

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
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-tasks h1 {
  margin: 0;
}
.agent-tasks-tools {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: 0.75rem;
}
.agent-tasks-filter {
  min-width: 14rem;
}
.agent-tasks-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  min-width: 0;
}
.agent-tasks-error {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem;
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
