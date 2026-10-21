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

  it('adds a small chip next to a banner', () => {
    const { w } = mountWith(statusOf({ paused: true, pause_reason: 'dormant' }), 'polling')
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    const chip = w.find('.agent-connection')
    expect(chip.classes()).toEqual(expect.arrayContaining(['agent-chip', 'agent-chip--neutral']))
    expect(chip.text()).toContain('Live updates paused')
    expect(chip.attributes('title')).toBe('Live updates are unavailable; refreshing regularly.')
    expect(chip.find('[data-stub=OcIcon]').attributes('data-icon-file')).toBe('pause-circle-fill')
  })

  it('shows only the chip when polling and all is well', () => {
    const { w } = mountWith(statusOf(), 'polling')
    expect(w.find('.agent-banner').exists()).toBe(false)
    expect(w.find('.agent-connection').text()).toContain('Live updates paused')
    expect(w.text()).not.toContain('Live updates are unavailable')
  })

  it('shows a spinning reconnecting chip when offline', () => {
    const { w } = mountWith(statusOf(), 'offline')
    const chip = w.find('.agent-connection')
    expect(chip.text()).toContain('Reconnecting…')
    expect(chip.find('[data-stub=OcIcon]').classes()).toContain('agent-spin')
  })
})

describe('StatusBanner: outage and errors', () => {
  const outage = (over: { reconnecting?: boolean; statusError?: string | null; status?: Status | null } = {}) => {
    const r = mountWith(over.status === undefined ? statusOf() : over.status, 'offline')
    r.store.serviceDown = true
    r.store.reconnecting = over.reconnecting ?? false
    if (over.statusError !== undefined) r.store.errors.status = over.statusError
    return r
  }

  it('shows one unreachable banner with a retry, over the kill switch, and no chip', async () => {
    const { w, store } = outage({ status: statusOf({ kill_switch: { database: true, environment: false } }) })
    await w.vm.$nextTick()
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    const b = banner(w)
    expect(b.classes()).toEqual(
      expect.arrayContaining(['agent-banner-warning', 'agent-banner-unreachable'])
    )
    expect(b.text()).toContain("The agent service can't be reached.")
    expect(b.find('.agent-banner-sub').text()).toContain('Showing what was loaded before. Retrying automatically.')
    expect(b.find('[data-stub=OcSpinner]').exists()).toBe(false)
    expect(b.find('[data-stub=OcIcon]').attributes('data-icon-file')).toBe('wifi-off-fill')
    expect(w.find('.agent-connection').exists()).toBe(false)
    const retry = vi.spyOn(store, 'retryNow').mockResolvedValue()
    const button = b.find('button')
    expect(button.text()).toBe('Retry')
    await button.trigger('click')
    expect(retry).toHaveBeenCalledTimes(1)
  })

  it('says Reconnecting… with a spinner and disables Retry while a retry runs', async () => {
    const { w } = outage({ reconnecting: true })
    await w.vm.$nextTick()
    const b = banner(w)
    expect(b.find('.agent-banner-sub').text()).toContain('Reconnecting…')
    expect(b.find('[data-stub=OcSpinner]').exists()).toBe(true)
    expect(b.find('button').attributes('disabled')).toBeDefined()
  })

  it('does not repeat the status error text during an outage', async () => {
    const { w } = outage({ statusError: 'The agent service is not reachable right now. Try again later.' })
    await w.vm.$nextTick()
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    expect(w.text()).not.toContain('The agent service is not reachable right now')
    expect(w.text()).not.toContain('may be out of date')
  })

  it('shows another status error as the one danger banner, with Try again', async () => {
    const { w, store } = mountWith(statusOf())
    store.errors.status = 'Something went wrong in the agent service.'
    await w.vm.$nextTick()
    expect(w.findAll('.agent-banner')).toHaveLength(1)
    const b = banner(w)
    expect(b.classes()).toEqual(expect.arrayContaining(['agent-banner-danger', 'agent-banner-status_error']))
    expect(b.text()).toContain('Something went wrong in the agent service.')
    expect(b.find('.agent-banner-sub').text()).toBe('What is shown may be out of date.')
    const refresh = vi.spyOn(store, 'refreshStatus').mockResolvedValue()
    expect(b.find('button').text()).toBe('Try again')
    await b.find('button').trigger('click')
    expect(refresh).toHaveBeenCalledTimes(1)
  })

  it('shows no status-error banner without a status', () => {
    const { w, store } = mountWith(null)
    store.errors.status = 'Something went wrong in the agent service.'
    return w.vm.$nextTick().then(() => expect(w.find('.agent-banner').exists()).toBe(false))
  })
})
