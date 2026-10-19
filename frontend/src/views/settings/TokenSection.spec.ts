import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import TokenSection from './TokenSection.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, json, statusOf } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import type { TokenSummary } from '../../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (extra?: Handler, confirm = true) => {
  const server = fakeServer(extra)
  const { store, platform } = await startStore(server)
  platform.confirm.mockResolvedValue(confirm)
  wrapper = mount(TokenSection, { global: { provide: { [PLATFORM_KEY as symbol]: platform } } })
  await flushPromises()
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

const tokens: TokenSummary[] = [
  { label: 'ocis-agent-i1-abc', created_at: '2026-01-01T10:00:00Z', expires_at: '2026-01-04T10:00:00Z', ours: true },
  { label: 'my phone', created_at: '2025-06-01T10:00:00Z', expires_at: null, ours: false },
  {
    label: '<script>alert(1)</script>\u202eexe.txt',
    created_at: null,
    expires_at: null,
    ours: false
  }
]

const incident = (over: Handler = () => undefined): Handler => (c) =>
  over(c) ??
  (c.method === 'GET' && c.path === '/token/revoke-all/preview'
    ? json({ tokens })
    : c.method === 'POST' && c.path === '/token/revoke-all'
      ? json({ revoked: 3 })
      : undefined)

describe('TokenSection', () => {
  it('shows the token state', async () => {
    const { w } = await setup((c) =>
      c.path === '/status'
        ? json(statusOf({ token: { status: 'valid', expires_at: '2026-01-04T10:00:00Z' } }))
        : undefined
    )
    expect(w.text()).toContain('valid')
  })

  it('renews the token now', async () => {
    const { w, server, platform } = await setup((c) =>
      c.method === 'POST' && c.path === '/token/renew' ? json({ renewed: true, old_cleaned: true }) : undefined
    )
    const st = server.count('GET', '/status')
    await button(w, 'Renew now')!.trigger('click')
    await flushPromises()
    expect(server.calls.find((c) => c.path === '/token/renew')?.body).toEqual({})
    expect(server.count('GET', '/status')).toBe(st + 1)
    expect(platform.notify).toHaveBeenCalledWith('success', "The agent's token was renewed")
  })

  it('shows the retry time when renewing is rate limited', async () => {
    const { w, platform } = await setup((c) =>
      c.path === '/token/renew'
        ? new Response(JSON.stringify({ error: { code: 'rate_limited', message: 'server text' } }), {
            status: 429,
            headers: { 'Retry-After': '5', 'Content-Type': 'application/json' }
          })
        : undefined
    )
    await button(w, 'Renew now')!.trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith(
      'error',
      'Too many requests. Wait a moment and try again.',
      'Try again in 5 seconds.'
    )
  })

  it('revokes the agent tokens after a confirmation', async () => {
    const { w, server, platform } = await setup((c) =>
      c.method === 'DELETE' && c.path === '/token' ? json({ revoked: 2 }) : undefined
    )
    await button(w, "Revoke the agent's tokens")!.trigger('click')
    await flushPromises()
    expect(platform.confirm).toHaveBeenCalledWith(expect.objectContaining({ danger: true }))
    expect(server.count('DELETE', '/token')).toBe(1)
    expect(platform.notify).toHaveBeenCalledWith('success', 'Revoked 2 tokens')
  })

  it('does not revoke when the confirmation is cancelled', async () => {
    const { w, server } = await setup(undefined, false)
    await button(w, "Revoke the agent's tokens")!.trigger('click')
    await flushPromises()
    expect(server.count('DELETE', '/token')).toBe(0)
  })

  describe('incident revoke', () => {
    const open = async (h: Handler = incident()) => {
      const s = await setup(h)
      await button(s.w, 'I suspect compromise')!.trigger('click')
      await flushPromises()
      return s
    }
    const finalButton = (w: VueWrapper) => button(w, 'Revoke all my app tokens')!

    it('lists all app tokens with hostile labels as text', async () => {
      const { w, server } = await open()
      expect(server.count('GET', '/token/revoke-all/preview')).toBe(1)
      const rows = w.findAll('[data-testid="agent-token-row"]')
      expect(rows).toHaveLength(3)
      expect(rows[0].text()).toContain('ocis-agent-i1-abc')
      expect(rows[0].text()).toContain('agent token')
      expect(rows[1].text()).toContain('my phone')
      expect(rows[1].text()).not.toContain('agent token')
      expect(rows[2].text()).toContain('<script>alert(1)</script>')
      expect(rows[2].text()).toContain('\\u{202E}')
      expect(w.find('script').exists()).toBe(false)
      expect(w.text()).toContain('ALL your app tokens')
    })

    it('enables the final button only after typing REVOKE', async () => {
      const { w, server } = await open()
      expect(finalButton(w).attributes('disabled')).toBeDefined()
      const input = w.find('input[data-testid="agent-revoke-confirm"]')
      for (const v of ['revoke', 'REVOK', 'REVOKE ', ' REVOKE', 'REVOKED']) {
        await input.setValue(v)
        expect(finalButton(w).attributes('disabled')).toBeDefined()
      }
      await finalButton(w).trigger('click')
      expect(server.count('POST', '/token/revoke-all')).toBe(0)
      await input.setValue('REVOKE')
      expect(finalButton(w).attributes('disabled')).toBeUndefined()
    })

    it('sends confirm true and shows the disabled state', async () => {
      const { w, server, store } = await open(
        incident((c) =>
          c.method === 'POST' && c.path === '/enroll' ? json(statusOf()) : undefined
        )
      )
      await w.find('input[data-testid="agent-revoke-confirm"]').setValue('REVOKE')
      await finalButton(w).trigger('click')
      await flushPromises()
      expect(server.calls.filter((c) => c.path === '/token/revoke-all').map((c) => c.body)).toEqual([
        { confirm: true }
      ])
      expect(w.find('[data-testid="agent-revoked-all"]').text()).toContain(
        'Agent disabled; enable it again to continue'
      )
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(0)
      await button(w, 'Enable agent')!.trigger('click')
      await flushPromises()
      expect(server.count('POST', '/enroll')).toBe(1)
      expect(store.status?.enrolled).toBe(true)
      expect(w.find('[data-testid="agent-revoked-all"]').exists()).toBe(false)
    })

    it('never sends twice on a double click', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const { w, server } = await open(
        incident((c) => (c.path === '/token/revoke-all' ? gate.then(() => json({ revoked: 1 })) : undefined))
      )
      await w.find('input[data-testid="agent-revoke-confirm"]').setValue('REVOKE')
      await finalButton(w).trigger('click')
      await finalButton(w).trigger('click')
      release()
      await flushPromises()
      expect(server.count('POST', '/token/revoke-all')).toBe(1)
    })

    it('a reopened dialog waits for its own list', async () => {
      const releases: Array<() => void> = []
      const lists = [[tokens[1]], tokens]
      let n = 0
      const { w } = await setup(
        incident((c) => {
          if (c.path !== '/token/revoke-all/preview') return undefined
          const mine = lists[n++]
          return new Promise<Response>((r) => releases.push(() => r(json({ tokens: mine }))))
        })
      )
      await button(w, 'I suspect compromise')!.trigger('click')
      await button(w, 'Cancel')!.trigger('click')
      await button(w, 'I suspect compromise')!.trigger('click')
      await flushPromises()
      releases[0]()
      await flushPromises()
      expect(w.find('[role="alertdialog"] [data-stub="OcSpinner"]').exists()).toBe(true)
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(0)
      releases[1]()
      await flushPromises()
      expect(w.find('[role="alertdialog"] [data-stub="OcSpinner"]').exists()).toBe(false)
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(3)
    })

    it('can be cancelled', async () => {
      const { w, server } = await open()
      await button(w, 'Cancel')!.trigger('click')
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(0)
      expect(server.count('POST', '/token/revoke-all')).toBe(0)
    })

    it('shows a fixed error when the list cannot be loaded and retries', async () => {
      let fail = true
      const { w } = await open(
        incident((c) => (c.path === '/token/revoke-all/preview' && fail ? apiErr(502, 'upstream_unavailable') : undefined))
      )
      expect(w.find('[role="alert"]').text()).toContain('ownCloud could not be reached. Try again later.')
      expect(w.text()).not.toContain('server text')
      expect(button(w, 'Revoke all my app tokens')).toBeUndefined()
      fail = false
      await button(w, 'Try again')!.trigger('click')
      await flushPromises()
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(3)
    })

    it('keeps the dialog and shows a fixed error when the revoke fails', async () => {
      const { w } = await open(
        incident((c) => (c.path === '/token/revoke-all' ? apiErr(502, 'upstream_unavailable') : undefined))
      )
      await w.find('input[data-testid="agent-revoke-confirm"]').setValue('REVOKE')
      await finalButton(w).trigger('click')
      await flushPromises()
      expect(w.find('[role="alert"]').text()).toContain('ownCloud could not be reached. Try again later.')
      expect(w.find('[data-testid="agent-revoked-all"]').exists()).toBe(false)
      expect(w.findAll('[data-testid="agent-token-row"]')).toHaveLength(3)
    })

    it('shows an empty list', async () => {
      const { w } = await open(incident((c) => (c.path === '/token/revoke-all/preview' ? json({ tokens: [] }) : undefined)))
      expect(w.text()).toContain('You have no app tokens')
    })
  })
})
