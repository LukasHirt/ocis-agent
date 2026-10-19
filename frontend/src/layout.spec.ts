import { describe, expect, it } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount } from '@vue/test-utils'
import { createLayout } from './layout'
import { usePlatform } from './platform'
import { makePlatform } from './test/platform'

describe('createLayout', () => {
  // The host registers router-view globally (app.use(router)); the layout
  // must render that one, so the test registers a stand-in under that name.
  it('renders the globally registered router-view and provides the platform below it', () => {
    const Child = defineComponent({
      setup() {
        const p = usePlatform()
        return () => h('span', p.backendUrl)
      }
    })
    const HostRouterView = defineComponent({ render: () => h('div', { class: 'host-rv' }, h(Child)) })
    const w = mount(createLayout(makePlatform({ backendUrl: '/via-layout' })), {
      global: { components: { RouterView: HostRouterView } }
    })
    expect(w.find('.host-rv').exists()).toBe(true)
    expect(w.text()).toBe('/via-layout')
  })
})
