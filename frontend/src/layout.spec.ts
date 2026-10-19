import { describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { createLayout } from './layout'
import { usePlatform } from './platform'
import { makePlatform } from './test/platform'
import { json, settingsOf, statusOf } from './test/fixtures'

describe('createLayout', () => {
  // The host registers router-view globally (app.use(router)); the layout
  // must render that one, so the test registers a stand-in under that name.
  it('renders the globally registered router-view and provides the platform below it', async () => {
    const Child = defineComponent({
      setup() {
        const p = usePlatform()
        return () => h('span', { class: 'child' }, p.backendUrl)
      }
    })
    const HostRouterView = defineComponent({ render: () => h('div', { class: 'host-rv' }, h(Child)) })
    const fetch = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input)
      if (path.endsWith('/status')) return json(statusOf())
      if (path.endsWith('/settings')) return json(settingsOf())
      if (path.includes('/approvals')) return json({ approvals: [], next_cursor: null })
      return new Response('', { status: 503 })
    })
    const platform = makePlatform({ backendUrl: '/via-layout', fetch: fetch as unknown as typeof globalThis.fetch })
    const w = mount(createLayout(platform), {
      global: { components: { RouterView: HostRouterView }, mocks: { $router: { push: vi.fn() } } }
    })
    await flushPromises()
    expect(w.find('.host-rv').exists()).toBe(true)
    expect(w.find('.child').text()).toBe('/via-layout')
    expect(String(fetch.mock.calls[0][0])).toMatch(/^\/via-layout\//)
    w.unmount()
  })
})
