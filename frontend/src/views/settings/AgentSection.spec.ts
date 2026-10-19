import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import AgentSection from './AgentSection.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, json, settingsOf, statusOf } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import type { Status } from '../../api/types'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (status: Partial<Status>, extra?: Handler) => {
  const server = fakeServer((c) => {
    const r = extra?.(c)
    if (r) return r
    if (c.method === 'GET' && c.path === '/status') return json(statusOf(status))
    return undefined
  })
  const { store, platform } = await startStore(server)
  wrapper = mount(AgentSection, { global: { provide: { [PLATFORM_KEY as symbol]: platform } } })
  await flushPromises()
  return { w: wrapper, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('AgentSection', () => {
  it('shows the enrolled user as text', async () => {
    const { w } = await setup({ user: { id: 'u1', name: '<img src=x onerror=alert(1)>\u202eevil', login: 'einstein' } })
    expect(w.text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.text()).toContain('\\u{202E}')
    expect(w.text()).toContain('einstein')
    expect(w.find('img').exists()).toBe(false)
  })

  it('pauses a running agent', async () => {
    const { w, server, store } = await setup({}, (c) =>
      c.method === 'POST' && c.path === '/pause' ? json(statusOf({ paused: true, pause_reason: 'user' })) : undefined
    )
    expect(w.text()).toContain('The agent is running')
    expect(button(w, 'Resume')).toBeUndefined()
    await button(w, 'Pause')!.trigger('click')
    await flushPromises()
    expect(server.count('POST', '/pause')).toBe(1)
    expect(store.status?.paused).toBe(true)
    expect(w.text()).toContain('You paused the agent')
    expect(button(w, 'Pause')).toBeUndefined()
  })

  it('resumes a user pause with reason user', async () => {
    const { w, server } = await setup({ paused: true, pause_reason: 'user' }, (c) =>
      c.method === 'POST' && c.path === '/resume' ? json(statusOf()) : undefined
    )
    expect(button(w, 'I have reviewed my app tokens — resume')).toBeUndefined()
    await button(w, 'Resume')!.trigger('click')
    await flushPromises()
    expect(server.calls.find((c) => c.path === '/resume')?.body).toEqual({ reason: 'user' })
    expect(w.text()).toContain('The agent is running')
  })

  it('resumes a token anomaly only through the explicit review button', async () => {
    const { w, server } = await setup({ paused: true, pause_reason: 'token_anomaly' }, (c) =>
      c.method === 'POST' && c.path === '/resume' ? json(statusOf()) : undefined
    )
    expect(w.text()).toContain('unknown app token')
    expect(button(w, 'Resume')).toBeUndefined()
    expect(button(w, 'Pause')).toBeUndefined()
    await button(w, 'I have reviewed my app tokens — resume')!.trigger('click')
    await flushPromises()
    expect(server.calls.find((c) => c.path === '/resume')?.body).toEqual({ reason: 'token_anomaly' })
  })

  it.each(['credential_invalid', 'credential_revoked', 'credential_expired'])(
    'offers to reconnect for %s and no resume',
    async (reason) => {
      const { w, server } = await setup({ paused: true, pause_reason: reason }, (c) =>
        c.method === 'POST' && c.path === '/enroll' ? json(statusOf()) : undefined
      )
      expect(button(w, 'Resume')).toBeUndefined()
      await button(w, 'Reconnect')!.trigger('click')
      await flushPromises()
      expect(server.count('POST', '/enroll')).toBe(1)
    }
  )

  it('explains a dormant pause without a button', async () => {
    const { w } = await setup({ paused: true, pause_reason: 'dormant' })
    expect(w.text()).toContain('away')
    expect(button(w, 'Resume')).toBeUndefined()
    expect(button(w, 'Pause')).toBeUndefined()
  })

  it('shows an unknown pause reason generically', async () => {
    const { w } = await setup({ paused: true, pause_reason: '<b>weird</b>' })
    expect(w.text()).toContain('The agent is paused')
    expect(w.text()).not.toContain('weird')
  })

  it('shows the kill switch', async () => {
    const off = await setup({})
    expect(off.w.text()).toContain("The operator's emergency stop is off")
    off.w.unmount()
    off.store.stop()
    const on = await setup({ kill_switch: { database: false, environment: true } })
    expect(on.w.text()).toContain('The operator has stopped the agent for everyone')
  })

  it('shows the dormancy days from the settings', async () => {
    const { w } = await setup({}, (c) =>
      c.method === 'GET' && c.path === '/settings' ? json(settingsOf({ dormancy_days: 12 })) : undefined
    )
    expect(w.text()).toContain('12 days')
  })

  it('shows a fixed error when pausing fails', async () => {
    const { w, platform } = await setup({}, (c) =>
      c.method === 'POST' && c.path === '/pause' ? apiErr(503, 'unavailable') : undefined
    )
    await button(w, 'Pause')!.trigger('click')
    await flushPromises()
    expect(platform.notify).toHaveBeenCalledWith(
      'error',
      'The agent service is not reachable right now. Try again later.',
      undefined
    )
  })

  describe('disable', () => {
    const unenrolling = (): Handler => {
      let enrolled = true
      return (c) => {
        if (c.method === 'POST' && c.path === '/unenroll') {
          enrolled = false
          return new Response(null, { status: 204 })
        }
        if (c.method === 'GET' && c.path === '/status') return json(statusOf({ enrolled }))
        return undefined
      }
    }

    it('asks first and can be cancelled', async () => {
      const { w, server } = await setup({}, unenrolling())
      await button(w, 'Disable agent')!.trigger('click')
      const dialog = w.find('[role="alertdialog"]')
      expect(dialog.exists()).toBe(true)
      expect(dialog.text()).toContain("Also delete the agent's memory folder")
      await button(w, 'Cancel')!.trigger('click')
      expect(w.find('[role="alertdialog"]').exists()).toBe(false)
      expect(server.count('POST', '/unenroll')).toBe(0)
    })

    it('unenrols and keeps the memory by default', async () => {
      const { w, server, store } = await setup({}, unenrolling())
      await button(w, 'Disable agent')!.trigger('click')
      await w.find('[role="alertdialog"]').findAll('button').find((b) => b.text() === 'Disable agent')!.trigger('click')
      await flushPromises()
      expect(server.calls.filter((c) => c.path === '/unenroll').map((c) => c.body)).toEqual([{ delete_memory: false }])
      expect(store.status?.enrolled).toBe(false)
    })

    it('unenrols and deletes the memory when ticked', async () => {
      const { w, server } = await setup({}, unenrolling())
      await button(w, 'Disable agent')!.trigger('click')
      await w.find('[role="alertdialog"] input[type="checkbox"]').setValue(true)
      await w.find('[role="alertdialog"]').findAll('button').find((b) => b.text() === 'Disable agent')!.trigger('click')
      await flushPromises()
      expect(server.calls.filter((c) => c.path === '/unenroll').map((c) => c.body)).toEqual([{ delete_memory: true }])
    })

    it('never sends twice on a double click', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const { w, server } = await setup({}, (c) =>
        c.method === 'POST' && c.path === '/unenroll'
          ? gate.then(() => new Response(null, { status: 204 }))
          : undefined
      )
      await button(w, 'Disable agent')!.trigger('click')
      const confirm = w.find('[role="alertdialog"]').findAll('button').find((b) => b.text() === 'Disable agent')!
      await confirm.trigger('click')
      await confirm.trigger('click')
      release()
      await flushPromises()
      expect(server.count('POST', '/unenroll')).toBe(1)
    })

    it('locks the memory choice while the request runs', async () => {
      let release!: () => void
      const gate = new Promise<void>((r) => (release = r))
      const { w } = await setup({}, (c) =>
        c.method === 'POST' && c.path === '/unenroll' ? gate.then(() => apiErr(502, 'upstream_unavailable')) : undefined
      )
      await button(w, 'Disable agent')!.trigger('click')
      const box = () => w.find('[role="alertdialog"] input[type="checkbox"]')
      expect(box().attributes('disabled')).toBeUndefined()
      await w.find('[role="alertdialog"]').findAll('button').find((b) => b.text() === 'Disable agent')!.trigger('click')
      expect(box().attributes('disabled')).toBeDefined()
      release()
      await flushPromises()
      expect(box().attributes('disabled')).toBeUndefined()
    })

    it('shows a fixed error and keeps the dialog when it fails', async () => {
      const { w } = await setup({}, (c) =>
        c.method === 'POST' && c.path === '/unenroll' ? apiErr(502, 'upstream_unavailable') : undefined
      )
      await button(w, 'Disable agent')!.trigger('click')
      await w.find('[role="alertdialog"]').findAll('button').find((b) => b.text() === 'Disable agent')!.trigger('click')
      await flushPromises()
      const dialog = w.find('[role="alertdialog"]')
      expect(dialog.exists()).toBe(true)
      expect(dialog.find('[role="alert"]').text()).toBe('ownCloud could not be reached. Try again later.')
      expect(dialog.text()).not.toContain('server text')
    })
  })
})
