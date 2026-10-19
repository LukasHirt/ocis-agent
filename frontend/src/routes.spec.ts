import { describe, expect, it } from 'vitest'
import { defineComponent } from 'vue'
import { buildNavItems, buildRoutes } from './routes'
import { APPID } from './appid'

const gettext = (s: string) => s
const layout = defineComponent({ render: () => null })

describe('buildRoutes', () => {
  it('redirects the app root to the approvals inbox by its host name', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.path).toBe('/')
    expect(root.redirect).toEqual({ name: `${APPID}-approvals` })
  })

  it('requires an authenticated user on the root and every child route', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.meta?.authContext).toBe('user')
    for (const child of root.children ?? []) {
      expect(child.meta?.authContext).toBe('user')
    }
  })

  it('has the approvals inbox, the chat, the tasks, the activity and the settings as children of the layout', () => {
    const [root] = buildRoutes(gettext, layout)
    const children = (root.children ?? []).map((c) => [c.path, c.name, c.meta?.title])
    // the host prefixes the names with the app id (agent-approvals, agent-settings)
    expect(children).toEqual([
      ['approvals', 'approvals', 'Approvals'],
      ['chat', 'chat', 'Chat'],
      ['chat/:id', 'chat-conversation', 'Chat'],
      ['tasks', 'tasks', 'Tasks'],
      ['tasks/:id', 'tasks-job', 'Tasks'],
      ['activity', 'activity', 'Activity'],
      ['settings', 'settings', 'Settings']
    ])
    for (const child of root.children ?? []) {
      expect(typeof child.component).toBe('function') // lazily loaded views
    }
  })

  it('passes the conversation id and the approvals job filter as props', () => {
    const [root] = buildRoutes(gettext, layout)
    const byName = (n: string) => (root.children ?? []).find((c) => c.name === n)!
    expect(byName('chat-conversation').props).toBe(true)
    expect(byName('tasks-job').props).toBe(true)
    const approvalsProps = byName('approvals').props as (r: unknown) => unknown
    expect(approvalsProps({ query: { job: 'j1' } })).toEqual({ job: 'j1' })
    expect(approvalsProps({ query: {} })).toEqual({ job: undefined })
    expect(approvalsProps({ query: { job: ['a', 'b'] } })).toEqual({ job: undefined })
  })

  it('uses the given layout as the root component', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.component).toBe(layout)
  })
})

describe('buildNavItems', () => {
  it('links the approvals, the chat, the tasks, the activity and the settings, without a badge', () => {
    let user = false
    const items = buildNavItems(gettext, () => user)
    expect(items.map((i) => [i.name, i.icon, i.route])).toEqual([
      ['Approvals', 'checkbox-circle', { path: `/${APPID}/approvals` }],
      ['Chat', 'chat-3', { path: `/${APPID}/chat` }],
      ['Tasks', 'task', { path: `/${APPID}/tasks` }],
      ['Activity', 'history', { path: `/${APPID}/activity` }],
      ['Settings', 'settings-4', { path: `/${APPID}/settings` }]
    ])
    expect(items.every((i) => i.isVisible?.() === false)).toBe(true)
    user = true
    expect(items.every((i) => i.isVisible?.() === true)).toBe(true)
    for (const i of items) expect(i.name).not.toMatch(/\d/)
  })
})
