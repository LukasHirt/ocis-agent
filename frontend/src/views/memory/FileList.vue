<template>
  <div class="agent-memory-files">
    <section data-testid="agent-memory-core">
      <h2 class="agent-memory-group" v-text="$gettext('Profile and projects')" />
      <ul class="agent-memory-list">
        <li v-for="f in mem.coreFiles" :key="f.name" class="agent-memory-file">
          <router-link class="agent-memory-file-link" :to="fileRoute(f.name)">
            <span class="agent-memory-file-name" v-text="f.name" />
          </router-link>
          <span class="agent-memory-file-meta">
            <span v-text="formatBytes(f.size)" />
            <time v-if="f.modified" :datetime="f.modified" v-text="formatTime(f.modified)" />
            <span
              v-if="f.size > MAX_FILE_BYTES"
              class="agent-memory-readonly"
              v-text="$gettext('Read only: too large to edit here')"
            />
          </span>
        </li>
      </ul>
      <ul v-if="mem.listLoaded && mem.missingCore.length" class="agent-memory-list" data-testid="agent-memory-missing">
        <li v-for="name in mem.missingCore" :key="name" class="agent-memory-missing-file">
          <span class="agent-memory-file-missing-name" v-text="name" />
          <span class="agent-memory-file-meta" v-text="$gettext('Not created yet')" />
          <oc-button size="small" appearance="outline" :disabled="mem.creating" @click="createCore(name)">
            <oc-icon name="add" size="small" />
            <span v-text="$gettext('Create')" />
          </oc-button>
        </li>
      </ul>
      <p v-if="coreError" role="alert" class="agent-memory-error" v-text="coreError" />
    </section>

    <section data-testid="agent-memory-notes">
      <h2 class="agent-memory-group" v-text="$gettext('Notes')" />
      <ul class="agent-memory-list">
        <li v-for="f in mem.notes" :key="f.name" class="agent-memory-file">
          <router-link class="agent-memory-file-link" :to="fileRoute(f.name)">
            <span class="agent-memory-file-name" v-text="f.name" />
          </router-link>
          <span class="agent-memory-file-meta">
            <span v-text="formatBytes(f.size)" />
            <time v-if="f.modified" :datetime="f.modified" v-text="formatTime(f.modified)" />
            <span
              v-if="f.size > MAX_FILE_BYTES"
              class="agent-memory-readonly"
              v-text="$gettext('Read only: too large to edit here')"
            />
          </span>
        </li>
      </ul>
      <p
        v-if="mem.listLoaded && mem.notes.length === 0"
        class="agent-memory-empty"
        v-text="$gettext('No notes yet.')"
      />
    </section>
  </div>
</template>

<script setup lang="ts">
import { getCurrentInstance, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
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
  gap: 1rem;
}
.agent-memory-group {
  font-size: 1.1rem;
  margin: 0 0 0.5rem;
}
.agent-memory-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.agent-memory-file,
.agent-memory-missing-file {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 0.5rem 1rem;
  min-width: 0;
}
.agent-memory-file-link {
  overflow-wrap: anywhere;
  font-family: var(--oc-font-family-mono, monospace);
}
.agent-memory-file-missing-name {
  font-family: var(--oc-font-family-mono, monospace);
}
.agent-memory-file-meta {
  display: flex;
  flex-wrap: wrap;
  gap: 0.75rem;
  color: var(--oc-color-text-muted, #666);
  font-size: 0.875rem;
}
.agent-memory-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
</style>
