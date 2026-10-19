<template>
  <form class="agent-memory-new" @submit.prevent="submit">
    <h2 v-text="$gettext('New note')" />
    <oc-text-input
      v-model="slug"
      class="agent-memory-new-slug"
      :label="$gettext('Name')"
      :error-message="slugError ?? undefined"
      autocomplete="off"
      spellcheck="false"
    />
    <p class="agent-memory-new-hint">
      <span v-text="$gettext('The note is saved as')" />
      <code class="agent-memory-new-name" v-text="name" />
    </p>
    <oc-textarea
      v-model="content"
      class="agent-memory-new-content"
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
  display: flex;
  flex-direction: column;
  gap: 0.5rem;
  max-width: 40rem;
}
.agent-memory-new h2 {
  margin: 0;
  font-size: 1.1rem;
}
.agent-memory-new-hint {
  margin: 0;
  display: flex;
  flex-wrap: wrap;
  gap: 0.25rem;
}
.agent-memory-new-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-memory-actions {
  display: flex;
  flex-wrap: wrap;
  gap: 0.5rem;
}
</style>
