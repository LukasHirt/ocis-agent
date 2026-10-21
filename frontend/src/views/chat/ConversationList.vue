<template>
  <nav class="agent-convs" :aria-label="$gettext('Conversations')">
    <ErrorState
      v-if="showError"
      class="agent-convs-error"
      :title="chat.conversationsError ?? ''"
      :icon="agent.serviceDown ? 'wifi-off' : 'error-warning'"
      :compact="chat.conversationsLoaded"
      :busy="chat.conversationsLoading"
      @retry="retry"
    />
    <p
      v-if="chat.actionError"
      class="agent-convs-action-error agent-notice agent-notice--danger"
      role="alert"
      v-text="chat.actionError"
    />
    <LoadingState
      v-if="!chat.conversationsLoaded && !chat.conversationsError"
      variant="rows"
      :count="4"
      :label="$gettext('Loading the conversations')"
    />
    <EmptyState
      v-else-if="chat.conversationsLoaded && chat.conversations.length === 0"
      class="agent-convs-empty"
      icon="chat-3"
      :title="$gettext('No conversations yet. Start one to ask the agent something.')"
    />
    <ul class="agent-convs-list">
      <li
        v-for="c in chat.conversations"
        :key="c.id"
        :class="['agent-conv', { 'agent-conv-current': c.id === chat.current }]"
      >
        <div v-if="editing === c.id" class="agent-conv-rename">
          <oc-text-input
            ref="titleInput"
            v-model="draft"
            :label="$gettext('Title')"
            :error-message="renameError"
            @keydown="(e: KeyboardEvent) => onRenameKey(e, c.id)"
          />
          <oc-button size="small" appearance="filled" :disabled="saving" @click="save(c.id)" v-text="$gettext('Save')" />
          <oc-button size="small" :disabled="saving" @click="cancel" v-text="$gettext('Cancel')" />
        </div>
        <template v-else>
          <router-link
            class="agent-conv-link agent-row-link"
            :to="{ name: `${APPID}-chat-conversation`, params: { id: c.id } }"
            :aria-current="c.id === chat.current ? 'page' : undefined"
          >
            <span class="agent-conv-title">
              <TextBlock v-if="c.title" :text="c.title" wrap="nowrap" />
              <span v-else class="agent-conv-untitled" v-text="$gettext('New conversation')" />
            </span>
            <time
              v-if="c.updated_at"
              class="agent-conv-time"
              :datetime="c.updated_at"
              v-text="formatTime(c.updated_at)"
            />
          </router-link>
          <span class="agent-conv-actions agent-row-aside">
            <oc-button
              appearance="raw"
              size="small"
              :aria-label="interpolate($gettext('Rename %{title}'), { title: nameOf(c) }, true)"
              @click="edit(c)"
            >
              <oc-icon name="pencil" size="small" />
            </oc-button>
            <oc-button
              appearance="raw"
              size="small"
              :aria-label="interpolate($gettext('Delete %{title}'), { title: nameOf(c) }, true)"
              :disabled="deleting === c.id"
              @click="remove(c.id)"
            >
              <oc-icon name="delete-bin" size="small" />
            </oc-button>
          </span>
        </template>
      </li>
    </ul>
    <oc-button
      v-if="chat.conversationsCursor"
      appearance="outline"
      size="small"
      :disabled="chat.conversationsLoading"
      @click="chat.loadMoreConversations()"
      v-text="$gettext('Load more')"
    />
  </nav>
</template>

<script setup lang="ts">
import { computed, nextTick, ref } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import EmptyState from '../../components/state/EmptyState.vue'
import ErrorState from '../../components/state/ErrorState.vue'
import LoadingState from '../../components/state/LoadingState.vue'
import { APPID } from '../../appid'
import { useChatStore } from '../../stores/chat'
import { useAgentStore } from '../../stores/agent'
import { formatTime } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import type { Conversation } from '../../api/types'

// ConversationList lists the conversations (most recently updated first)
// with inline rename and a confirmed delete; it emits deleted after a
// conversation is gone, so the screen can leave it.
const emit = defineEmits<{ deleted: [string] }>()
const chat = useChatStore()
const agent = useAgentStore()
const { $gettext, interpolate } = useGettext()

