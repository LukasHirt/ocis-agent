import type { Component } from 'vue'
import type { RouteLocationRaw, RouteRecordRaw } from 'vue-router'
import type { AppNavigationItem } from '@ownclouders/web-pkg'
import type { Status } from './api/types'
import { APPID } from './appid'

// homeRoute is the first screen: the approvals inbox while approvals are
// pending (v1 has no nav badge, so they must be the first thing a user
// sees), otherwise the chat. Null until the status is loaded.
export const homeRoute = (status: Status | null): RouteLocationRaw | null => {
  if (!status) return null
  return { name: status.pending_approvals > 0 ? `${APPID}-approvals` : `${APPID}-chat` }
}

// The layout is the root route component: it provides the platform and
// the agent's frame to every route below it. The host prefixes the route
// names with the app id (approvals becomes agent-approvals).
//
// The app root shows the home view, which replaces itself with homeRoute
// once the layout has the status (a static redirect cannot see it).
export const buildRoutes = (
  gettext: (s: string) => string,
  layout: Component
): RouteRecordRaw[] => [
  {
    path: '/',
    name: 'root',
    component: layout,
    meta: { authContext: 'user' },
    children: [
      {
        path: '',
        name: 'home',
        component: () => import('./views/Home.vue'),
        meta: { authContext: 'user', title: gettext('Agent') }
      },
      {
        path: 'approvals',
        name: 'approvals',
        component: () => import('./views/Approvals.vue'),
        // ?job=<id> shows only that job's approvals
        props: (route) => ({ job: typeof route.query.job === 'string' ? route.query.job : undefined }),
        meta: { authContext: 'user', title: gettext('Approvals') }
      },
      {
        path: 'chat',
        name: 'chat',
        component: () => import('./views/Chat.vue'),
        meta: { authContext: 'user', title: gettext('Chat') }
      },
      {
        path: 'chat/:id',
        name: 'chat-conversation',
        component: () => import('./views/Chat.vue'),
        props: true,
        meta: { authContext: 'user', title: gettext('Chat') }
      },
      {
        path: 'tasks',
        name: 'tasks',
        component: () => import('./views/Tasks.vue'),
        meta: { authContext: 'user', title: gettext('Tasks') }
      },
      {
        path: 'tasks/:id',
        name: 'tasks-job',
        component: () => import('./views/Tasks.vue'),
        props: true,
        meta: { authContext: 'user', title: gettext('Tasks') }
      },
      {
        path: 'activity',
        name: 'activity',
        component: () => import('./views/Activity.vue'),
        meta: { authContext: 'user', title: gettext('Activity') }
      },
      {
        path: 'memory',
        name: 'memory',
        component: () => import('./views/Memory.vue'),
        meta: { authContext: 'user', title: gettext('Memory') }
      },
      {
        // notes/<slug>.md arrives as two segments
        path: 'memory/:name+',
        name: 'memory-file',
        component: () => import('./views/Memory.vue'),
        props: (route) => {
          const n: unknown = route.params.name
          return { name: Array.isArray(n) ? n.join('/') : typeof n === 'string' ? n : undefined }
        },
        meta: { authContext: 'user', title: gettext('Memory') }
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
// loaded, in priority order. The icon names exist in the design system
// (both fill and line variants).
export const buildNavItems = (
  gettext: (s: string) => string,
  isVisible: () => boolean
): AppNavigationItem[] => [
  {
    name: gettext('Chat'),
    icon: 'chat-3',
    route: { path: `/${APPID}/chat` },
    isVisible,
    priority: 10
  },
  {
    name: gettext('Approvals'),
    icon: 'checkbox-circle',
    route: { path: `/${APPID}/approvals` },
    isVisible,
    priority: 15
  },
  {
    name: gettext('Tasks'),
    icon: 'task',
    route: { path: `/${APPID}/tasks` },
    isVisible,
    priority: 17
  },
  {
    name: gettext('Activity'),
    icon: 'history',
    route: { path: `/${APPID}/activity` },
    isVisible,
    priority: 18
  },
  {
    name: gettext('Memory'),
    icon: 'brain',
    route: { path: `/${APPID}/memory` },
    isVisible,
    priority: 19
  },
  {
    name: gettext('Settings'),
    icon: 'settings-4',
    route: { path: `/${APPID}/settings` },
    isVisible,
    priority: 20
  }
]
