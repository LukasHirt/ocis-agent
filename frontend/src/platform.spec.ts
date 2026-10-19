import { describe, expect, it, vi } from 'vitest'
import { createApp, defineComponent, h } from 'vue'
import { mount } from '@vue/test-utils'
import { createPlatform, PLATFORM_KEY, providePlatform, usePlatform, type Platform } from './platform'
import { makePlatform } from './test/platform'

const Consumer = defineComponent({
  setup() {
    const p = usePlatform()
    return () => h('span', p.backendUrl)
  }
})

describe('platform seam', () => {
  it('throws a clear error when no platform was provided', () => {
    let err: unknown
    const Probe = defineComponent({
      setup() {
        try {
          usePlatform()
        } catch (e) {
          err = e
        }
        return () => h('span')
      }
    })
    mount(Probe)
    expect(err).toBeInstanceOf(Error)
    expect((err as Error).message).toMatch(/platform/i)
  })

  it('throws outside a component setup', () => {
    expect(() => usePlatform()).toThrow(/platform/i)
  })

  it('injects a platform provided on the app', () => {
    const p = makePlatform({ backendUrl: '/custom' })
    const app = createApp(Consumer)
    providePlatform(app, p)
    const el = document.createElement('div')
    app.mount(el)
    expect(el.textContent).toBe('/custom')
    app.unmount()
  })

  it('injects a platform provided by a parent component', () => {
    const p = makePlatform({ backendUrl: '/from-parent' })
    const Parent = defineComponent({
      setup() {
        providePlatform(null, p)
        return () => h(Consumer)
      }
    })
    expect(mount(Parent).text()).toBe('/from-parent')
  })

  it('injects a platform given through the test mount options', () => {
    const p: Platform = makePlatform({ backendUrl: '/mounted' })
    expect(mount(Consumer, { global: { provide: { [PLATFORM_KEY]: p } } }).text()).toBe('/mounted')
  })

  it('the fake platform exposes spies', async () => {
    const p = makePlatform()
    expect(p.getToken()).toBe('test-token')
    await expect(p.confirm({ title: 't', message: 'm', confirmText: 'ok' })).resolves.toBe(true)
    p.notify('info', 'x')
    expect(p.notify).toHaveBeenCalledWith('info', 'x')
  })
})

