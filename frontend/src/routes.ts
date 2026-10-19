import type { Component } from 'vue'
import type { RouteRecordRaw } from 'vue-router'
import type { AppNavigationItem } from '@ownclouders/web-pkg'
import { APPID } from './appid'

// The layout is the root route component: it provides the platform and
// the agent's frame to every route below it. The host prefixes the route
// names with the app id (approvals becomes agent-approvals).
//
// The inbox is the first screen: v1 has no nav badge, so pending
// approvals must be the first thing a user sees.
export const buildRoutes = (
  gettext: (s: string) => string,
  layout: Component
): RouteRecordRaw[] => [
  {
    path: '/',
    name: 'root',
    component: layout,
    redirect: { name: `${APPID}-approvals` },
    meta: { authContext: 'user' },
    children: [
      {
        path: 'approvals',
        name: 'approvals',
        component: () => import('./views/Approvals.vue'),
        meta: { authContext: 'user', title: gettext('Approvals') }
      },
      {
        path: 'settings',
        name: 'settings',
        component: () => import('./views/Settings.vue'),
        meta: { authContext: 'user', title: gettext('Settings') }
      }
    ]
  }
]

// buildNavItems returns the app's sidebar entries, visible once the user is
// loaded.
export const buildNavItems = (
  gettext: (s: string) => string,
  isVisible: () => boolean
): AppNavigationItem[] => [
  {
    name: gettext('Approvals'),
    icon: 'checkbox-circle',
    route: { path: `/${APPID}/approvals` },
    isVisible,
    priority: 10
  },
  {
    name: gettext('Settings'),
    icon: 'settings-4',
    route: { path: `/${APPID}/settings` },
    isVisible,
    priority: 20
  }
]
