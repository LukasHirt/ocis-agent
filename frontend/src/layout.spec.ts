import { describe, expect, it } from 'vitest'
import { defineComponent, h } from 'vue'
import { mount, flushPromises } from '@vue/test-utils'
import { createMemoryHistory, createRouter, RouterView } from 'vue-router'
import { createLayout } from './layout'
import { usePlatform } from './platform'
import { makePlatform } from './test/platform'

describe('createLayout', () => {
  it('provides the platform to the routes below it', async () => {
    const Child = defineComponent({
      setup() {
        const p = usePlatform()
        return () => h('span', p.backendUrl)
      }
    })
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        {
          path: '/',
          component: createLayout(makePlatform({ backendUrl: '/via-layout' })),
          children: [{ path: '', component: Child }]
        }
      ]
    })
    await router.push('/')
    const w = mount(RouterView, { global: { plugins: [router] } })
    await flushPromises()
    expect(w.text()).toBe('/via-layout')
  })
})
