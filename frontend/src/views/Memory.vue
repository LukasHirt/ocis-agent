<template>
  <section :class="['agent-memory', 'agent-page', 'agent-page--fill', { 'agent-memory--open': !!name || creating }]">
    <aside class="agent-memory-side">
      <div class="agent-memory-side-head">
        <h1 class="agent-page-title" v-text="$gettext('Memory')" />
        <div class="agent-memory-tools">
          <oc-button v-if="!name && !creating" size="small" @click="creating = true">
            <oc-icon name="add" size="small" />
            <span v-text="$gettext('New note')" />
          </oc-button>
          <oc-button size="small" :disabled="mem.listLoading" @click="mem.refreshList()">
            <oc-icon name="refresh" size="small" />
            <span v-text="$gettext('Refresh')" />
          </oc-button>
        </div>
      </div>
      <p
        class="agent-memory-intro agent-meta"
        v-text="
          $gettext(
            'Notes the agent keeps about you and your projects. You can read and edit them; the agent treats them as notes, never as instructions.'
          )
        "
      />

      <ErrorState
        v-if="mem.listError && (!mem.listLoaded || !agent.serviceDown)"
        class="agent-memory-error"
        :title="mem.listError"
        :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
        :compact="mem.listLoaded"
        :busy="mem.listLoading"
        @retry="retry"
      />
      <LoadingState
        v-if="!mem.listLoaded && !mem.listError"
        variant="rows"
        :count="3"
        :label="$gettext('Loading the memory files')"
      />
      <FileList v-if="mem.listLoaded" />

      <p class="agent-memory-hint agent-meta">
        <oc-icon name="information" size="small" />
        <span v-text="$gettext('Changes you make are recorded in Activity.')" />
      </p>
      <p class="agent-memory-hint agent-meta">
        <span v-text="$gettext('To delete a file, use the Files app: the files are in this folder of your personal space:')" />
        <code v-text="'.ocis-agent/memory'" />
      </p>
    </aside>

    <div class="agent-memory-detail">
      <Viewer v-if="name" />
      <NewFile v-else-if="creating" @created="opened" @cancel="creating = false" />
      <EmptyState v-else icon="file-text" :title="$gettext('Choose a file to read or edit it.')" />
    </div>
  </section>
</template>

<script setup lang="ts">
import { getCurrentInstance, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import FileList from './memory/FileList.vue'
import NewFile from './memory/NewFile.vue'
import Viewer from './memory/Viewer.vue'
import EmptyState from '../components/state/EmptyState.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import { useMemoryStore } from '../stores/memory'
import { useAgentStore } from '../stores/agent'
import { fileRoute } from './memory/route'

// The memory screen: the memory files, or the route's file with its
// editor. There is no delete (the API has none). It listens for changes
// only while shown.
const props = defineProps<{ name?: string }>()
const mem = useMemoryStore()
const agent = useAgentStore()
useGettext()

const retry = () => {
  void mem.refreshList()
  if (agent.serviceDown) void agent.retryNow()
}

const creating = ref(false)
const instance = getCurrentInstance()

const opened = (name: string) => {
  creating.value = false
  void instance?.proxy?.$router?.push(fileRoute(name))
}

let detach: (() => void) | null = null
onMounted(() => {
  detach = mem.attach()
})
onBeforeUnmount(() => {
  detach?.()
  detach = null
  // a draft left behind was confirmed to be discarded
  mem.close()
})

watch(
  () => props.name,
  (name) => {
    // the list is read in both cases: live changes compare the open
    // file's etag with it
    void mem.refreshList()
    if (name) {
      if (name !== mem.openName) void mem.open(name)
    } else {
      mem.close()
    }
  },
  { immediate: true }
)
</script>

<style>
.agent-memory {
  display: grid;
  grid-template-columns: var(--agent-pane) minmax(0, 1fr);
  grid-template-rows: minmax(0, 1fr);
  gap: 0;
  padding: 0;
}
@container agent (max-width: 999px) {
  .agent-memory {
    --agent-pane: 15rem;
  }
}
@container agent (max-width: 699px) {
  .agent-memory {
    grid-template-columns: minmax(0, 1fr);
  }
  .agent-memory--open .agent-memory-side {
    display: none;
  }
  .agent-memory:not(.agent-memory--open) .agent-memory-detail {
    display: none;
  }
}
.agent-memory-side {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-3);
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  padding: var(--agent-space-4);
  border-right: var(--agent-line);
  background: var(--oc-color-background-muted);
  box-sizing: border-box;
}
.agent-memory-side-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: var(--agent-space-2);
}
.agent-memory-side h1 {
  margin: 0;
}
.agent-memory-detail {
  min-width: 0;
  min-height: 0;
  overflow-y: auto;
  padding: var(--agent-space-4) var(--agent-space-5);
  box-sizing: border-box;
}
.agent-memory-intro,
.agent-memory-hint {
  margin: 0;
}
.agent-memory-hint {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-1);
}
.agent-memory-tools {
  display: flex;
  flex-wrap: wrap;
  gap: var(--agent-space-2);
}
</style>
