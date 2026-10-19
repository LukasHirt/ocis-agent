<template>
  <section class="agent-settings-section agent-settings-policy" aria-labelledby="agent-settings-policy-h">
    <h2 id="agent-settings-policy-h" v-text="$gettext('Proactive work')" />
    <oc-switch
      data-testid="agent-proactive"
      :checked="form.draft.proactive_enabled"
      :label="$gettext('Let the agent work on its own on the watched folders')"
      @update:checked="setProactive"
    />

    <h3 v-text="$gettext('Watched folders')" />
    <p v-if="form.draft.watch.length === 0" v-text="$gettext('No folders are watched.')" />
    <ul v-else class="agent-settings-watch">
      <li v-for="(p, i) in form.draft.watch" :key="i + ':' + p" data-testid="agent-watch-item">
        <TextBlock :text="p" mono />
        <oc-button
          size="small"
          :aria-label="removeLabel(p)"
          :disabled="form.saving.value"
          @click="remove(i)"
          v-text="$gettext('Remove')"
        />
      </li>
    </ul>
    <p v-if="full" class="agent-settings-hint" v-text="fullText" />
    <div class="agent-settings-add">
      <oc-text-input
        v-model="newPath"
        data-testid="agent-watch-new"
        :label="$gettext('Folder in your personal space, e.g. /Projects')"
        :disabled="form.saving.value || full"
        :error-message="watchError ?? undefined"
        @enter-key-down="add"
      />
      <oc-button :disabled="form.saving.value || full" @click="add" v-text="$gettext('Add')" />
    </div>

    <h3 v-text="$gettext('Schedule')" />
    <oc-select
      :model-value="sweepSelected"
      :options="sweepOptions"
      option-label="label"
      :clearable="false"
      :searchable="false"
      data-testid="agent-sweep"
      :label="$gettext('Look for changes')"
      :disabled="form.saving.value"
      @update:model-value="(o: SweepOption | null) => o && (form.draft.sweepSeconds = o.seconds)"
    />
    <oc-text-input
      v-model="form.draft.quiet_hours"
      data-testid="agent-quiet-hours"
      placeholder="22:00-07:00"
      :label="$gettext('Quiet hours (HH:MM-HH:MM, empty for none)')"
      :disabled="form.saving.value"
      :error-message="
        form.problems.value.quiet_hours
          ? $gettext('Use HH:MM-HH:MM in 24-hour time with two different times, or leave it empty.')
          : undefined
      "
    />
  </section>
</template>

<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from '../../components/TextBlock.vue'
import { formatDuration } from '../../util/format'
import { visibleText } from '../../util/visibleText'
import { useSettingsForm } from './form'
import { MAX_WATCH, SWEEP_PRESETS, cleanWatchPath, parseGoDuration, watchPathProblem } from './rules'

interface SweepOption {
  seconds: number
  label: string
}

// PolicySection: proactive work, the watched folders, the sweep interval
// and quiet hours. The checks mirror the server's; the server decides.
const form = useSettingsForm()
const { $gettext, interpolate } = useGettext()

const newPath = ref('')
const watchError = ref<string | null>(null)
watch(newPath, () => {
  watchError.value = null
})

const full = computed(() => form.draft.watch.length >= MAX_WATCH)
const fullText = computed(() =>
  interpolate($gettext('At most %{n} folders can be watched.'), { n: MAX_WATCH }, true)
)

const removeLabel = (p: string) => interpolate($gettext('Remove %{path}'), { path: visibleText(p).text }, true)

const problemText = (p: string): string | null => {
  switch (watchPathProblem(p)) {
    case 'empty':
      return $gettext('Enter a folder.')
    case 'relative':
      return $gettext('A folder must start with /.')
    case 'too_long':
      return $gettext('The folder path is too long.')
    case 'control':
      return $gettext('The folder path contains control characters.')
    case 'dot_segment':
      return $gettext('A folder path must not contain . or .. parts.')
    case 'reserved':
      return $gettext("The agent's own folder cannot be watched.")
  }
  return null
}

const add = () => {
  if (form.saving.value) return
  if (full.value) {
    watchError.value = fullText.value
    return
  }
  const p = newPath.value
  const problem = problemText(p)
  if (problem) {
    watchError.value = problem
    return
  }
  const clean = cleanWatchPath(p)
  if (form.draft.watch.includes(clean)) {
    watchError.value = $gettext('This folder is already in the list.')
    return
  }
  form.draft.watch.push(clean)
  newPath.value = ''
  watchError.value = null
}

// OcSwitch cannot be disabled: a toggle while saving is ignored (the form
// is rebuilt from the server's answer afterwards).
const setProactive = (v: boolean) => {
  if (!form.saving.value) form.draft.proactive_enabled = v
}

const remove = (i: number) => {
  form.draft.watch.splice(i, 1)
}

const presetLabel = (seconds: number): string => {
  switch (seconds) {
    case 300:
      return $gettext('Every 5 minutes')
    case 900:
      return $gettext('Every 15 minutes')
    case 3600:
      return $gettext('Every hour')
    case 21600:
      return $gettext('Every 6 hours')
    case 86400:
      return $gettext('Every day')
    case 604800:
      return $gettext('Every 7 days')
  }
  return ''
}

const sweepOptions = computed<SweepOption[]>(() => {
  const opts = SWEEP_PRESETS.map((p) => ({ seconds: p.seconds, label: presetLabel(p.seconds) }))
  const stored = form.base.value ? parseGoDuration(form.base.value.sweep_interval) : null
  if (stored !== null && !SWEEP_PRESETS.some((p) => p.seconds === stored)) {
    opts.unshift({
      seconds: stored,
      label: interpolate($gettext('Every %{interval} (current)'), { interval: formatDuration(stored * 1000) }, true)
    })
  }
  return opts
})

const sweepSelected = computed(() => sweepOptions.value.find((o) => o.seconds === form.draft.sweepSeconds) ?? null)
</script>
