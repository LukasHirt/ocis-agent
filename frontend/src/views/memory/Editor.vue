<template>
  <div class="agent-memory-editor">
    <p v-if="mem.serverChanged && !mem.conflict" class="agent-memory-banner agent-memory-changed" role="status">
      <oc-icon name="error-warning" size="small" />
      <span v-text="$gettext('Changed on the server. Your text is kept; saving shows both versions so you can choose.')" />
    </p>

    <section v-if="mem.conflict" class="agent-memory-conflict">
      <p
        role="alert"
        v-text="
          mem.conflict.current
            ? $gettext('The file changed since you opened it. Compare the versions and choose which to keep.')
            : $gettext('The file was deleted since you opened it. Your version is kept below.')
        "
      />
      <div class="agent-memory-versions">
        <div class="agent-memory-version">
          <h3 v-text="$gettext('Your version')" />
          <TextBlock :text="mem.draft" mono :max-lines="30" />
        </div>
        <div v-if="mem.conflict.current" class="agent-memory-version">
          <h3 v-text="$gettext('Current version')" />
          <TextBlock :text="mem.conflict.current.content" mono :max-lines="30" />
        </div>
      </div>
      <div class="agent-memory-actions">
        <template v-if="mem.conflict.current">
          <oc-button
            appearance="filled"
            variation="danger"
            :disabled="mem.saving"
            @click="mem.overwriteMine()"
            v-text="$gettext('Overwrite with mine')"
          />
          <oc-button :disabled="mem.saving" @click="mem.useCurrent()" v-text="$gettext('Use current')" />
        </template>
        <oc-button
          v-else
          appearance="filled"
          variation="primary"
          :disabled="mem.saving"
          @click="mem.recreate()"
          v-text="$gettext('Create it again with my version')"
        />
        <oc-button :disabled="mem.saving" @click="mem.keepEditing()" v-text="$gettext('Keep editing')" />
      </div>
    </section>

    <template v-else>
      <oc-textarea
        v-model="mem.draft"
        class="agent-memory-draft"
        :label="label"
        :submit-on-enter="false"
        :error-message="problem ?? undefined"
        rows="18"
        spellcheck="false"
      />
      <p class="agent-memory-size" v-text="size" />
      <p
        v-if="mem.mixedLineEndings"
        class="agent-memory-eol"
        role="status"
        v-text="$gettext('This file mixes line endings. Saving it changes them all to LF (Unix style).')"
      />
    </template>

    <p v-if="mem.saveError" role="alert" class="agent-memory-save-error" v-text="mem.saveError" />

    <div v-if="!mem.conflict" class="agent-memory-actions">
      <oc-button
        appearance="filled"
        variation="primary"
        :disabled="!canSave"
        @click="mem.save()"
        v-text="mem.saving ? $gettext('Saving…') : $gettext('Save')"
      />
      <oc-button :disabled="mem.saving" @click="mem.cancelEdit()" v-text="$gettext('Cancel')" />
    </div>
  </div>
</template>

<script setup lang="ts">
import { computed, getCurrentInstance, onBeforeUnmount, onMounted } from 'vue'
import { useGettext } from 'vue3-gettext'
import type { RouteLocationNormalized } from 'vue-router'
import TextBlock from '../../components/TextBlock.vue'
import { useMemoryStore } from '../../stores/memory'
import { formatBytes } from '../../util/format'
import { MAX_FILE_BYTES, describeContentProblem } from '../../util/memoryText'

// The editor of the open memory file. The draft lives in the store and is
// never replaced while it differs from the version it started from.
// Leaving with unsaved changes asks first: a guard on the host's router
// (vue-router is never bundled) and the browser's beforeunload prompt.
const mem = useMemoryStore()
const { $gettext, interpolate } = useGettext()

const encoder = new TextEncoder()

const label = computed(() => interpolate($gettext('Content of %{name}'), { name: mem.openName ?? '' }))
const problem = computed(() => {
  const p = mem.contentProblem
  return p ? describeContentProblem(p, $gettext) : null
})
const size = computed(() =>
  interpolate($gettext('%{used} of %{max}'), {
    used: formatBytes(encoder.encode(mem.toSave).length),
    max: formatBytes(MAX_FILE_BYTES)
  })
)
const canSave = computed(() => mem.dirty && !mem.saving && problem.value === null)

const instance = getCurrentInstance()
let removeGuard: (() => void) | null = null

// The host's sign-in and sign-out routes (and its redirect when access is
// denied) pass without asking: the session may already be gone, so the
// editor could not save anyway. The browser's beforeunload prompt still
// covers a full page load.
const AUTH_ROUTES: readonly string[] = ['login', 'logout', 'oidcCallback', 'oidcSilentRedirect', 'accessDenied']
const isAuthRoute = (to: RouteLocationNormalized) => typeof to.name === 'string' && AUTH_ROUTES.includes(to.name)

const onBeforeUnload = (e: BeforeUnloadEvent) => {
  if (!mem.dirty) return
  e.preventDefault()
  // older browsers need a return value to show the prompt
  e.returnValue = ''
}

onMounted(() => {
  const router = instance?.proxy?.$router
  if (router && typeof router.beforeEach === 'function') {
    removeGuard = router.beforeEach((to, from) =>
      to.fullPath === from.fullPath || isAuthRoute(to) ? true : mem.confirmLeave()
    )
  }
  window.addEventListener('beforeunload', onBeforeUnload)
})

onBeforeUnmount(() => {
  removeGuard?.()
  removeGuard = null
  window.removeEventListener('beforeunload', onBeforeUnload)
})
</script>

<style>
.agent-memory-editor {
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  min-width: 0;
}
.agent-memory-draft textarea {
  font-family: var(--oc-font-family-mono, monospace);
  width: 100%;
  box-sizing: border-box;
}
.agent-memory-size {
  margin: 0;
  font-size: 0.875rem;
  color: var(--oc-color-text-muted, #666);
}
.agent-memory-save-error,
.agent-memory-conflict > [role='alert'] {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-memory-versions {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(16rem, 1fr));
  gap: 0.75rem;
}
.agent-memory-version {
  min-width: 0;
}
.agent-memory-version h3 {
  margin: 0 0 0.25rem;
  font-size: 1rem;
}
.agent-memory-version .agent-text {
  padding: 0.5rem;
  border: 1px solid var(--oc-color-border, #ddd);
  border-radius: 4px;
}
</style>
