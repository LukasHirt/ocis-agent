import type { RouteRecordRaw } from 'vue-router'
import { APPID } from './appid'

// The inbox is the first screen: v1 has no nav badge, so pending
// approvals must be the first thing a user sees.
export const buildRoutes = (gettext: (s: string) => string): RouteRecordRaw[] => [
  {
    path: '/',
    name: 'root',
    component: () => import('./views/Home.vue'),
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
