import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Home from './Home.vue'
import { APPID } from '../appid'
import { json, statusOf } from '../test/fixtures'
import { fakeServer, startStore } from '../test/server'
import type { Status } from '../api/types'
import { useAgentStore } from '../stores/agent'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (status: Status) => {
  const server = fakeServer((c) => (c.path === '/status' ? json(status) : undefined))
  await startStore(server)
  const replace = vi.fn(async () => undefined)
  wrapper = mount(Home, { global: { mocks: { $router: { replace } } } })
  await flushPromises()
  return { w: wrapper, replace }
}

describe('Home', () => {
  it('opens the approvals inbox when approvals are pending', async () => {
    const { replace } = await setup(statusOf({ pending_approvals: 3 }))
    expect(replace).toHaveBeenCalledTimes(1)
    expect(replace).toHaveBeenCalledWith({ name: `${APPID}-approvals` })
  })

  it('opens the chat when nothing is pending', async () => {
    const { replace } = await setup(statusOf({ pending_approvals: 0 }))
    expect(replace).toHaveBeenCalledTimes(1)
    expect(replace).toHaveBeenCalledWith({ name: `${APPID}-chat` })
  })

  it('waits with a spinner for the status and then decides once', async () => {
    const replace = vi.fn(async () => undefined)
    const store = useAgentStore()
    wrapper = mount(Home, { global: { mocks: { $router: { replace } } } })
    await flushPromises()
    expect(wrapper.find('[data-stub=OcSpinner]').exists()).toBe(true)
    expect(replace).not.toHaveBeenCalled()
    store.status = statusOf({ pending_approvals: 1 })
    await flushPromises()
    store.status = statusOf({ pending_approvals: 0 })
    await flushPromises()
    expect(replace).toHaveBeenCalledTimes(1)
    expect(replace).toHaveBeenCalledWith({ name: `${APPID}-approvals` })
  })
})
