<template>
  <section class="agent-settings-section agent-settings-budget" aria-labelledby="agent-settings-budget-h">
    <h2 id="agent-settings-budget-h" v-text="$gettext('Daily token budget')" />
    <template v-if="budget">
      <p
        data-testid="agent-budget-usage"
        v-text="
          interpolate(
            $gettext('%{used} of %{total} tokens used today'),
            { used: number(budget.used_tokens), total: number(budget.daily_tokens) },
            true
          )
        "
      />
      <progress
        class="agent-settings-progress"
        :value="Math.min(budget.used_tokens, budget.daily_tokens)"
        :max="budget.daily_tokens"
        :aria-label="$gettext('Tokens used today')"
      />
      <p v-if="budget.resets_at" data-testid="agent-budget-reset" v-text="resetText" />
    </template>

    <oc-text-input
      v-model="tokens"
      data-testid="agent-daily-tokens"
      inputmode="numeric"
      :label="$gettext('Daily limit (tokens)')"
      :disabled="form.saving.value"
      :error-message="problem ? problemText : undefined"
    />
    <p
      class="agent-settings-hint"
      v-text="
        interpolate(
          $gettext('You can lower the limit set by the operator (%{max} tokens) but not raise it.'),
          { max: number(max) },
          true
        )
      "
    />
    <p v-if="form.draft.followOperator" class="agent-settings-hint" v-text="$gettext('Follows the operator\'s limit once saved.')" />
    <oc-button
      size="small"
      :disabled="form.saving.value || form.draft.followOperator"
      @click="follow"
      v-text="$gettext('Follow the operator\'s limit')"
    />
  </section>
</template>

<script setup lang="ts">
import { computed } from 'vue'
import { useGettext } from 'vue3-gettext'
import { useAgentStore } from '../../stores/agent'
import { formatTime } from '../../util/format'
import { utcTime } from '../../util/banner'
import { useSettingsForm } from './form'
import { MIN_DAILY_TOKENS } from './rules'

// BudgetSection: today's token usage and the user's own daily limit (only
// lower than the operator's cap; null follows the cap again).
const store = useAgentStore()
const form = useSettingsForm()
const { $gettext, interpolate } = useGettext()

const budget = computed(() => store.status?.budget ?? null)
const max = computed(() => form.base.value?.daily_tokens_max ?? 0)
const number = (n: number) => new Intl.NumberFormat().format(n)

const resetText = computed(() => {
  const at = budget.value?.resets_at ?? ''
  return interpolate(
    $gettext('It resets at %{utc} UTC (%{local} your time).'),
    { utc: utcTime(at), local: formatTime(at) },
    true
  )
})

const tokens = computed({
  get: () => form.draft.dailyTokens,
  set: (v: string) => {
    form.draft.dailyTokens = v
    form.draft.followOperator = false
  }
})

const follow = () => {
  form.draft.dailyTokens = String(max.value)
  form.draft.followOperator = true
}

const problem = computed(() => form.problems.value.dailyTokens)
const problemText = computed(() =>
  interpolate(
    $gettext('Enter a whole number between %{min} and %{max}.'),
    { min: number(MIN_DAILY_TOKENS), max: number(max.value) },
    true
  )
)
</script>
