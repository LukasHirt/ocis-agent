<template>
  <div class="agent-composer">
    <oc-textarea
      ref="input"
      class="agent-composer-input"
      :model-value="modelValue"
      :label="$gettext('Message')"
      :placeholder="$gettext('Ask the agent… (Enter sends, Shift+Enter starts a new line)')"
      :submit-on-enter="false"
      :disabled="inputDisabled"
      rows="3"
      @update:model-value="(v: string) => emit('update:modelValue', v)"
      @keydown="onKeydown"
    />
    <div class="agent-composer-bar">
      <span
        v-if="nearLimit"
        :class="['agent-composer-count', { 'agent-composer-over': over }]"
        aria-live="polite"
        v-text="interpolate($gettext('%{n} / %{max} bytes'), { n: bytes, max: MAX_MESSAGE_BYTES })"
      />
      <span v-if="over" class="agent-composer-over-text" v-text="$gettext('Too long to send.')" />
      <span v-if="hint" class="agent-composer-hint" v-text="hint" />
      <oc-button
        class="agent-composer-send"
        appearance="filled"
        :disabled="sendDisabled"
        @click="trySend"
      >
        <oc-icon name="send-plane" size="small" />
        <span v-text="$gettext('Send')" />
      </oc-button>
      <oc-spinner v-if="sending" size="small" :aria-label="$gettext('Sending')" />
    </div>
    <p v-if="error" class="agent-composer-error" role="alert" v-text="error.message" />
  </div>
</template>

<script setup lang="ts">
import { computed, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import { MAX_MESSAGE_BYTES, byteLength } from '../../util/chatText'
import type { SendError } from '../../stores/chat'

// Composer is the send box: Enter sends, Shift+Enter makes a new line. The
// byte count shows near the server's limit. The parent sends and focuses
// the box again afterwards (focus()).
const props = withDefaults(
  defineProps<{
    modelValue: string
    sending?: boolean
    disabled?: boolean
    hint?: string
    error?: SendError | null
  }>(),
  { sending: false, disabled: false, hint: '', error: null }
)
const emit = defineEmits<{ 'update:modelValue': [string]; send: [] }>()
const { interpolate } = useGettext()

// the count shows from this many bytes on
const COUNT_FROM = MAX_MESSAGE_BYTES - 1024

const bytes = computed(() => byteLength(props.modelValue ?? ''))
const nearLimit = computed(() => bytes.value >= COUNT_FROM)
const over = computed(() => bytes.value > MAX_MESSAGE_BYTES)
const inputDisabled = computed(() => props.disabled || props.sending)
const sendDisabled = computed(
  () => inputDisabled.value || over.value || (props.modelValue ?? '').trim() === ''
)

// blank or too long text is not sent (the box already says why)
const trySend = () => {
  if (sendDisabled.value) return
  emit('send')
}

const onKeydown = (e: KeyboardEvent) => {
  // an Enter that commits an IME composition is not a send (Safari sends
  // it with isComposing false but keyCode 229)
  if (e.key !== 'Enter' || e.shiftKey || e.ctrlKey || e.altKey || e.metaKey) return
  if (e.isComposing || e.keyCode === 229) return
  e.preventDefault()
  trySend()
}

const input = ref<{ focus: () => void } | null>(null)
defineExpose({ focus: () => input.value?.focus() })
</script>

<style>
.agent-composer {
  display: flex;
  flex-direction: column;
  gap: 0.25rem;
}
.agent-composer-input textarea {
  resize: vertical;
  min-height: 4.5rem;
}
.agent-composer-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: flex-end;
  gap: 0.5rem;
  font-size: 0.85em;
}
.agent-composer-over,
.agent-composer-over-text,
.agent-composer-error {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-composer-over {
  font-weight: bold;
}
.agent-composer-hint {
  margin-right: auto;
  color: var(--oc-color-text-muted, #555);
}
.agent-composer-error {
  margin: 0;
}
</style>
