import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import ErrorState from './ErrorState.vue'

describe('ErrorState', () => {
  it('is an alert with the default icon, the title and a Try again button', async () => {
    const w = mount(ErrorState, { props: { title: 'Broken' } })
    expect(w.attributes('role')).toBe('alert')
    expect(w.classes()).toContain('agent-state--error')
    expect(w.classes()).not.toContain('agent-state--compact')
    expect(w.find('[data-stub=OcIcon]').attributes('data-icon')).toBe('error-warning')
    expect(w.find('.agent-state-title').text()).toBe('Broken')
    expect(w.find('.agent-state-text').exists()).toBe(false)
    await w.find('button').trigger('click')
    expect(w.emitted('retry')).toHaveLength(1)
    expect(w.find('button').text()).toBe('Try again')
  })

  it('takes an icon, a text and the compact look; class falls through', () => {
    const w = mount(ErrorState, {
      props: { title: 'T', text: 'More', icon: 'wifi-off', compact: true },
      attrs: { class: 'agent-tasks-error' }
    })
    expect(w.find('[data-stub=OcIcon]').attributes('data-icon')).toBe('wifi-off')
    expect(w.find('.agent-state-text').text()).toBe('More')
    expect(w.classes()).toEqual(expect.arrayContaining(['agent-state--compact', 'agent-tasks-error']))
  })

  it('disables the button and shows a labelled spinner while busy', () => {
    const w = mount(ErrorState, { props: { title: 'T', busy: true } })
    expect(w.find('button').attributes('disabled')).toBeDefined()
    expect(w.find('[data-stub=OcSpinner]').attributes('aria-label')).toBe('Trying again')
    const idle = mount(ErrorState, { props: { title: 'T' } })
    expect(idle.find('button').attributes('disabled')).toBeUndefined()
    expect(idle.find('[data-stub=OcSpinner]').exists()).toBe(false)
  })

  it('renders hostile text as text', () => {
    const w = mount(ErrorState, { props: { title: '<img src=x onerror=1>' } })
    expect(w.find('img').exists()).toBe(false)
  })
})
