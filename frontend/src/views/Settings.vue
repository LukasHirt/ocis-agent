<template>
  <section class="agent-settings agent-page">
    <header class="agent-page-head">
      <div class="agent-page-titles">
        <h1 class="agent-page-title" v-text="$gettext('Settings')" />
      </div>
    </header>
    <div class="agent-settings-layout">
      <nav class="agent-settings-nav" :aria-label="$gettext('Settings sections')">
        <oc-button v-for="item in navItems" :key="item.id" appearance="raw" @click="go(item.id)" v-text="item.label" />
      </nav>
      <div class="agent-settings-content">
    <AgentSection />

    <div v-if="!base" class="agent-settings-loading">
      <ErrorState
        v-if="store.errors.settings"
        :title="store.errors.settings"
        :icon="store.serviceDown ? 'wifi-off' : 'error-warning'"
        :busy="store.loading.settings"
        @retry="retry"
      />
      <LoadingState v-else variant="cards" :count="3" :label="$gettext('Loading the settings')" />
    </div>
    <div v-else class="agent-settings-form">
      <ErrorState
        v-if="store.errors.settings && !store.serviceDown"
        compact
        class="agent-settings-error"
        :title="store.errors.settings"
        :text="$gettext('What is shown may be out of date.')"
        :busy="store.loading.settings"
        @retry="retry"
      />
      <p
        v-if="changedElsewhere"
        class="agent-settings-note"
        role="status"
        v-text="
          $gettext(
            'The settings were changed elsewhere. Saving applies only your changes; discard them to see the current settings.'
          )
        "
      />
      <BudgetSection />
      <ModelSection />
      <PolicySection />
      <p v-if="saveError" class="agent-settings-error agent-notice agent-notice--danger" data-testid="agent-save-error" role="alert">
        <span v-text="saveError.message" />
        <span v-if="saveError.detail" v-text="' ' + saveError.detail" />
      </p>
      <div class="agent-settings-actions agent-settings-save">
        <span v-if="dirty" class="agent-settings-unsaved">
          <oc-icon name="pencil" size="small" />
          <span v-text="$gettext('Unsaved changes')" />
        </span>
        <oc-button appearance="filled" variation="primary" :disabled="!canSave" @click="save" v-text="$gettext('Save')" />
        <oc-button :disabled="!dirty || saving" @click="discard" v-text="$gettext('Discard changes')" />
        <oc-spinner v-if="saving" :aria-label="$gettext('Saving the settings')" />
      </div>
    </div>

    <TokenSection />
      </div>
    </div>
  </section>
</template>

