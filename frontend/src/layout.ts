import { defineComponent, h, type Component } from 'vue'
import { RouterView } from 'vue-router'
import { providePlatform, type Platform } from './platform'

// createLayout returns the root route component: it provides the platform
// to every route below it.
export const createLayout = (platform: Platform): Component =>
  defineComponent({
    name: 'AgentLayout',
    setup() {
      providePlatform(null, platform)
      return () => h(RouterView)
    }
  })
