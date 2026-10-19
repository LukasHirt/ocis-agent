import { describe, expect, it } from 'vitest'
import { defineComponent } from 'vue'
import { buildNavItems, buildRoutes, homeRoute } from './routes'
import { statusOf } from './test/fixtures'
import { APPID } from './appid'

const gettext = (s: string) => s
const layout = defineComponent({ render: () => null })

describe('buildRoutes', () => {
  it('shows the home screen at the app root instead of a fixed redirect', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.path).toBe('/')
    expect(root.redirect).toBeUndefined()
    const home = (root.children ?? [])[0]
    expect([home.path, home.name]).toEqual(['', 'home'])
  })

  it('requires an authenticated user on the root and every child route', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.meta?.authContext).toBe('user')
    for (const child of root.children ?? []) {
      expect(child.meta?.authContext).toBe('user')
    }
  })

  it('has the approvals inbox, the chat, the tasks, the activity, the memory and the settings as children of the layout', () => {
    const [root] = buildRoutes(gettext, layout)
    const children = (root.children ?? []).map((c) => [c.path, c.name, c.meta?.title])
    // the host prefixes the names with the app id (agent-approvals, agent-settings)
    expect(children).toEqual([
      ['', 'home', 'Agent'],
      ['approvals', 'approvals', 'Approvals'],
      ['chat', 'chat', 'Chat'],
      ['chat/:id', 'chat-conversation', 'Chat'],
      ['tasks', 'tasks', 'Tasks'],
      ['tasks/:id', 'tasks-job', 'Tasks'],
      ['activity', 'activity', 'Activity'],
      ['memory', 'memory', 'Memory'],
      ['memory/:name+', 'memory-file', 'Memory'],
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

  it('passes the memory file name joined from its segments', () => {
    const [root] = buildRoutes(gettext, layout)
    const memoryProps = (root.children ?? []).find((c) => c.name === 'memory-file')!.props as (r: unknown) => unknown
    expect(memoryProps({ params: { name: ['notes', 'a.md'] } })).toEqual({ name: 'notes/a.md' })
    expect(memoryProps({ params: { name: ['profile.md'] } })).toEqual({ name: 'profile.md' })
    expect(memoryProps({ params: { name: 'profile.md' } })).toEqual({ name: 'profile.md' })
    expect(memoryProps({ params: {} })).toEqual({ name: undefined })
  })

  it('uses the given layout as the root component', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.component).toBe(layout)
  })
})

describe('homeRoute', () => {
  it('opens the approvals inbox while approvals are pending', () => {
    expect(homeRoute(statusOf({ pending_approvals: 2 }))).toEqual({ name: `${APPID}-approvals` })
  })

  it('opens the chat when nothing is pending', () => {
    expect(homeRoute(statusOf({ pending_approvals: 0 }))).toEqual({ name: `${APPID}-chat` })
  })

  it('decides nothing before the status is loaded', () => {
    expect(homeRoute(null)).toBeNull()
  })
})

describe('buildNavItems', () => {
  it('links the chat, the approvals, the tasks, the activity, the memory and the settings in that order, without a badge', () => {
    let user = false
    const items = buildNavItems(gettext, () => user)
    expect(items.map((i) => [i.name, i.icon, i.route])).toEqual([
      ['Chat', 'chat-3', { path: `/${APPID}/chat` }],
      ['Approvals', 'checkbox-circle', { path: `/${APPID}/approvals` }],
      ['Tasks', 'task', { path: `/${APPID}/tasks` }],
      ['Activity', 'history', { path: `/${APPID}/activity` }],
      ['Memory', 'brain', { path: `/${APPID}/memory` }],
      ['Settings', 'settings-4', { path: `/${APPID}/settings` }]
    ])
    expect(items.every((i) => i.isVisible?.() === false)).toBe(true)
    user = true
    expect(items.every((i) => i.isVisible?.() === true)).toBe(true)
    for (const i of items) expect(i.name).not.toMatch(/\d/)
    const priorities = items.map((i) => i.priority ?? 0)
    expect(priorities).toEqual([...priorities].sort((a, b) => a - b))
    expect(new Set(priorities).size).toBe(priorities.length)
  })
})