// The error is shown once per screen: with an open conversation whose
// messages are not shown yet, the thread pane carries it; with the list
// loaded and the service down, the banner does.
const showError = computed(() => {
  if (!chat.conversationsError) return false
  if (!chat.conversationsLoaded) return !(chat.current && !chat.messagesLoaded)
  return !agent.serviceDown
})
const retry = () => {
  void chat.loadConversations()
  if (agent.serviceDown) void agent.retryNow()
}

// nameOf is a title for labels: control characters made visible. The
// labels are attribute bindings, so interpolate must not HTML-escape it
// (its third argument).
const nameOf = (c: Conversation) => (c.title ? visibleText(c.title).text : $gettext('New conversation'))

const editing = ref<string | null>(null)
const draft = ref('')
const renameError = ref<string | null>(null)
const saving = ref(false)
const deleting = ref<string | null>(null)
const titleInput = ref<Array<{ $el?: HTMLElement }> | null>(null)

const edit = async (c: Conversation) => {
  editing.value = c.id
  draft.value = c.title
  renameError.value = null
  await nextTick()
  const root = titleInput.value?.[0]?.$el
  root?.querySelector?.('input')?.focus()
}

const cancel = () => {
  editing.value = null
  renameError.value = null
}

// The keydown listener lands on the field's root and its input, so one
// key arrives twice: handle each event once. Enter saves, except the one
// that commits an IME composition (keyCode 229 in Safari).
let lastKey: KeyboardEvent | null = null
const onRenameKey = (e: KeyboardEvent, id: string) => {
  if (e === lastKey) return
  lastKey = e
  if (e.key === 'Escape') cancel()
  else if (e.key === 'Enter' && !e.isComposing && e.keyCode !== 229) void save(id)
}

const save = async (id: string) => {
  if (saving.value) return
  const problem = chat.titleError(draft.value)
  if (problem) {
    renameError.value = problem
    return
  }
  saving.value = true
  try {
    if (await chat.rename(id, draft.value)) {
      cancel()
    } else {
      // shown at the field, not twice
      renameError.value = chat.actionError
      chat.actionError = null
    }
  } finally {
    saving.value = false
  }
}

const remove = async (id: string) => {
  if (deleting.value) return
  deleting.value = id
  try {
    if (await chat.remove(id)) emit('deleted', id)
  } finally {
    deleting.value = null
  }
}
</script>

<style>
.agent-convs {
  flex: 1;
  min-height: 0;
  min-width: 0;
  overflow-y: auto;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  padding: 0 var(--agent-space-2) var(--agent-space-4);
}
.agent-convs-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-1);
}
.agent-conv {
  position: relative;
  display: flex;
  align-items: center;
  gap: var(--agent-space-1);
  border-radius: var(--agent-radius-m);
  padding: var(--agent-space-2) var(--agent-space-3);
  min-width: 0;
}
.agent-conv:hover {
  background: var(--oc-color-background-hover);
}
.agent-conv-current,
.agent-conv-current:hover {
  background: var(--oc-color-background-highlight);
  box-shadow: inset 3px 0 0 var(--oc-color-swatch-primary-default);
}
.agent-conv-link {
  flex: 1 1 auto;
  min-width: 0;
  display: flex;
  flex-direction: column;
  text-decoration: none;
  color: inherit;
}
.agent-conv-title {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.agent-conv-current .agent-conv-title {
  font-weight: 600;
}
.agent-conv-untitled {
  font-style: italic;
}
.agent-conv-time {
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
.agent-conv-actions {
  display: flex;
  gap: var(--agent-space-1);
  opacity: 0;
}
.agent-conv:hover .agent-conv-actions,
.agent-conv:focus-within .agent-conv-actions,
.agent-conv-current .agent-conv-actions {
  opacity: 1;
}
@media (hover: none) {
  .agent-conv-actions {
    opacity: 1;
  }
}
.agent-conv-rename {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--agent-space-1);
  width: 100%;
}
</style>
