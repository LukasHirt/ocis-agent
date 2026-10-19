import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import StatusBanner from './StatusBanner.vue'
import { useAgentStore } from '../stores/agent'
import { PLATFORM_KEY } from '../platform'
import { makePlatform } from '../test/platform'
import { statusOf } from '../test/fixtures'
import { ApiError } from '../api/errors'
import type { Status } from '../api/types'
import { formatTime } from '../util/format'

const mountWith = (status: Status | null, connection: 'streaming' | 'polling' | 'offline' = 'streaming') => {
  const store = useAgentStore()
  store.status = status
  store.connection = connection
  const platform = makePlatform()
  const push = vi.fn()
  const w = mount(StatusBanner, {
    global: { provide: { [PLATFORM_KEY as symbol]: platform }, mocks: { $router: { push } } }
  })
  return { w, store, platform, push }
}

const banner = (w: ReturnType<typeof mount>) => w.find('.agent-banner')

describe('StatusBanner', () => {
  it('renders nothing when all is well', () => {
    const { w } = mountWith(statusOf())
    expect(banner(w).exists()).toBe(false)
    expect(w.find('.agent-connection').exists()).toBe(false)
  })

  // icon files the design system ships (OcIcon loads <name>-<fillType>.svg)
  it.each([
    [statusOf({ kill_switch: { database: true, environment: false } }), 'stop-circle-fill'],
    [statusOf({ paused: true, pause_reason: 'credential_revoked' }), 'link-unlink'],
    [statusOf({ paused: true, pause_reason: 'token_anomaly' }), 'alarm-warning-fill'],
    [statusOf({ paused: true, pause_reason: 'user' }), 'pause-circle-fill']
  ])('uses an icon file that exists (%#)', (status, file) => {
    const { w } = mountWith(status)
    expect(banner(w).find('[data-stub="OcIcon"]').attributes('data-icon-file')).toBe(file)
  })

  it('renders nothing without a status', () => {
    const { w } = mountWith(null)
    expect(banner(w).exists()).toBe(false)
  })

  it('shows the kill switch with an icon and role status, over a pause', () => {
    const { w } = mountWith(
      statusOf({ kill_switch: { database: false, environment: true }, paused: true, pause_reason: 'user' })
    )
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    expect(banner(w).attributes('role')).toBe('status')
    expect(banner(w).text()).toContain('The agent is stopped by the operator')
    expect(banner(w).find('[data-stub=OcIcon]').exists()).toBe(true)
    expect(banner(w).find('button').exists()).toBe(false)
  })

  it('offers Resume for a user pause', async () => {
    const { w, store } = mountWith(statusOf({ paused: true, pause_reason: 'user' }))
    const resume = vi.spyOn(store, 'resume').mockResolvedValue(statusOf())
    expect(banner(w).text()).toContain('You paused the agent')
    await banner(w).find('button').trigger('click')
    expect(resume).toHaveBeenCalledWith('user')
  })

  it('shows a fixed message when resuming fails', async () => {
    const { w, store, platform } = mountWith(statusOf({ paused: true, pause_reason: 'user' }))
    vi.spyOn(store, 'resume').mockRejectedValue(new ApiError(429, 'rate_limited'))
    await banner(w).find('button').trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith('error', 'Too many requests. Wait a moment and try again.')
  })

  it('is informational for a dormant pause', () => {
    const { w } = mountWith(statusOf({ paused: true, pause_reason: 'dormant' }))
    expect(banner(w).text()).toContain('Paused because you were away; it resumes when you visit')
    expect(banner(w).find('button').exists()).toBe(false)
  })

  it('sends a token anomaly to the settings, without a resume button', async () => {
    const { w, push } = mountWith(statusOf({ paused: true, pause_reason: 'token_anomaly' }))
    expect(banner(w).text()).toContain('Possible token misuse: check your app tokens in Settings')
    const buttons = banner(w).findAll('button')
    expect(buttons).toHaveLength(1)
    expect(buttons[0].text()).not.toMatch(/resume/i)
    await buttons[0].trigger('click')
    expect(push).toHaveBeenCalledWith({ name: 'agent-settings' })
  })

  it.each(['credential_invalid', 'credential_revoked', 'credential_expired'])(
    'offers Reconnect for %s',
    async (reason) => {
      const { w, store } = mountWith(statusOf({ paused: true, pause_reason: reason }))
      const enrol = vi.spyOn(store, 'enrol').mockResolvedValue(statusOf())
      expect(banner(w).text()).toContain('Reconnect needed')
      const button = banner(w).find('button')
      expect(button.text()).toBe('Reconnect')
      await button.trigger('click')
      expect(enrol).toHaveBeenCalledTimes(1)
    }
  )

  it('shows an unknown reason generically and as text', () => {
    const { w } = mountWith(statusOf({ paused: true, pause_reason: '<img src=x onerror=alert(1)>' }))
    expect(banner(w).text()).toContain('The agent is paused')
    expect(w.find('img').exists()).toBe(false)
  })

  it('shows the budget with the UTC reset time', () => {
    const { w } = mountWith(
      statusOf({ budget: { used_tokens: 5, daily_tokens: 5, resets_at: '2026-01-02T00:00:00Z' } })
    )
    const text = banner(w).text()
    expect(text).toContain('Daily token budget used up; the agent continues tomorrow at 00:00 UTC')
    expect(text).toContain(`(${formatTime('2026-01-02T00:00:00Z')} your time)`)
  })

  it('shows the budget without a reset time', () => {
    const { w } = mountWith(statusOf({ budget: { used_tokens: 5, daily_tokens: 5, resets_at: null } }))
    expect(banner(w).text()).toContain('Daily token budget used up; the agent continues tomorrow')
  })

  it('adds a small connection note next to a banner', () => {
    const { w } = mountWith(statusOf({ paused: true, pause_reason: 'dormant' }), 'polling')
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    expect(w.find('.agent-connection').text()).toContain('Live updates are unavailable')
  })

  it('notes an offline connection', () => {
    const { w } = mountWith(statusOf(), 'offline')
    expect(w.find('.agent-connection').text()).toContain('Connection lost')
  })
})
