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

let fieldId = 0
const nextId = () => `stub-field-${++fieldId}`

// Form stubs follow the design system's v-model contracts: oc-text-input,
// oc-checkbox and oc-select use modelValue, oc-switch uses checked.
const OcTextInput = defineComponent({
  name: 'OcTextInput',
  inheritAttrs: false,
  props: {
    modelValue: { type: String, default: '' },
    label: { type: String, default: '' },
    type: { type: String, default: 'text' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['update:modelValue', 'change'],
  setup(props, { emit, attrs }) {
    const id = nextId()
    return () =>
      h('div', { 'data-stub': 'OcTextInput' }, [
        h('label', { for: id }, props.label),
        h('input', {
          ...attrs,
          id,
          type: props.type,
          disabled: props.disabled,
          value: props.modelValue ?? '',
          onInput: (e: Event) => emit('update:modelValue', (e.target as HTMLInputElement).value),
          onChange: (e: Event) => emit('change', (e.target as HTMLInputElement).value)
        })
      ])
  }
})

const OcCheckbox = defineComponent({
  name: 'OcCheckbox',
  inheritAttrs: false,
  props: {
    modelValue: { type: Boolean, default: false },
    label: { type: String, default: '' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['update:modelValue'],
  setup(props, { emit, attrs }) {
    const id = nextId()
    return () =>
      h('span', { 'data-stub': 'OcCheckbox' }, [
        h('input', {
          ...attrs,
          id,
          type: 'checkbox',
          disabled: props.disabled,
          checked: props.modelValue,
          onChange: (e: Event) => emit('update:modelValue', (e.target as HTMLInputElement).checked)
        }),
        h('label', { for: id }, props.label)
      ])
  }
})

const OcSwitch = defineComponent({
  name: 'OcSwitch',
  props: {
    checked: { type: Boolean, default: false },
    label: { type: String, default: '' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['update:checked'],
  setup(props, { emit }) {
    return () =>
      h(
        'button',
        {
          'data-stub': 'OcSwitch',
          type: 'button',
          role: 'switch',
          'aria-checked': String(props.checked),
          disabled: props.disabled,
          onClick: () => emit('update:checked', !props.checked)
        },
        props.label
      )
  }
})

const OcSelect = defineComponent({
  name: 'OcSelect',
  inheritAttrs: false,
  props: {
    modelValue: { type: null, default: undefined },
    options: { type: Array, default: () => [] },
    optionLabel: { type: String, default: 'label' },
    label: { type: String, default: '' },
    disabled: { type: Boolean, default: false }
  },
  emits: ['update:modelValue'],
  setup(props, { emit, attrs }) {
    const id = nextId()
    const labelOf = (o: unknown) =>
      o !== null && typeof o === 'object' ? String((o as Record<string, unknown>)[props.optionLabel]) : String(o)
    return () =>
      h('div', { 'data-stub': 'OcSelect' }, [
        h('label', { for: id }, props.label),
        h(
          'select',
          {
            ...attrs,
            id,
            disabled: props.disabled,
            value: String(props.options.indexOf(props.modelValue)),
            onChange: (e: Event) =>
              emit('update:modelValue', props.options[Number((e.target as HTMLSelectElement).value)])
          },
          props.options.map((o, i) => h('option', { value: String(i) }, labelOf(o)))
        )
      ])
  }
})

config.global.stubs = {
  'oc-button': OcButton,
  OcButton,
  'oc-icon': OcIcon,
  OcIcon,
  'oc-spinner': stub('OcSpinner', 'span'),
  'oc-text-input': OcTextInput,
  OcTextInput,
  'oc-select': OcSelect,
  OcSelect,
  'oc-checkbox': OcCheckbox,
  OcCheckbox,
  'oc-switch': OcSwitch,
  OcSwitch,
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
