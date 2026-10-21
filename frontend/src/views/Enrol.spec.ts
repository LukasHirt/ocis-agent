import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import Enrol from './Enrol.vue'
import { useAgentStore } from '../stores/agent'
import { statusOf } from '../test/fixtures'
import { ApiError } from '../api/errors'
import type { Status } from '../api/types'

const enableButton = (w: ReturnType<typeof mount>) =>
  w.findAll('button').find((b) => b.text() === 'Enable agent')!

describe('Enrol', () => {
  it('explains what the agent does and which access it uses', () => {
    const w = mount(Enrol)
    const text = w.text()
    expect(text).toContain('app token')
    expect(text).toContain('whatever you can')
    expect(text).toContain('personal space')
    expect(text).toContain('approve')
    expect(text).toContain('revoke')
    // the token is not limited to one space; only the agent is
    expect(text).not.toContain('full access to your own space')
    expect(text).not.toMatch(/never in other spaces/)
    expect(enableButton(w).exists()).toBe(true)
  })

  it('is a centred card with a list of what it can access and a primary button', () => {
    const w = mount(Enrol)
    expect(w.find('.agent-enrol-card').exists()).toBe(true)
    expect(w.find('h1').text()).toBe('Set up your agent')
    expect(w.findAll('.agent-enrol-list li')).toHaveLength(3)
    expect(enableButton(w).attributes('data-variation')).toBe('primary')
  })

  it('enrols, shows a spinner meanwhile and blocks a second click', async () => {
    const store = useAgentStore()
    let resolve!: (s: Status) => void
    const enrol = vi.spyOn(store, 'enrol').mockImplementation(() => new Promise<Status>((r) => (resolve = r)))
    const w = mount(Enrol)
    await enableButton(w).trigger('click')
    expect(w.find('[data-stub=OcSpinner]').exists()).toBe(true)
    expect(enableButton(w).attributes('disabled')).toBeDefined()
    await enableButton(w).trigger('click')
    expect(enrol).toHaveBeenCalledTimes(1)
    resolve(statusOf())
    await flushPromises()
    expect(w.find('[data-stub=OcSpinner]').exists()).toBe(false)
    expect(w.find('[role=alert]').exists()).toBe(false)
  })

  it('shows the fixed message of a failure, never the server text', async () => {
    const store = useAgentStore()
    vi.spyOn(store, 'enrol').mockRejectedValue(new ApiError(400, 'no_login_name'))
    const w = mount(Enrol)
    await enableButton(w).trigger('click')
    await flushPromises()
    expect(w.find('[role=alert]').text()).toBe('Your account has no login name, which the agent needs.')
    expect(enableButton(w).attributes('disabled')).toBeUndefined()
  })

  it('shows a generic message for an unexpected error', async () => {
    const store = useAgentStore()
    vi.spyOn(store, 'enrol').mockRejectedValue(new Error('secret detail'))
    const w = mount(Enrol)
    await enableButton(w).trigger('click')
    await flushPromises()
    expect(w.find('[role=alert]').text()).toBe('Something went wrong. Try again later.')
  })
})
