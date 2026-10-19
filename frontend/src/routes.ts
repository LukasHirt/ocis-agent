import type { Component } from 'vue'
import { RouterView, type RouteRecordRaw } from 'vue-router'
import { APPID } from './appid'

// The inbox is the first screen: v1 has no nav badge, so pending
// approvals must be the first thing a user sees. The layout is the root
// route component and provides the platform to every route below it.
export const buildRoutes = (
  gettext: (s: string) => string,
  layout: Component = RouterView
): RouteRecordRaw[] => [
  {
    path: '/',
    name: 'root',
    component: layout,
    redirect: `${APPID}/approvals`,
    meta: { authContext: 'user' },
    children: [
      {
        path: 'approvals',
        name: 'approvals',
        component: () => import('./views/Home.vue'),
        meta: { authContext: 'user', title: gettext('Approvals') }
      }
    ]
  }
]
