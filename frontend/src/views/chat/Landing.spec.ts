import { afterEach, describe, expect, it } from 'vitest'
import { mount, type VueWrapper } from '@vue/test-utils'
import { nextTick } from 'vue'
import Landing from './Landing.vue'

let wrapper: VueWrapper | undefined
let outside: HTMLElement | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  outside?.remove()
  outside = undefined
})

const mountLanding = (props: Record<string, unknown> = {}, host?: HTMLElement) => {
  wrapper = mount(Landing, {
    props: { name: '', modelValue: '', sending: false, disabled: false, hint: '', error: null, ...props } as never,
    attachTo: host ?? document.body
  })
  return wrapper
}

describe('Landing', () => {
  it('greets by name or plainly', () => {
    expect(mountLanding({ name: 'Albert Einstein' }).find('.agent-landing-title').text()).toBe(
      'Hello, Albert Einstein. How can I help?'
    )
    wrapper!.unmount()
    const w = mountLanding()
    expect(w.find('.agent-landing-title').text()).toBe('How can I help?')
    expect(w.find('section').attributes('aria-labelledby')).toBe(w.find('.agent-landing-title').attributes('id'))
  })

  it('renders a hostile name as plain text and cuts it', () => {
    const w = mountLanding({ name: '<img src=x onerror=alert(1)>‮Bob' })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('.agent-landing-title').text()).toContain('<img src=x onerror=alert(1)>\\u{202E}Bob')
    wrapper!.unmount()
    const long = mountLanding({ name: 'x'.repeat(200) }).find('.agent-landing-title').text()
    expect(long).toBe(`Hello, ${'x'.repeat(60)}…. How can I help?`)
  })

  it('fills the composer from an example and never sends', async () => {
    const w = mountLanding({ modelValue: 'old' })
    const buttons = w.findAll('.agent-landing-examples button')
    expect(buttons.map((b) => b.text())).toEqual([
      'What can you do?',
      'Which files in my space changed this week?',
      'Create a text file with a short to-do list'
    ])
    await buttons[1].trigger('click')
    expect(w.emitted('update:modelValue')!.at(-1)).toEqual(['Which files in my space changed this week?'])
    expect(w.emitted('send')).toBeUndefined()
  })

  it('emits send from the composer and shows its error', async () => {
    const w = mountLanding({ modelValue: 'hi', error: { code: 'x', message: 'Nope.' } })
    expect(w.find('.agent-composer-error').text()).toBe('Nope.')
    w.find('textarea').element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }))
    expect(w.emitted('send')).toHaveLength(1)
  })

  it('hides the examples and disables the composer when disabled', () => {
    const w = mountLanding({ disabled: true, hint: 'Set up the agent to ask it questions.' })
    expect(w.find('.agent-landing-examples').exists()).toBe(false)
    expect(w.find('textarea').attributes('disabled')).toBeDefined()
    expect(w.find('.agent-composer-hint').text()).toBe('Set up the agent to ask it questions.')
  })

  it('focuses the composer when nothing else has focus', async () => {
    const w = mountLanding()
    await nextTick()
    await nextTick()
    expect(document.activeElement).toBe(w.find('textarea').element)
  })

  it('keeps the focus of a host control outside the extension', async () => {
    outside = document.createElement('input')
    document.body.appendChild(outside)
    outside.focus()
    const w = mountLanding()
    await nextTick()
    await nextTick()
    expect(document.activeElement).toBe(outside)
    expect(w.find('textarea').element).not.toBe(document.activeElement)
  })

  it('takes the focus from a control inside the extension', async () => {
    const root = document.createElement('div')
    root.className = 'agent-layout'
    const btn = document.createElement('button')
    root.appendChild(btn)
    document.body.appendChild(root)
    btn.focus()
    const w = mountLanding({}, root)
    await nextTick()
    await nextTick()
    expect(document.activeElement).toBe(w.find('textarea').element)
    root.remove()
  })
})
