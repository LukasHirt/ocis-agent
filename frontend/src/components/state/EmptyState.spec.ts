import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import EmptyState from './EmptyState.vue'

describe('EmptyState', () => {
  it('shows the icon and the title; the text equals the title without a text', () => {
    const w = mount(EmptyState, { props: { icon: 'task', title: 'No tasks yet.' } })
    expect(w.classes()).toContain('agent-state--empty')
    expect(w.find('[data-stub=OcIcon]').attributes('data-icon')).toBe('task')
    expect(w.find('.agent-state-icon').attributes('aria-hidden')).toBe('true')
    expect(w.text()).toBe('No tasks yet.')
    expect(w.find('.agent-state-text').exists()).toBe(false)
    expect(w.find('.agent-state-actions').exists()).toBe(false)
  })

  it('shows the text and the actions slot', () => {
    const w = mount(EmptyState, {
      props: { icon: 'task', title: 'T', text: 'More' },
      slots: { default: '<button class="act">Go</button>' }
    })
    expect(w.find('.agent-state-text').text()).toBe('More')
    expect(w.find('.agent-state-actions .act').exists()).toBe(true)
  })

  it('lets a class fall through and renders markup as text', () => {
    const w = mount(EmptyState, { props: { icon: 'task', title: '<b>x</b>' }, attrs: { class: 'agent-tasks-empty' } })
    expect(w.classes()).toContain('agent-tasks-empty')
    expect(w.find('b').exists()).toBe(false)
    expect(w.find('.agent-state-title').text()).toBe('<b>x</b>')
  })
})
