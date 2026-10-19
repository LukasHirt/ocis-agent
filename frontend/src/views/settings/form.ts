import { inject, provide, type ComputedRef, type InjectionKey, type Ref } from 'vue'
import type { Settings } from '../../api/types'
import type { Draft, FormProblems } from './rules'

// The settings form shared by the page and its form sections: the settings
// the draft was made from, the draft itself (edited in place) and its
// problems. The page owns saving.
export interface SettingsForm {
  base: Readonly<Ref<Settings | null>>
  draft: Draft
  problems: ComputedRef<FormProblems>
  saving: Readonly<Ref<boolean>>
}

const KEY: InjectionKey<SettingsForm> = Symbol('ocis-agent-settings-form')

export const provideSettingsForm = (form: SettingsForm) => provide(KEY, form)

export const useSettingsForm = (): SettingsForm => {
  const form = inject(KEY, null)
  if (!form) throw new Error('ocis-agent: settings form section used outside the settings page')
  return form
}
