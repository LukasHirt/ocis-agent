import { afterEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Layout from './Layout.vue'
import { PLATFORM_KEY } from '../platform'
import { makePlatform } from '../test/platform'
import { apiErr, json, settingsOf, statusOf } from '../test/fixtures'
import { useAgentStore } from '../stores/agent'
import type { Status } from '../api/types'

const HostRouterView = defineComponent({ render: () => h('div', { class: 'host-rv' }) })

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

// serve answers the API; GET /status waits for statusGate if given.
const serve = (status: () => Response | Promise<Response>) =>
  vi.fn(async (input: RequestInfo | URL) => {
    const path = new URL(String(input), 'https://ocis.test').pathname.replace('/agent-api/v1', '')
    if (path === '/status') return status()
    if (path === '/settings') return json(settingsOf())
    if (path === '/approvals') return json({ approvals: [], next_cursor: null })
    return apiErr(503, 'unavailable') // the event stream
  })

const mountLayout = (fetch: ReturnType<typeof serve>) => {
  const platform = makePlatform({ fetch: fetch as unknown as typeof globalThis.fetch })
  wrapper = mount(Layout, {
    global: {
      provide: { [PLATFORM_KEY as symbol]: platform },
      components: { RouterView: HostRouterView },
      mocks: { $router: { push: vi.fn() } }
    }
  })
  return wrapper
}

describe('Layout', () => {
  it('shows a spinner until the status is known', async () => {
    let release!: (r: Response) => void
    const w = mountLayout(serve(() => new Promise<Response>((r) => (release = r))))
    await flushPromises()
    expect(w.find('.agent-layout-loading [data-stub=OcSpinner]').exists()).toBe(true)
    expect(w.find('.host-rv').exists()).toBe(false)
    release(json(statusOf()))
    await flushPromises()
    expect(w.find('.agent-layout-loading').exists()).toBe(false)
    expect(w.find('.host-rv').exists()).toBe(true)
  })

  it('renders the enrolment instead of the router view when not enrolled', async () => {
    const w = mountLayout(serve(() => json(statusOf({ enrolled: false }))))
    await flushPromises()
    expect(w.find('.agent-enrol').exists()).toBe(true)
    expect(w.find('.host-rv').exists()).toBe(false)
  })

  it('renders the host router view and the banner when enrolled', async () => {
    const w = mountLayout(serve(() => json(statusOf({ paused: true, pause_reason: 'dormant' }))))
    await flushPromises()
    expect(w.find('.host-rv').exists()).toBe(true)
    expect(w.find('.agent-banner').text()).toContain('Paused because you were away')
  })

  it('shows the fixed error and a retry when the status cannot be loaded', async () => {
    let fail = true
    const fetch = serve(() => (fail ? apiErr(503, 'unavailable') : json(statusOf())))
    const w = mountLayout(fetch)
    await flushPromises()
    expect(w.find('[role=alert]').text()).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await w.findAll('button').find((b) => b.text() === 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.find('.host-rv').exists()).toBe(true)
  })

  it('stops the store when unmounted', async () => {
    const w = mountLayout(serve(() => json(statusOf())))
    await flushPromises()
    const store = useAgentStore()
    const stop = vi.spyOn(store, 'stop')
    w.unmount()
    wrapper = undefined
    expect(stop).toHaveBeenCalledTimes(1)
    const status: Status | null = store.status
    expect(status).toBeNull() // nothing stale for the next mount
  })
})

describe('Layout after the first load', () => {
  it('shows a non-blocking error with a retry when a status refresh fails', async () => {
    let fail = false
    const w = mountLayout(serve(() => (fail ? apiErr(503, 'unavailable') : json(statusOf()))))
    await flushPromises()
    expect(w.find('.agent-layout-stale').exists()).toBe(false)
    fail = true
    const store = useAgentStore()
    await store.refreshStatus()
    await flushPromises()
    const line = w.find('.agent-layout-stale')
    expect(line.attributes('role')).toBe('alert')
    expect(line.text()).toContain('The agent service is not reachable right now. Try again later.')
    expect(line.text()).toContain('may be out of date')
    expect(w.find('.host-rv').exists()).toBe(true) // the page stays
    fail = false
    await line.findAll('button').find((b) => b.text() === 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-layout-stale').exists()).toBe(false)
  })

  it('Reconnect enrols again and clears the credential pause', async () => {
    let paused = true
    const fetch = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
      const path = new URL(String(input), 'https://ocis.test').pathname.replace('/agent-api/v1', '')
      if (path === '/enroll' && init.method === 'POST') {
        paused = false
        return json(statusOf())
      }
      if (path === '/status')
        return json(paused ? statusOf({ paused: true, pause_reason: 'credential_revoked' }) : statusOf())
      if (path === '/settings') return json(settingsOf())
      if (path === '/approvals') return json({ approvals: [], next_cursor: null })
      return apiErr(503, 'unavailable')
    })
    const w = mountLayout(fetch)
    await flushPromises()
    expect(w.find('.agent-banner').text()).toContain('Reconnect needed')
    await w.find('.agent-banner button').trigger('click')
    await flushPromises()
    expect(fetch.mock.calls.some(([u, i]) => String(u).endsWith('/enroll') && i?.method === 'POST')).toBe(true)
    expect(w.find('.agent-banner').exists()).toBe(false)
    await useAgentStore().refreshStatus()
    await flushPromises()
    expect(w.find('.agent-banner').exists()).toBe(false)
  })
})
