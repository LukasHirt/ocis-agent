<template>
  <div class="agent-composer">
    <div class="agent-composer-box">
    <oc-textarea
      ref="input"
      class="agent-composer-input"
      :model-value="modelValue"
      :label="$gettext('Message')"
      :placeholder="$gettext('Ask the agent…')"
      :submit-on-enter="false"
      :disabled="inputDisabled"
      :rows="rows"
      @update:model-value="(v: string) => emit('update:modelValue', v)"
      @keydown="onKeydown"
    />
    <div class="agent-composer-bar">
      <span class="agent-composer-keys" v-text="$gettext('Enter sends, Shift+Enter starts a new line')" />
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
        variation="primary"
        size="small"
        :disabled="sendDisabled"
        @click="trySend"
      >
        <oc-icon name="send-plane" size="small" />
        <span v-text="$gettext('Send')" />
      </oc-button>
      <oc-spinner v-if="sending" size="small" :aria-label="$gettext('Sending')" />
    </div>
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
const { $gettext, interpolate } = useGettext()

// the count shows from this many bytes on
const COUNT_FROM = MAX_MESSAGE_BYTES - 1024

// the box grows with the lines typed, from 2 to 8 rows, then scrolls
const rows = computed(() => Math.min(8, Math.max(2, (props.modelValue ?? '').split('\n').length)))

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
  flex: none;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-1);
  padding: var(--agent-space-3) var(--agent-space-5) var(--agent-space-4);
  border-top: var(--agent-line);
  background: var(--oc-color-background-default);
}
.agent-composer-box {
  width: 100%;
  max-width: 52rem;
  margin: 0 auto;
  box-sizing: border-box;
  border: 1px solid var(--oc-color-input-border);
  border-radius: var(--agent-radius-m);
  background: var(--oc-color-input-bg);
  padding: var(--agent-space-2);
}
.agent-composer-box:focus-within {
  border-color: var(--oc-color-swatch-primary-default);
}
.agent-composer-input .oc-label {
  position: absolute;
  width: 1px;
  height: 1px;
  padding: 0;
  margin: -1px;
  overflow: hidden;
  clip: rect(0 0 0 0);
  white-space: nowrap;
  border: 0;
}
.agent-composer-input textarea {
  width: 100%;
  box-sizing: border-box;
  background: transparent;
  border: 0;
  resize: none;
  min-height: auto;
  max-height: 12rem;
  padding: var(--agent-space-1);
}
.agent-composer-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  justify-content: flex-end;
  gap: var(--agent-space-2);
  font-size: var(--agent-font-xs);
}
.agent-composer-keys {
  margin-right: auto;
  color: var(--oc-color-text-muted);
}
.agent-composer-over,
.agent-composer-over-text,
.agent-composer-error {
  color: var(--oc-color-swatch-danger-default);
}
.agent-composer-over {
  font-weight: 600;
}
.agent-composer-hint {
  color: var(--oc-color-text-muted);
}
.agent-composer-error {
  width: 100%;
  max-width: 52rem;
  margin: 0 auto;
}
</style>
