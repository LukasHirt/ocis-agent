import { defineComponent, h, resolveComponent, type Component } from 'vue'
import { providePlatform, type Platform } from './platform'

// createLayout returns the root route component: it provides the platform
// to every route below it. It renders the host's globally registered
// router-view: vue-router is not externalised by the SDK, so a bundled
// RouterView would not see the host router.
export const createLayout = (platform: Platform): Component =>
  defineComponent({
    name: 'AgentLayout',
    setup() {
      providePlatform(null, platform)
      const RouterView = resolveComponent('router-view')
      return () => h(RouterView)
    }
  })
