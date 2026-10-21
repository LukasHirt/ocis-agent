<template>
  <form class="agent-memory-new agent-surface" @submit.prevent="submit">
    <h2 class="agent-section-title" v-text="$gettext('New note')" />
    <oc-text-input
      v-model="slug"
      class="agent-memory-new-slug agent-field"
      :label="$gettext('Name')"
      :error-message="slugError ?? undefined"
      autocomplete="off"
      spellcheck="false"
    />
    <p class="agent-memory-new-hint agent-field-help">
      <span v-text="$gettext('The note is saved as')" />
      <code class="agent-memory-new-name" v-text="name" />
    </p>
    <oc-textarea
      v-model="content"
      class="agent-memory-new-content agent-field"
      :label="$gettext('Content (optional)')"
      :submit-on-enter="false"
      :error-message="contentError ?? undefined"
      rows="6"
    />
    <p v-if="mem.createError" role="alert" class="agent-memory-new-error" v-text="mem.createError" />
    <div class="agent-memory-actions">
      <oc-button
        appearance="filled"
        variation="primary"
        :disabled="!canCreate"
        @click="submit"
        v-text="$gettext('Create')"
      />
      <oc-button :disabled="mem.creating" @click="emit('cancel')" v-text="$gettext('Cancel')" />
    </div>
  </form>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useMemoryStore } from '../../stores/memory'
import { MAX_SLUG_CHARS, checkContent, checkSlug, describeContentProblem, noteName } from '../../util/memoryText'

// The form for a new note: notes/<name>.md, with the server's name rules
// checked as the user types.
const emit = defineEmits<{ created: [name: string]; cancel: [] }>()
const mem = useMemoryStore()
const { $gettext, interpolate } = useGettext()

const slug = ref('')
const content = ref('')

const name = computed(() => noteName(slug.value || '…'))

const slugError = computed<string | null>(() => {
  switch (checkSlug(slug.value)) {
    case 'invalid':
      return $gettext('Use 1 to 64 lower-case letters, digits and hyphens, starting with a letter or digit.')
    case 'too_long':
      return interpolate($gettext('A name can have at most %{max} characters.'), { max: MAX_SLUG_CHARS })
    default:
      return null
  }
})

const contentError = computed<string | null>(() => {
  const p = checkContent(content.value)
  return p ? describeContentProblem(p, $gettext) : null
})

const canCreate = computed(
  () => !mem.creating && checkSlug(slug.value) === null && contentError.value === null
)

const submit = async () => {
  if (!canCreate.value) return
  const n = noteName(slug.value)
  if (await mem.create(n, content.value)) emit('created', n)
}

onMounted(() => mem.clearCreateError())
onBeforeUnmount(() => mem.clearCreateError())
</script>

<style>
.agent-memory-new {
  max-width: 40rem;
}
.agent-memory-new .agent-field {
  max-width: none;
}
.agent-memory-new-hint {
  display: flex;
  flex-wrap: wrap;
  gap: var(--agent-space-1);
}
.agent-memory-new-error {
  margin: 0;
  color: var(--oc-color-swatch-danger-default);
}
.agent-memory-actions {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: var(--agent-space-2);
}
</style>
