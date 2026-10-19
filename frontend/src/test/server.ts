import { vi } from 'vitest'
import { flushPromises } from '@vue/test-utils'
import { apiErr, json, settingsOf, statusOf } from './fixtures'
import { makePlatform, type FakePlatform } from './platform'
import { useAgentStore } from '../stores/agent'
import type { EventClientOptions } from '../api/events'

// A fake agent API for component tests: every request is recorded; the
// handler answers what it wants and returns undefined for the defaults
// (status, settings, an empty approvals page, else 404).
export interface Call {
  method: string
  path: string
  query: URLSearchParams
  body: unknown
  headers: Headers
}

export type Handler = (c: Call) => Response | Promise<Response> | undefined

export const fakeServer = (handler: Handler = () => undefined) => {
  const calls: Call[] = []
  const fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const u = new URL(String(input), 'https://ocis.test')
    const c: Call = {
      method: init?.method ?? 'GET',
      path: u.pathname.replace(/^\/agent-api\/v1/, ''),
      query: u.searchParams,
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
      headers: new Headers(init?.headers)
    }
    calls.push(c)
    const r = await handler(c)
    if (r) return r
    if (c.path === '/status') return json(statusOf())
    if (c.path === '/settings') return json(settingsOf())
    if (c.path === '/approvals') return json({ approvals: [], next_cursor: null })
    return apiErr(404, 'not_found')
  })
  const count = (method: string, path: string) =>
    calls.filter((c) => c.method === method && c.path === path).length
  return { fetch, calls, count }
}

export type FakeServer = ReturnType<typeof fakeServer>

// startStore starts the agent store against the fake server, without an
// event stream, and waits for the first refresh.
// events() returns the callbacks the store gave its event client, so a
// test can deliver changes, readies and polls.
export const startStore = async (server: FakeServer, platform?: FakePlatform) => {
  const p = platform ?? makePlatform({ fetch: server.fetch as unknown as typeof fetch })
  const store = useAgentStore()
  let opts: EventClientOptions | undefined
  store.start({
    platform: p,
    gettext: (s) => s,
    createEvents: (o) => {
      opts = o
      return { start() {}, stop() {} }
    }
  })
  await flushPromises()
  const events = (): EventClientOptions => {
    if (!opts) throw new Error('the store made no event client')
    return opts
  }
  return { store, platform: p, events }
}
