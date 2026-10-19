import { defineComponent, h, type Component } from 'vue'
import { providePlatform, type Platform } from './platform'
import AgentLayout from './views/Layout.vue'

// createLayout returns the root route component: it provides the platform
// to every route below it and renders the agent's frame (views/Layout.vue),
// which renders the host's globally registered router-view: vue-router is
// not externalised by the SDK, so a bundled RouterView would not see the
// host router.
export const createLayout = (platform: Platform): Component =>
  defineComponent({
    name: 'AgentRoot',
    setup() {
      providePlatform(null, platform)
      return () => h(AgentLayout)
    }
  })
