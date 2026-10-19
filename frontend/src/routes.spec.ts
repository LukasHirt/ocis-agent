import { describe, expect, it } from 'vitest'
import { defineComponent } from 'vue'
import { buildRoutes } from './routes'
import { APPID } from './appid'

const gettext = (s: string) => s
const layout = defineComponent({ render: () => null })

describe('buildRoutes', () => {
  it('redirects the app root to the approvals inbox', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.path).toBe('/')
    expect(root.redirect).toBe(`${APPID}/approvals`)
  })

  it('requires an authenticated user on the root and every child route', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.meta?.authContext).toBe('user')
    for (const child of root.children ?? []) {
      expect(child.meta?.authContext).toBe('user')
    }
  })

  it('exposes the approvals inbox as a child route', () => {
    const [root] = buildRoutes(gettext, layout)
    const paths = (root.children ?? []).map((c) => c.path)
    expect(paths).toContain('approvals')
  })
})

describe('buildRoutes layout', () => {
  it('uses the given layout as the root component', () => {
    const [root] = buildRoutes(gettext, layout)
    expect(root.component).toBe(layout)
  })
})
