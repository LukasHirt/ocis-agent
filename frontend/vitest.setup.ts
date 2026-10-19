import { beforeEach } from 'vitest'
import { config } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createGettext } from 'vue3-gettext'
import { defineComponent, h } from 'vue'

export { makePlatform } from './src/test/platform'

// The oCIS design system registers the oc-* components globally at runtime.
// Tests replace them with minimal stubs that forward attributes and slots.
const stub = (name: string, tag = 'div') =>
  defineComponent({
    name,
    inheritAttrs: true,
    setup(_, { slots }) {
      return () => h(tag, { 'data-stub': name }, slots.default?.())
    }
  })

const OcButton = defineComponent({
  name: 'OcButton',
  props: {
    type: { type: String, default: 'button' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['click'],
  setup(props, { slots, emit }) {
    return () =>
      h(
        'button',
        {
          type: props.type,
          disabled: props.disabled,
          onClick: (e: MouseEvent) => emit('click', e)
        },
        slots.default?.()
      )
  }
})

const OcIcon = defineComponent({
  name: 'OcIcon',
  props: { name: { type: String, default: '' } },
  setup(props) {
    return () => h('span', { 'data-stub': 'OcIcon', 'data-icon': props.name, 'aria-hidden': 'true' })
  }
})

config.global.stubs = {
  'oc-button': OcButton,
  OcButton,
  'oc-icon': OcIcon,
  OcIcon,
  'oc-spinner': stub('OcSpinner', 'span'),
  'oc-text-input': stub('OcTextInput'),
  'oc-select': stub('OcSelect'),
  'oc-checkbox': stub('OcCheckbox'),
  'oc-switch': stub('OcSwitch'),
  'oc-table-simple': stub('OcTableSimple', 'table'),
  'oc-tag': stub('OcTag', 'span'),
  'oc-modal': stub('OcModal'),
  'oc-notification-message': stub('OcNotificationMessage')
}

beforeEach(() => {
  const pinia = createPinia()
  setActivePinia(pinia)
  config.global.plugins = [
    createGettext({ availableLanguages: { en: 'English' }, defaultLanguage: 'en', translations: {} }),
    pinia
  ]
})
