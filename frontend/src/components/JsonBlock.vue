<template>
  <div class="agent-json">
    <div v-if="canCopy" class="agent-json-actions">
      <oc-button appearance="raw" size="small" @click="copy">
        <oc-icon name="file-copy" size="small" />
        <span v-text="copied ? $gettext('Copied') : $gettext('Copy')" />
      </oc-button>
    </div>
    <TextBlock class="agent-json-pre" :style="preStyle" :text="shown" mono />
  </div>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, ref } from 'vue'
import TextBlock from './TextBlock.vue'
import { prettyJson, stringifyValue } from '../util/json'

// JsonBlock shows JSON in full (approval cards need the exact arguments):
// raw text is re-indented as stored, invalid JSON is shown as text.
const props = defineProps<{ value?: unknown; raw?: string }>()

const shown = computed(() => {
  if (typeof props.raw === 'string') return prettyJson(props.raw) ?? props.raw
  return stringifyValue(props.value)
})

const preStyle = { maxHeight: '24rem', overflow: 'auto' }

const canCopy = typeof navigator !== 'undefined' && !!navigator.clipboard?.writeText
const copied = ref(false)
let resetTimer: ReturnType<typeof setTimeout> | undefined

const clearResetTimer = () => {
  if (resetTimer !== undefined) clearTimeout(resetTimer)
  resetTimer = undefined
}

const copy = async () => {
  try {
    await navigator.clipboard.writeText(shown.value)
    copied.value = true
    clearResetTimer()
    resetTimer = setTimeout(() => {
      resetTimer = undefined
      copied.value = false
    }, 2000)
  } catch {
    copied.value = false
  }
}

onBeforeUnmount(clearResetTimer)
</script>

<style>
.agent-json {
  position: relative;
}
.agent-json-actions {
  display: flex;
  justify-content: flex-end;
}
.agent-json-pre {
  background: var(--oc-color-background-muted);
  padding: var(--agent-space-2) var(--agent-space-3);
  border-radius: var(--agent-radius-s);
  font-size: var(--agent-font-xs);
}
</style>
