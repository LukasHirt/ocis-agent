<template>
  <section class="agent-settings oc-p-m">
    <h1 v-text="$gettext('Settings')" />
    <AgentSection />

    <div v-if="!base" class="agent-settings-loading">
      <template v-if="store.errors.settings">
        <p role="alert" v-text="store.errors.settings" />
        <oc-button
          :disabled="store.loading.settings"
          @click="store.refreshSettings()"
          v-text="$gettext('Try again')"
        />
      </template>
      <oc-spinner v-else :aria-label="$gettext('Loading the settings')" />
    </div>
    <div v-else class="agent-settings-form">
      <p v-if="store.errors.settings" class="agent-settings-error" role="alert">
        <span v-text="store.errors.settings" />
        <span v-text="' ' + $gettext('What is shown may be out of date.')" />
      </p>
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
      <p v-if="saveError" class="agent-settings-error" data-testid="agent-save-error" role="alert">
        <span v-text="saveError.message" />
        <span v-if="saveError.detail" v-text="' ' + saveError.detail" />
      </p>
      <div class="agent-settings-actions agent-settings-save">
        <oc-button appearance="filled" :disabled="!canSave" @click="save" v-text="$gettext('Save')" />
        <oc-button :disabled="!dirty || saving" @click="discard" v-text="$gettext('Discard changes')" />
        <oc-spinner v-if="saving" :aria-label="$gettext('Saving the settings')" />
      </div>
    </div>

    <TokenSection />
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

const discard = () => {
  if (saving.value) return
  if (store.settings) reset(store.settings)
  else if (base.value) reset(base.value)
}
</script>

<style>
.agent-settings {
  max-width: 48rem;
}
.agent-settings-section {
  margin: 0 0 1.5rem;
}
.agent-settings-actions,
.agent-settings-add {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: 0.5rem;
}
.agent-settings-facts {
  display: grid;
  grid-template-columns: max-content 1fr;
  gap: 0.25rem 1rem;
}
.agent-settings-facts dd {
  margin: 0;
}
.agent-settings-error,
.agent-settings-warning {
  color: var(--oc-color-swatch-danger-default, #b00020);
}
.agent-settings-hint {
  font-size: 0.9em;
}
.agent-settings-progress {
  width: 100%;
  max-width: 24rem;
}
.agent-settings-dialog {
  border: 1px solid var(--oc-color-swatch-danger-default, #b00020);
  border-radius: 4px;
  padding: 0.75rem;
  margin: 0.5rem 0;
}
.agent-settings-watch {
  list-style: none;
  padding: 0;
}
.agent-settings-watch li {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}
.agent-settings-watch pre {
  margin: 0;
}
.agent-settings-badge {
  display: inline-block;
  margin-left: 0.5rem;
  padding: 0 0.4rem;
  border: 1px solid currentColor;
  border-radius: 3px;
  font-size: 0.8em;
}
.agent-settings-token-table {
  border-collapse: collapse;
  width: 100%;
}
.agent-settings-token-table th,
.agent-settings-token-table td {
  text-align: left;
  vertical-align: top;
  padding: 0.25rem 0.5rem;
}
.agent-settings-token-table pre {
  margin: 0;
  display: inline;
}
</style>
