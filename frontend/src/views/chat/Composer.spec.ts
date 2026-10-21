import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import Composer from './Composer.vue'

const mountComposer = (props: Record<string, unknown> = {}) =>
  mount(Composer, { props: { modelValue: '', ...props } as never, attachTo: document.body })

describe('Composer', () => {
  it('has a labelled textarea that documents the keys', () => {
    const w = mountComposer()
    const ta = w.find('textarea')
    const label = w.find(`label[for="${ta.attributes('id')}"]`)
    expect(label.text()).toBe('Message')
    expect(ta.attributes('placeholder')).toBe('Ask the agent…')
    expect(w.find('.agent-composer-keys').text()).toBe('Enter sends, Shift+Enter starts a new line')
    expect(w.find('.agent-composer-box textarea').exists()).toBe(true)
    w.unmount()
  })

  it('grows from 2 to 8 rows with the lines typed', async () => {
    const w = mountComposer()
    const rows = () => w.find('textarea').attributes('rows')
    expect(rows()).toBe('2')
    await w.setProps({ modelValue: 'a\nb\nc\nd' })
    expect(rows()).toBe('4')
    await w.setProps({ modelValue: Array(20).fill('x').join('\n') })
    expect(rows()).toBe('8')
    w.unmount()
  })

  it('sends on Enter and not on Shift+Enter', async () => {
    const w = mountComposer({ modelValue: 'hello' })
    const ta = w.find('textarea')
    const shift = new KeyboardEvent('keydown', { key: 'Enter', shiftKey: true, cancelable: true })
    ta.element.dispatchEvent(shift)
    expect(w.emitted('send')).toBeUndefined()
    expect(shift.defaultPrevented).toBe(false) // a new line
    const enter = new KeyboardEvent('keydown', { key: 'Enter', cancelable: true })
    ta.element.dispatchEvent(enter)
    expect(w.emitted('send')).toHaveLength(1)
    expect(enter.defaultPrevented).toBe(true)
    w.unmount()
  })

  it('does not send on Enter during an IME composition or while sending', async () => {
    const w = mountComposer({ modelValue: 'hello' })
    const ta = w.find('textarea')
    ta.element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', isComposing: true, cancelable: true }))
    expect(w.emitted('send')).toBeUndefined()
    // Safari commits a composition with an Enter of keyCode 229 and isComposing false
    const commit = new KeyboardEvent('keydown', { key: 'Enter', cancelable: true })
    Object.defineProperty(commit, 'keyCode', { value: 229 })
    ta.element.dispatchEvent(commit)
    expect(w.emitted('send')).toBeUndefined()
    expect(commit.defaultPrevented).toBe(false)
    await w.setProps({ sending: true })
    ta.element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }))
    expect(w.emitted('send')).toBeUndefined()
    w.unmount()
  })

  it('sends with the button and emits typed text', async () => {
    const w = mountComposer({ modelValue: 'x' })
    await w.find('textarea').setValue('typed')
    expect(w.emitted('update:modelValue')?.at(-1)).toEqual(['typed'])
    await w.find('button.agent-composer-send').trigger('click')
    expect(w.emitted('send')).toHaveLength(1)
    w.unmount()
  })

  it('disables input and button while sending', () => {
    const w = mountComposer({ modelValue: 'x', sending: true })
    expect(w.find('textarea').attributes('disabled')).toBeDefined()
    expect(w.find('button.agent-composer-send').attributes('disabled')).toBeDefined()
    w.unmount()
  })

  it('is disabled with a hint when the agent is not set up', () => {
    const w = mountComposer({ modelValue: 'x', disabled: true, hint: 'Set up the agent to ask questions.' })
    expect(w.find('textarea').attributes('disabled')).toBeDefined()
    expect(w.find('button.agent-composer-send').attributes('disabled')).toBeDefined()
    expect(w.find('.agent-composer-hint').text()).toBe('Set up the agent to ask questions.')
    w.unmount()
  })

  it('shows the byte count only near the limit, counting bytes of multi-byte text', async () => {
    const w = mountComposer({ modelValue: 'short' })
    expect(w.find('.agent-composer-count').exists()).toBe(false)
    await w.setProps({ modelValue: '€'.repeat(2500) }) // 7500 bytes
    expect(w.find('.agent-composer-count').text()).toBe('7500 / 8192 bytes')
    await w.setProps({ modelValue: '€'.repeat(2731) }) // 8193 bytes
    expect(w.find('.agent-composer-count').text()).toBe('8193 / 8192 bytes')
    expect(w.find('.agent-composer-count').classes()).toContain('agent-composer-over')
    expect(w.find('.agent-composer-over-text').text()).toBe('Too long to send.')
    expect(w.find('button.agent-composer-send').attributes('disabled')).toBeDefined()
    w.unmount()
  })

  it('shows the send error with its retry hint', () => {
    const w = mountComposer({
      modelValue: 'x',
      error: { code: 'limit_reached', message: "You have reached today's limit of questions. You can ask again in 2h.", retryAfter: 7200 }
    })
    expect(w.find('[role=alert]').text()).toBe("You have reached today's limit of questions. You can ask again in 2h.")
    w.unmount()
  })

  it('can be focused by its parent', () => {
    const w = mountComposer()
    ;(w.vm as unknown as { focus: () => void }).focus()
    expect(document.activeElement).toBe(w.find('textarea').element)
    w.unmount()
  })
})
