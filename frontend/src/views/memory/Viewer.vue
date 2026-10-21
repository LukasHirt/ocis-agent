<template>
  <article class="agent-memory-viewer">
    <router-link class="agent-memory-back" :to="listRoute()">
      <oc-icon name="arrow-left" size="small" />
      <span v-text="$gettext('All memory files')" />
    </router-link>

    <div class="agent-memory-viewer-head">
      <h2 v-if="validName" class="agent-memory-viewer-name"><TextBlock :text="validName" /></h2>
      <div v-if="!mem.editing" class="agent-memory-actions">
        <oc-button v-if="mem.canEdit" size="small" @click="mem.startEdit()">
          <oc-icon name="edit" size="small" />
          <span v-text="$gettext('Edit')" />
        </oc-button>
        <oc-button size="small" :disabled="mem.fileLoading" @click="mem.reload()">
          <oc-icon name="refresh" size="small" />
          <span v-text="$gettext('Reload')" />
        </oc-button>
      </div>
    </div>

    <LoadingState
      v-if="mem.fileLoading && !mem.file"
      variant="rows"
      :count="3"
      :label="$gettext('Loading the file')"
    />
    <ErrorState
      v-if="mem.fileError && (!mem.file || !agent.serviceDown)"
      class="agent-memory-error"
      :title="mem.fileError"
      :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
      :compact="!!mem.file"
      :busy="mem.fileLoading"
      @retry="retry"
    />
    <EmptyState
      v-if="mem.fileMissing"
      class="agent-memory-missing"
      icon="file-text"
      :title="$gettext('This file does not exist (any more).')"
    />

    <template v-if="mem.file">
      <Editor v-if="mem.editing" />
      <template v-else>
        <p v-if="mem.file.truncated" class="agent-memory-banner" role="status">
          <oc-icon name="information" size="small" />
          <span v-text="$gettext('Showing the first part; this file cannot be edited here.')" />
        </p>
        <p v-else-if="!mem.file.editable" class="agent-memory-banner" role="status">
          <oc-icon name="information" size="small" />
          <span v-text="$gettext('This file cannot be edited here.')" />
        </p>
        <TextBlock class="agent-memory-content" :text="mem.file.content" mono />
        <p v-if="mem.file.content === ''" class="agent-memory-empty" v-text="$gettext('This file is empty.')" />
      </template>
    </template>
  </article>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { isMemoryName } from '../../util/memoryText'
import TextBlock from '../../components/TextBlock.vue'
import Editor from './Editor.vue'
import EmptyState from '../../components/state/EmptyState.vue'
import ErrorState from '../../components/state/ErrorState.vue'
import LoadingState from '../../components/state/LoadingState.vue'
import { useMemoryStore } from '../../stores/memory'
import { useAgentStore } from '../../stores/agent'
import { listRoute } from './route'

// The open memory file as plain text (untrusted: control characters shown
// as codes, nothing interpreted), or its editor.
const mem = useMemoryStore()
const agent = useAgentStore()
useGettext()

const retry = () => {
  void mem.reload()
  if (agent.serviceDown) void agent.retryNow()
}

// the name from the route, shown only when the server would accept it
const validName = computed(() => (mem.openName !== null && isMemoryName(mem.openName) ? mem.openName : null))
</script>

<style>
.agent-memory-viewer {
  display: flex;
  flex-direction: column;
  gap: 0.75rem;
  min-width: 0;
}
.agent-memory-back {
  display: inline-flex;
  align-items: center;
  gap: 0.25rem;
  align-self: flex-start;
}
.agent-memory-viewer-head {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: space-between;
  gap: 0.5rem;
}
.agent-memory-viewer-name {
  margin: 0;
  font-family: var(--oc-font-family-mono, monospace);
  overflow-wrap: anywhere;
}
.agent-memory-banner {
  display: flex;
  align-items: center;
  gap: 0.5rem;
  margin: 0;
  padding: 0.5rem 0.75rem;
  border-left: 4px solid var(--oc-color-swatch-warning-default, #b8860b);
  background: var(--oc-color-background-muted, #f5f5f5);
}
.agent-memory-content {
  padding: 0.75rem;
  border: 1px solid var(--oc-color-border, #ddd);
  border-radius: 4px;
  max-width: 100%;
  overflow-x: auto;
}
</style>
