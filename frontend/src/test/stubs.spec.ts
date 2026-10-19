import { describe, expect, it } from 'vitest'
import { defineComponent, ref } from 'vue'
import { mount } from '@vue/test-utils'

// The oc-* stubs registered in vitest.setup.ts must support v-model, so
// form components can be tested through them.
describe('oc-* test stubs', () => {
  it('oc-text-input binds v-model and shows its label', async () => {
    const C = defineComponent({
      setup: () => ({ v: ref('a') }),
      template: '<oc-text-input v-model="v" label="Name" />'
    })
    const w = mount(C)
    const input = w.find('input')
    expect((input.element as HTMLInputElement).value).toBe('a')
    expect(w.find('label').text()).toBe('Name')
    await input.setValue('b')
    expect((w.vm as unknown as { v: string }).v).toBe('b')
  })

  it('oc-checkbox binds v-model', async () => {
    const C = defineComponent({
      setup: () => ({ v: ref(false) }),
      template: '<oc-checkbox v-model="v" label="On" />'
    })
    const w = mount(C)
    await w.find('input[type=checkbox]').setValue(true)
    expect((w.vm as unknown as { v: boolean }).v).toBe(true)
  })

  it('oc-switch binds v-model:checked', async () => {
    const C = defineComponent({
      setup: () => ({ v: ref(false) }),
      template: '<oc-switch v-model:checked="v" label="Paused" />'
    })
    const w = mount(C)
    expect(w.find('[role=switch]').attributes('aria-checked')).toBe('false')
    await w.find('[role=switch]').trigger('click')
    expect((w.vm as unknown as { v: boolean }).v).toBe(true)
    expect(w.find('[role=switch]').attributes('aria-checked')).toBe('true')
  })

  it('oc-select binds v-model to the chosen option', async () => {
    const options = [
      { label: 'One', id: 1 },
      { label: 'Two', id: 2 }
    ]
    const C = defineComponent({
      setup: () => ({ v: ref<unknown>(options[0]), options }),
      template: '<oc-select v-model="v" :options="options" label="Pick" />'
    })
    const w = mount(C)
    const select = w.find('select')
    expect(select.findAll('option').map((o) => o.text())).toEqual(['One', 'Two'])
    await select.setValue('1')
    expect((w.vm as unknown as { v: unknown }).v).toEqual(options[1])
  })

  it('oc-select uses optionLabel and plain string options', async () => {
    const C = defineComponent({
      setup: () => ({ v: ref<unknown>('x'), objs: [{ name: 'N' }], strs: ['x', 'y'] }),
      template:
        '<div><oc-select :options="objs" option-label="name" label="A" /><oc-select v-model="v" :options="strs" label="B" /></div>'
    })
    const w = mount(C)
    const [a, b] = w.findAll('select')
    expect(a.find('option').text()).toBe('N')
    await b.setValue('1')
    expect((w.vm as unknown as { v: unknown }).v).toBe('y')
  })
})
