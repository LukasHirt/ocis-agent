import { beforeEach } from 'vitest'
import { config } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'
import { createGettext } from 'vue3-gettext'
import { defineComponent, h, ref } from 'vue'

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
    disabled: { type: Boolean, default: false },
    appearance: { type: String, default: undefined },
    variation: { type: String, default: undefined }
  },
  emits: ['click'],
  setup(props, { slots, emit }) {
    return () =>
      h(
        'button',
        {
          type: props.type,
          disabled: props.disabled,
          'data-appearance': props.appearance,
          'data-variation': props.variation,
          onClick: (e: MouseEvent) => emit('click', e)
        },
        slots.default?.()
      )
  }
})

// OcIcon loads icons/<name>-<fillType>.svg (or <name>.svg for 'none');
// data-icon-file names the file it would load.
const OcIcon = defineComponent({
  name: 'OcIcon',
  props: { name: { type: String, default: '' }, fillType: { type: String, default: 'fill' } },
  setup(props) {
    return () =>
      h('span', {
        'data-stub': 'OcIcon',
        'data-icon': props.name,
        'data-icon-file': props.fillType === 'none' ? props.name : `${props.name}-${props.fillType}`,
        'aria-hidden': 'true'
      })
  }
})

let fieldId = 0
const nextId = () => `stub-field-${++fieldId}`

// Form stubs follow the design system's v-model contracts: oc-text-input,
// oc-checkbox and oc-select use modelValue, oc-switch uses checked. Like the
// real components, oc-text-input and oc-select do not turn off attribute
// inheritance: attributes and listeners land on the root AND on the inner
// control.
const OcTextInput = defineComponent({
  name: 'OcTextInput',
  props: {
    modelValue: { type: String, default: '' },
    label: { type: String, default: '' },
    type: { type: String, default: 'text' },
    disabled: { type: Boolean, default: false },
    errorMessage: { type: String, default: null }
  },
  emits: ['update:modelValue', 'change', 'enterKeyDown'],
  setup(props, { emit, attrs }) {
    const id = nextId()
    const messageId = `${id}-message`
    return () => {
      const { onChange, onInput, onFocus, class: _c, ...rest } = attrs
      return h('div', { 'data-stub': 'OcTextInput' }, [
        h('label', { for: id }, props.label),
        h('input', {
          ...rest,
          ...(props.errorMessage ? { 'aria-describedby': messageId } : {}),
          id,
          type: props.type,
          disabled: props.disabled,
          value: props.modelValue ?? '',
          'aria-invalid': props.errorMessage ? 'true' : 'false',
          onInput: (e: Event) => emit('update:modelValue', (e.target as HTMLInputElement).value),
          onChange: (e: Event) => emit('change', (e.target as HTMLInputElement).value),
          onKeydown: (e: KeyboardEvent) => {
            ;(rest.onKeydown as ((e: KeyboardEvent) => void) | undefined)?.(e)
            if (e.key === 'Enter') emit('enterKeyDown')
          }
        }),
        props.errorMessage
          ? h('div', { role: 'alert' }, [h('span', { id: messageId }, props.errorMessage)])
          : null
      ])
    }
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

// OcSwitch has no disabled prop: a disabled attribute lands on its root
// span and does nothing.
const OcSwitch = defineComponent({
  name: 'OcSwitch',
  props: {
    checked: { type: Boolean, default: false },
    label: { type: String, default: '' }
  },
  emits: ['update:checked'],
  setup(props, { emit }) {
    return () =>
      h('span', { 'data-stub': 'OcSwitch' }, [
        h('span', props.label),
        h('button', {
          type: 'button',
          role: 'switch',
          'aria-checked': String(props.checked),
          onClick: () => emit('update:checked', !props.checked)
        })
      ])
  }
})

const OcSelect = defineComponent({
  name: 'OcSelect',
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

// OcTextarea, like the real one, keeps attribute inheritance: attributes
// land on the root AND on the textarea. It emits change on Enter without
// Ctrl or Shift unless submitOnEnter is false, emits every keydown (so a
// keydown listener is not an attribute) and exposes focus().
const OcTextarea = defineComponent({
  name: 'OcTextarea',
  props: {
    modelValue: { type: String, default: null },
    label: { type: String, default: null },
    errorMessage: { type: String, default: null },
    descriptionMessage: { type: String, default: null },
    submitOnEnter: { type: Boolean, default: true }
  },
  emits: ['update:modelValue', 'change', 'keydown', 'focus'],
  setup(props, { emit, attrs, expose }) {
    const id = nextId()
    const el = ref<HTMLTextAreaElement | null>(null)
    expose({ focus: () => el.value?.focus() })
    return () => {
      const message = props.errorMessage || props.descriptionMessage
      return h('div', { 'data-stub': 'OcTextarea' }, [
        h('label', { for: id }, props.label ?? ''),
        h('textarea', {
          ...attrs,
          ...(message ? { 'aria-describedby': `${id}-message` } : {}),
          id,
          ref: el,
          value: props.modelValue,
          'aria-invalid': props.errorMessage ? 'true' : 'false',
          onInput: (e: Event) => emit('update:modelValue', (e.target as HTMLTextAreaElement).value),
          onFocus: () => emit('focus', true),
          onKeydown: (e: KeyboardEvent) => {
            if (props.submitOnEnter && e.key?.toLowerCase() === 'enter' && !e.ctrlKey && !e.shiftKey) {
              emit('change', (e.target as HTMLTextAreaElement).value)
            }
            emit('keydown', e)
          }
        }),
        message ? h('div', [h('span', { id: `${id}-message` }, message)]) : null
      ])
    }
  }
})

// RouterLink renders a link; data-to holds the target it was given (the
// host's router resolves it at runtime).
const RouterLink = defineComponent({
  name: 'RouterLink',
  props: { to: { type: [String, Object], required: true } },
  setup(props, { slots }) {
    return () =>
      h(
        'a',
        { href: typeof props.to === 'string' ? props.to : '#', 'data-to': JSON.stringify(props.to) },
        slots.default?.()
      )
  }
})

config.global.stubs = {
  'oc-textarea': OcTextarea,
  OcTextarea,
  'router-link': RouterLink,
  RouterLink,
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
