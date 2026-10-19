<template>
  <section class="agent-settings-section agent-settings-models" aria-labelledby="agent-settings-models-h">
    <h2 id="agent-settings-models-h" v-text="$gettext('Models')" />
    <p v-if="available.length === 0" v-text="$gettext('The operator offers no models to choose from.')" />
    <template v-else>
      <div v-for="task in tasks" :key="task.key" class="agent-settings-field">
        <oc-select
          :model-value="selected(task.key)"
          :options="options"
          option-label="label"
          :clearable="false"
          :searchable="false"
          :label="task.label"
          :data-testid="'agent-model-' + task.key"
          :disabled="form.saving.value"
          @update:model-value="(o: Option | null) => choose(task.key, o)"
        />
        <p
          class="agent-settings-hint"
          :data-testid="'agent-model-' + task.key + '-effective'"
          v-text="
            $gettextInterpolate($gettext('Used: %{model}'), { model: labelOf(form.draft.models[task.key] ?? defaultId) }, true)
          "
        />
      </div>
    </template>
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../../stores/agent'
import { visibleText } from '../../util/visibleText'
import { useSettingsForm } from './form'

type Task = 'chat' | 'proactive'
interface Option {
  id: string | null // null: the operator's default
  label: string
}

// ModelSection: the model per task kind, from the operator's catalog; the
// operator default clears the preference (null).
const store = useAgentStore()
const form = useSettingsForm()
const { $gettext, interpolate } = useGettext()

const tasks = computed<{ key: Task; label: string }[]>(() => [
  { key: 'chat', label: $gettext('Model for chat') },
  { key: 'proactive', label: $gettext('Model for proactive work') }
])

const available = computed(() => store.status?.models.available ?? [])
const defaultId = computed(() => store.status?.models.default ?? '')

const labelOf = (id: string) => visibleText(available.value.find((m) => m.id === id)?.label ?? id).text

const options = computed<Option[]>(() => [
  {
    id: null,
    label: interpolate($gettext('Operator default (%{label})'), { label: labelOf(defaultId.value) }, true)
  },
  ...available.value.map((m) => ({
    id: m.id,
    label: interpolate(
      $gettext('%{label} (%{id})'),
      { label: visibleText(m.label).text, id: visibleText(m.id).text },
      true
    )
  }))
])

const selected = (task: Task) => options.value.find((o) => o.id === form.draft.models[task]) ?? options.value[0]

const choose = (task: Task, o: Option | null) => {
  form.draft.models[task] = o?.id ?? null
}
</script>
