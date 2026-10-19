<template>
  <section class="agent-memory oc-p-m">
    <h1 v-text="$gettext('Memory')" />
    <p
      class="agent-memory-intro"
      v-text="
        $gettext(
          'Notes the agent keeps about you and your projects. You can read and edit them; the agent treats them as notes, never as instructions.'
        )
      "
    />
    <p class="agent-memory-hint">
      <oc-icon name="information" size="small" />
      <span v-text="$gettext('Changes you make are recorded in Activity.')" />
    </p>

    <Viewer v-if="name" />

    <template v-else>
      <p class="agent-memory-hint">
        <span v-text="$gettext('To delete a file, use the Files app: the files are in this folder of your personal space:')" />
        <code v-text="'.ocis-agent/memory'" />
      </p>
      <div class="agent-memory-tools">
        <oc-button v-if="!creating" size="small" @click="creating = true">
          <oc-icon name="add" size="small" />
          <span v-text="$gettext('New note')" />
        </oc-button>
        <oc-button size="small" :disabled="mem.listLoading" @click="mem.refreshList()">
          <oc-icon name="refresh" size="small" />
          <span v-text="$gettext('Refresh')" />
        </oc-button>
      </div>
      <NewFile v-if="creating" @created="opened" @cancel="creating = false" />

      <div v-if="mem.listError" class="agent-memory-error">
        <p role="alert" v-text="mem.listError" />
        <oc-button size="small" :disabled="mem.listLoading" @click="mem.refreshList()" v-text="$gettext('Try again')" />
      </div>
      <oc-spinner v-if="!mem.listLoaded && !mem.listError" :aria-label="$gettext('Loading the memory files')" />
      <FileList v-if="mem.listLoaded" />
    </template>
  </section>
</template>

<script setup lang="ts">
import { getCurrentInstance, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import FileList from './memory/FileList.vue'
import NewFile from './memory/NewFile.vue'
import Viewer from './memory/Viewer.vue'
import { useMemoryStore } from '../stores/memory'
import { fileRoute } from './memory/route'

// The memory screen: the memory files, or the route's file with its
// editor. There is no delete (the API has none). It listens for changes
// only while shown.
const props = defineProps<{ name?: string }>()
const mem = useMemoryStore()
useGettext()

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
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-memory h1 {
  margin: 0;
}
.agent-memory-intro,
.agent-memory-hint {
  margin: 0;
}
.agent-memory-hint {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.25rem;
  color: var(--oc-color-text-muted, #666);
}
.agent-memory-tools {
  display: flex;
  flex-wrap: wrap;
  gap: 0.5rem;
}
</style>
