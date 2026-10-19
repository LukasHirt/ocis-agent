import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import RiskBadge from './RiskBadge.vue'

describe('RiskBadge', () => {
  it.each([
    ['read', 'Read', 'agent-risk-read'],
    ['write', 'Write', 'agent-risk-write'],
    ['destructive', 'Destructive', 'agent-risk-destructive'],
    ['outward', 'Outward', 'agent-risk-outward']
  ])('shows %s with a text label, an icon and a class', (risk, label, cls) => {
    const w = mount(RiskBadge, { props: { risk } })
    expect(w.classes()).toContain('agent-risk')
    expect(w.classes()).toContain(cls)
    expect(w.text()).toContain(label)
    const icon = w.find('[data-stub="OcIcon"]')
    expect(icon.exists()).toBe(true)
    expect(icon.attributes('data-icon')).not.toBe('')
  })

  it('shows an unknown risk as its raw text, marked as unknown', () => {
    const w = mount(RiskBadge, { props: { risk: '<b>weird</b>\u202E' } })
    expect(w.classes()).toContain('agent-risk-unknown')
    expect(w.text()).toContain('Unknown')
    expect(w.text()).toContain('<b>weird</b>\\u{202E}')
    expect(w.find('b').exists()).toBe(false)
    expect(w.find('[data-stub="OcIcon"]').exists()).toBe(true)
  })

  it('does not treat inherited object keys as known risks', () => {
    const w = mount(RiskBadge, { props: { risk: 'constructor' } })
    expect(w.classes()).toContain('agent-risk-unknown')
  })
})