<script setup lang="ts">
import { computed, reactive, ref, shallowRef, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../stores/agent'
import { usePlatform } from '../platform'
import type { Settings } from '../api/types'
import AgentSection from './settings/AgentSection.vue'
import BudgetSection from './settings/BudgetSection.vue'
import ModelSection from './settings/ModelSection.vue'
import PolicySection from './settings/PolicySection.vue'
import TokenSection from './settings/TokenSection.vue'
import ErrorState from '../components/state/ErrorState.vue'
import LoadingState from '../components/state/LoadingState.vue'
import { provideSettingsForm } from './settings/form'
import { buildUpdate, draftOf, draftsEqual, formProblems, sameSettings, type Draft } from './settings/rules'
import { useReport, type ErrorText } from './settings/report'

// The settings page. The form sections edit a draft; Save sends only the
// fields that differ from the settings the draft was made from, and the
// form then shows what the server stored (nothing is applied before the
// server answered).
const store = useAgentStore()
const platform = usePlatform()
const { $gettext } = useGettext()
const { describe } = useReport()

const retry = () => {
  void store.refreshSettings()
  if (store.serviceDown) void store.retryNow()
}

const base = shallowRef<Settings | null>(null)
const draft = reactive<Draft>({
  proactive_enabled: false,
  watch: [],
  sweepSeconds: null,
  quiet_hours: '',
  dailyTokens: '',
  followOperator: false,
  models: { chat: null, proactive: null }
})
const saving = ref(false)
const saveError = ref<ErrorText | null>(null)
const changedElsewhere = ref(false)

const problems = computed(() => (base.value ? formProblems(draft, base.value) : {}))
const update = computed(() => (base.value ? buildUpdate(base.value, draft) : {}))
// dirty compares the draft with the settings it was made from, typed text
// included; Save needs an update that actually sends something.
const dirty = computed(() => (base.value ? !draftsEqual(draft, draftOf(base.value)) : false))
const canSave = computed(
  () => Object.keys(update.value).length > 0 && !saving.value && Object.keys(problems.value).length === 0
)

provideSettingsForm({ base, draft, problems, saving })

const reset = (s: Settings) => {
  base.value = s
  Object.assign(draft, draftOf(s))
  changedElsewhere.value = false
  saveError.value = null
}

// Settings read while the form is untouched replace it; while the user is
// editing, the edits stay (relative to what they saw) and a note says so.
watch(
  () => store.settings,
  (s) => {
    if (!s || saving.value || s === base.value) return
    if (base.value && sameSettings(s, base.value)) base.value = s // same content: nothing to say
    else if (!base.value || !dirty.value) reset(s)
    else changedElsewhere.value = true
  },
  { immediate: true }
)

const save = async () => {
  if (!canSave.value) return
  const body = update.value
  saving.value = true
  saveError.value = null
  try {
    const v = await store.saveSettings(body)
    reset(v)
    platform.notify('success', $gettext('Settings saved'))
  } catch (e) {
    saveError.value = describe(e)
  } finally {
    saving.value = false
  }
}

const navItems = computed(() => [
  { id: 'agent-settings-agent-h', label: $gettext('Agent') },
  { id: 'agent-settings-budget-h', label: $gettext('Daily token budget') },
  { id: 'agent-settings-models-h', label: $gettext('Models') },
  { id: 'agent-settings-policy-h', label: $gettext('Proactive work') },
  { id: 'agent-settings-tokens-h', label: $gettext('App token') }
])

// The host's router owns the URL, so the nav scrolls and moves focus
// instead of linking to a fragment.
const go = (id: string) => {
  const reduce = typeof window.matchMedia === 'function' && window.matchMedia('(prefers-reduced-motion: reduce)').matches
  document.getElementById(id)?.scrollIntoView({ block: 'start', behavior: reduce ? 'auto' : 'smooth' })
  ;(document.getElementById(id) as HTMLElement | null)?.focus({ preventScroll: true })
}

const discard = () => {
  if (saving.value) return
  if (store.settings) reset(store.settings)
  else if (base.value) reset(base.value)
}
</script>

<style>
.agent-settings-layout {
  display: block;
  min-width: 0;
}
.agent-settings-nav {
  display: none;
}
@container agent (min-width: 900px) {
  .agent-settings-layout {
    display: grid;
    grid-template-columns: 11rem minmax(0, 48rem);
    gap: var(--agent-space-5);
    align-items: start;
  }
  .agent-settings-nav {
    position: sticky;
    top: var(--agent-space-4);
    align-self: start;
    display: flex;
    flex-direction: column;
    gap: var(--agent-space-1);
  }
  .agent-settings-nav .oc-button {
    justify-content: flex-start;
    width: 100%;
    padding: var(--agent-space-2) var(--agent-space-3);
    text-align: left;
    border-radius: var(--agent-radius-s);
  }
  .agent-settings-nav .oc-button:hover {
    background: var(--oc-color-background-hover);
  }
}
.agent-settings-content {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
  min-width: 0;
}
.agent-settings-section {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-3);
  min-width: 0;
  padding: var(--agent-space-4);
  background: var(--oc-color-background-default);
  border: var(--agent-line);
  border-radius: var(--agent-radius-m);
  box-sizing: border-box;
}
.agent-settings-section h2,
.agent-settings-section h3,
.agent-settings-section h4,
.agent-settings-section p,
.agent-settings-section dl {
  margin: 0;
}
.agent-settings-section h2 {
  scroll-margin-top: var(--agent-space-4);
}
.agent-settings-section h3,
.agent-settings-section h4 {
  font-size: var(--agent-font-m);
  font-weight: 600;
  line-height: 1.3;
}
.agent-settings-form {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-4);
  min-width: 0;
}
.agent-settings-loading {
  min-width: 0;
}
.agent-settings-actions,
.agent-settings-add {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: var(--agent-space-2);
}
.agent-settings-save {
  position: sticky;
  bottom: 0;
  z-index: 2;
  align-items: center;
  padding: var(--agent-space-3) var(--agent-space-4);
  background: var(--oc-color-background-default);
  border: var(--agent-line);
  border-radius: var(--agent-radius-m);
  box-shadow: 0 -4px 12px color-mix(in srgb, var(--oc-color-text-default) 8%, transparent);
}
.agent-settings-unsaved {
  display: inline-flex;
  align-items: center;
  gap: var(--agent-space-1);
  margin-right: auto;
  font-weight: 600;
}
.agent-settings-unsaved .oc-icon svg {
  fill: currentColor;
}
.agent-settings-facts {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: var(--agent-space-1) var(--agent-space-4);
}
.agent-settings-facts dt {
  font-size: var(--agent-font-xs);
  font-weight: 600;
  color: var(--oc-color-text-muted);
}
.agent-settings-facts dd {
  margin: 0;
}
.agent-settings-state {
  font-weight: 600;
}
.agent-settings-error,
.agent-settings-warning {
  color: var(--oc-color-swatch-danger-default);
}
.agent-settings-hint {
  font-size: var(--agent-font-xs);
  color: var(--oc-color-text-muted);
}
.agent-settings-usage {
  font-weight: 600;
}
.agent-settings-progress {
  width: 100%;
  max-width: none;
  height: 0.5rem;
  accent-color: var(--oc-color-swatch-primary-default);
}
.agent-settings-danger {
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  align-items: flex-start;
  padding: var(--agent-space-3);
  border: 1px solid color-mix(in srgb, var(--oc-color-swatch-danger-default) 40%, transparent);
  border-radius: var(--agent-radius-m);
}
.agent-settings-danger .agent-card-title {
  color: var(--oc-color-swatch-danger-default);
}
.agent-settings-dialog {
  align-self: stretch;
  display: flex;
  flex-direction: column;
  gap: var(--agent-space-2);
  padding: var(--agent-space-3);
  background: var(--oc-color-background-default);
  border: 1px solid var(--oc-color-swatch-danger-default);
  border-radius: var(--agent-radius-m);
}
.agent-settings-watch {
  list-style: none;
  margin: 0;
  padding: 0;
  border: var(--agent-line);
  border-radius: var(--agent-radius-m);
  overflow: hidden;
}
.agent-settings-watch li {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--agent-space-2);
  padding: var(--agent-space-2) var(--agent-space-3);
}
.agent-settings-watch li + li {
  border-top: var(--agent-line);
}
.agent-settings-watch pre {
  margin: 0;
  font-family: var(--agent-font-mono);
  min-width: 0;
}
.agent-settings-badge {
  display: inline-block;
  margin-left: var(--agent-space-2);
  padding: 0 var(--agent-space-2);
  border: 1px solid currentColor;
  border-radius: var(--agent-radius-pill);
  font-size: var(--agent-font-xs);
}
.agent-settings-token-table {
  border-collapse: collapse;
  width: 100%;
}
.agent-settings-token-table th,
.agent-settings-token-table td {
  text-align: left;
  vertical-align: top;
  padding: var(--agent-space-2);
}
.agent-settings-token-table th {
  font-size: var(--agent-font-xs);
  font-weight: 600;
  color: var(--oc-color-text-muted);
}
.agent-settings-token-table tr + tr {
  border-top: var(--agent-line);
}
.agent-settings-token-table pre {
  margin: 0;
  display: inline;
}
</style>
