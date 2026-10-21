<template>
  <section class="agent-landing" :aria-labelledby="titleId">
    <h2 :id="titleId" class="agent-landing-title" v-text="greeting" />
    <Composer
      ref="composer"
      :model-value="modelValue"
      :sending="sending"
      :disabled="disabled"
      :hint="hint"
      :error="error"
      @update:model-value="(v: string) => emit('update:modelValue', v)"
      @send="emit('send')"
    />
    <ul v-if="!disabled" class="agent-landing-examples">
      <li v-for="e in examples" :key="e">
        <oc-button appearance="outline" size="small" @click="fill(e)">
          <span v-text="e" />
        </oc-button>
      </li>
    </ul>
  </section>
</template>

<script setup lang="ts">
import { computed, nextTick, onMounted, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import Composer from './Composer.vue'
import { visibleText } from '../../util/visibleText'
import type { SendError } from '../../stores/chat'

// Landing is the chat screen without an open conversation: a greeting, the
// send box and a few example questions that only fill the box.
const props = defineProps<{
  name: string // the user's display name: untrusted text
  modelValue: string
  sending: boolean
  disabled: boolean
  hint: string
  error: SendError | null
}>()
const emit = defineEmits<{ 'update:modelValue': [string]; send: [] }>()
const { $gettext, interpolate } = useGettext()

const NAME_MAX = 60
const titleId = `agent-landing-title-${Math.random().toString(36).slice(2, 8)}`

const shortName = computed(() => {
  const t = visibleText(props.name ?? '').text.trim()
  const chars = Array.from(t)
  return chars.length > NAME_MAX ? chars.slice(0, NAME_MAX).join('') + '…' : t
})
const greeting = computed(() =>
  shortName.value
    ? interpolate($gettext('Hello, %{name}. How can I help?'), { name: shortName.value }, true)
    : $gettext('How can I help?')
)

const examples = computed(() => [
  $gettext('What can you do?'),
  $gettext('Which files in my space changed this week?'),
  $gettext('Create a text file with a short to-do list')
])

const composer = ref<{ focus: () => void } | null>(null)
const fill = (text: string) => {
  emit('update:modelValue', text)
  composer.value?.focus()
}

// Focus the box only when nothing else holds the focus: a host control
// (search, navigation, another app) keeps it.
onMounted(async () => {
  await nextTick()
  const a = document.activeElement
  if (a === null || a === document.body || a.closest('.agent-layout')) composer.value?.focus()
})
defineExpose({ focus: () => composer.value?.focus() })
</script>

<style>
.agent-landing {
  flex: 1 1 auto;
  display: flex;
  flex-direction: column;
  align-items: center;
  justify-content: center;
  gap: var(--agent-space-4);
  padding: var(--agent-space-6) var(--agent-space-5);
  min-height: 0;
  overflow-y: auto;
}
.agent-landing > * {
  width: 100%;
  max-width: 44rem;
}
.agent-landing-title {
  margin: 0;
  text-align: center;
  font-size: var(--agent-font-xl);
  font-weight: 600;
  overflow: hidden;
  text-overflow: ellipsis;
}
.agent-landing .agent-composer {
  padding: 0;
  border-top: 0;
  background: transparent;
}
.agent-landing .agent-composer-box,
.agent-landing .agent-composer-error {
  max-width: none;
}
.agent-landing-examples {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-wrap: wrap;
  justify-content: center;
  gap: var(--agent-space-2);
}
</style>
