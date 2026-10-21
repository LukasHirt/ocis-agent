import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import LoadingState from './LoadingState.vue'

describe('LoadingState', () => {
  it('has a labelled spinner and three hidden skeleton rows by default', () => {
    const w = mount(LoadingState, { props: { label: 'Loading the tasks' } })
    expect(w.find('[data-stub=OcSpinner]').attributes('aria-label')).toBe('Loading the tasks')
    expect(w.classes()).toContain('agent-state-loading--rows')
    expect(w.find('.agent-skeleton').attributes('aria-hidden')).toBe('true')
    expect(w.findAll('.agent-skeleton-row')).toHaveLength(3)
    expect(w.text()).toBe('')
  })

  it('renders count cards or bubbles', () => {
    const cards = mount(LoadingState, { props: { label: 'L', variant: 'cards', count: 2 } })
    expect(cards.findAll('.agent-skeleton-card')).toHaveLength(2)
    const bubbles = mount(LoadingState, { props: { label: 'L', variant: 'bubbles', count: 3 } })
    expect(bubbles.findAll('.agent-skeleton-bubble')).toHaveLength(3)
    expect(bubbles.findAll('.agent-skeleton-bubble--end')).toHaveLength(1)
  })

  it('has no skeleton for the spinner variant', () => {
    const w = mount(LoadingState, { props: { label: 'L', variant: 'spinner' } })
    expect(w.find('.agent-skeleton').exists()).toBe(false)
    expect(w.find('[data-stub=OcSpinner]').exists()).toBe(true)
  })
})
