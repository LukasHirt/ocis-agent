<template>
  <div class="agent-memory-files">
    <section data-testid="agent-memory-core">
      <h2 class="agent-memory-group" v-text="$gettext('Profile and projects')" />
      <ul class="agent-memory-list">
        <li v-for="f in mem.coreFiles" :key="f.name" :class="['agent-memory-file', 'agent-row', { 'agent-memory-file-current': mem.openName === f.name }]">
          <oc-icon name="file-text" size="small" />
          <div class="agent-memory-file-body">
            <router-link
              class="agent-memory-file-link agent-row-link"
              :to="fileRoute(f.name)"
              :aria-current="mem.openName === f.name ? 'page' : undefined"
            >
              <span class="agent-memory-file-name" v-text="f.name" />
            </router-link>
            <span class="agent-memory-file-meta agent-meta">
              <span v-text="formatBytes(f.size)" />
              <time v-if="f.modified" :datetime="f.modified" v-text="formatTime(f.modified)" />
              <span
                v-if="f.size > MAX_FILE_BYTES"
                class="agent-memory-readonly"
                v-text="$gettext('Read only: too large to edit here')"
              />
            </span>
          </div>
        </li>
      </ul>
      <ul v-if="mem.listLoaded && mem.missingCore.length" class="agent-memory-list" data-testid="agent-memory-missing">
        <li v-for="name in mem.missingCore" :key="name" class="agent-memory-missing-file agent-row">
          <oc-icon name="file-text" size="small" />
          <div class="agent-memory-file-body">
            <span class="agent-memory-file-missing-name" v-text="name" />
            <span class="agent-memory-file-meta agent-meta" v-text="$gettext('Not created yet')" />
          </div>
          <oc-button class="agent-row-aside" size="small" appearance="outline" :disabled="mem.creating || mem.dirty" :title="mem.dirty ? $gettext('Save or discard your changes first.') : undefined" @click="createCore(name)">
            <oc-icon name="add" size="small" />
            <span v-text="$gettext('Create')" />
          </oc-button>
        </li>
      </ul>
      <p v-if="coreError" role="alert" class="agent-memory-error agent-notice agent-notice--danger" v-text="coreError" />
    </section>

    <section data-testid="agent-memory-notes">
      <h2 class="agent-memory-group" v-text="$gettext('Notes')" />
      <ul class="agent-memory-list">
        <li v-for="f in mem.notes" :key="f.name" :class="['agent-memory-file', 'agent-row', { 'agent-memory-file-current': mem.openName === f.name }]">
          <oc-icon name="file-text" size="small" />
          <div class="agent-memory-file-body">
            <router-link
              class="agent-memory-file-link agent-row-link"
              :to="fileRoute(f.name)"
              :aria-current="mem.openName === f.name ? 'page' : undefined"
            >
              <span class="agent-memory-file-name" v-text="f.name" />
            </router-link>
            <span class="agent-memory-file-meta agent-meta">
              <span v-text="formatBytes(f.size)" />
              <time v-if="f.modified" :datetime="f.modified" v-text="formatTime(f.modified)" />
              <span
                v-if="f.size > MAX_FILE_BYTES"
                class="agent-memory-readonly"
                v-text="$gettext('Read only: too large to edit here')"
              />
            </span>
          </div>
        </li>
      </ul>
      <EmptyState
        v-if="mem.listLoaded && mem.notes.length === 0"
        class="agent-memory-empty"
        icon="file-text"
        :title="$gettext('No notes yet.')"
      />
    </section>
  </div>
</template>

<script setup lang="ts">
import { getCurrentInstance, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import EmptyState from '../../components/state/EmptyState.vue'
import { useMemoryStore } from '../../stores/memory'
import { formatBytes, formatTime } from '../../util/format'
import { MAX_FILE_BYTES } from '../../util/memoryText'
import { fileRoute } from './route'

// The memory files: profile.md and projects.md first (with a button for
// one that does not exist yet), then the notes. The list has no delete:
// files are deleted in the Files app.
const mem = useMemoryStore()
useGettext()
const coreError = ref<string | null>(null)

const instance = getCurrentInstance()

const createCore = async (name: string) => {
  coreError.value = null
  if (await mem.create(name, '')) {
    void instance?.proxy?.$router?.push(fileRoute(name))
  } else {
    coreError.value = mem.createError
    mem.clearCreateError()
  }
}

</script>

<style>
.agent-memory-files {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
}
.agent-memory-group {
  margin: 0 0 var(--agent-space-1);
  font-size: var(--agent-font-xs);
  font-weight: 600;
  color: var(--oc-color-text-muted);
}
.agent-memory-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.agent-memory-file,
.agent-memory-missing-file {
  display: grid;
  grid-template-columns: 1.25rem minmax(0, 1fr);
  align-items: start;
  gap: var(--agent-space-2);
  padding: var(--agent-space-2);
  border-radius: var(--agent-radius-m);
}
.agent-memory-missing-file {
  grid-template-columns: 1.25rem minmax(0, 1fr) auto;
  align-items: center;
}
.agent-memory-missing-file .agent-memory-file-missing-name {
  color: var(--oc-color-text-muted);
}
.agent-memory-file-current {
  background: var(--oc-color-background-highlight);
}
.agent-memory-file-body {
  display: flex;
  flex-direction: column;
  min-width: 0;
}
.agent-memory-file-link {
  overflow-wrap: anywhere;
  font-family: var(--agent-font-mono);
}
.agent-memory-file-missing-name {
  font-family: var(--agent-font-mono);
  overflow-wrap: anywhere;
}
.agent-memory-file-meta {
  display: flex;
  flex-wrap: wrap;
  gap: var(--agent-space-1) var(--agent-space-2);
}
.agent-memory-error {
  color: var(--oc-color-swatch-danger-default);
}
</style>