describe('createPlatform', () => {
  const deps = () => {
    let token = 'first'
    const modals: Array<Record<string, unknown>> = []
    let open: string[] = []
    const listeners = new Set<() => void>()
    return {
      setToken: (t: string) => (token = t),
      modals,
      listeners,
      // what the host's removeModal / removeAllModals do
      close: (id?: string) => {
        open = id === undefined ? [] : open.filter((x) => x !== id)
        listeners.forEach((l) => l())
      },
      d: {
        accessToken: () => token,
        backendUrl: undefined as unknown,
        fetch: vi.fn<typeof fetch>(),
        showMessage: (_: { title: string; desc?: string; status?: string }) => undefined,
        showErrorMessage: (_: { title: string; desc?: string }) => undefined,
        dispatchModal: (m: Record<string, unknown>) => {
          const id = `m${modals.length}`
          modals.push(m)
          open.push(id)
          return { id }
        },
        isModalOpen: (id: string) => open.includes(id),
        onModalsChange: (cb: () => void) => {
          listeners.add(cb)
          return () => listeners.delete(cb)
        }
      }
    }
  }

  it('reads the token at every call and maps an empty token to undefined', () => {
    const x = deps()
    const p = createPlatform(x.d)
    expect(p.getToken()).toBe('first')
    x.setToken('second')
    expect(p.getToken()).toBe('second')
    x.setToken('')
    expect(p.getToken()).toBeUndefined()
  })

  it('defaults the backend URL and drops a trailing slash', () => {
    const x = deps()
    expect(createPlatform(x.d).backendUrl).toBe('/agent-api/v1')
    expect(createPlatform({ ...x.d, backendUrl: '/x/api/' }).backendUrl).toBe('/x/api')
    expect(createPlatform({ ...x.d, backendUrl: 42 }).backendUrl).toBe('/agent-api/v1')
  })

  it('accepts only a root-relative backend URL', () => {
    const x = deps()
    const url = (backendUrl: unknown) => createPlatform({ ...x.d, backendUrl }).backendUrl
    expect(url('/agent-api/v1')).toBe('/agent-api/v1')
    for (const bad of [
      'https://evil.example/api',
      '//evil.example/api',
      '/\\evil.example/api',
      '\\\\evil.example',
      'agent-api/v1',
      'javascript:alert(1)',
      '',
      '/',
      ' /agent-api/v1',
      '/agent api'
    ]) {
      expect(url(bad), bad).toBe('/agent-api/v1')
    }
  })

  it('routes notifications to the oCIS messages', () => {
    const x = deps()
    const showMessage = vi.fn()
    const showErrorMessage = vi.fn()
    const p = createPlatform({ ...x.d, showMessage, showErrorMessage })
    p.notify('success', 'Saved', 'desc')
    p.notify('info', 'Note')
    p.notify('error', 'Failed', 'why')
    expect(showMessage).toHaveBeenNthCalledWith(1, { title: 'Saved', desc: 'desc' })
    expect(showMessage).toHaveBeenNthCalledWith(2, { title: 'Note', status: 'passive' })
    expect(showErrorMessage).toHaveBeenCalledWith({ title: 'Failed', desc: 'why' })
  })

  it('resolves confirm with true on confirm and false on cancel', async () => {
    const x = deps()
    const p = createPlatform(x.d)
    const a = p.confirm({ title: 'Revoke', message: 'Sure?', confirmText: 'Revoke', danger: true })
    expect(x.modals[0]).toMatchObject({
      title: 'Revoke',
      message: 'Sure?',
      confirmText: 'Revoke',
      variation: 'danger'
    })
    ;(x.modals[0].onConfirm as () => void)()
    await expect(a).resolves.toBe(true)

    const b = p.confirm({ title: 't', message: 'm', confirmText: 'ok' })
    expect(x.modals[1].variation).toBe('passive')
    ;(x.modals[1].onCancel as () => void)()
    await expect(b).resolves.toBe(false)
    expect(x.listeners.size).toBe(0)
  })

  it('resolves confirm with false when the modal is removed without a decision', async () => {
    const x = deps()
    const p = createPlatform(x.d)
    const a = p.confirm({ title: 't', message: 'm', confirmText: 'ok' })
    const b = p.confirm({ title: 't2', message: 'm', confirmText: 'ok' })
    x.close('m0')
    await expect(a).resolves.toBe(false)
    x.close()
    await expect(b).resolves.toBe(false)
    expect(x.listeners.size).toBe(0)
  })

  it('keeps true when the host removes the modal after confirming', async () => {
    const x = deps()
    const p = createPlatform(x.d)
    const a = p.confirm({ title: 't', message: 'm', confirmText: 'ok' })
    ;(x.modals[0].onConfirm as () => void)()
    x.close('m0')
    await expect(a).resolves.toBe(true)
  })

  it('leaves other modals alone', async () => {
    const x = deps()
    const p = createPlatform(x.d)
    let settled = false
    void p.confirm({ title: 't', message: 'm', confirmText: 'ok' }).then(() => (settled = true))
    x.close('other')
    await Promise.resolve()
    expect(settled).toBe(false)
  })

  it('uses the given fetch and the real clock and random source', () => {
    const x = deps()
    const p = createPlatform(x.d)
    expect(p.fetch).toBe(x.d.fetch)
    expect(Math.abs(p.now() - Date.now())).toBeLessThan(1000)
    const r = p.random()
    expect(r).toBeGreaterThanOrEqual(0)
    expect(r).toBeLessThan(1)
  })
})
