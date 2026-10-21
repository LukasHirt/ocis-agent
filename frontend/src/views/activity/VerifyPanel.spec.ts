import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import VerifyPanel from './VerifyPanel.vue'
import { json } from '../../test/fixtures'
import { fakeServer, startStore } from '../../test/server'
import { auditHandler, auditState, eventsUpTo, type AuditState } from '../../test/audit'
import { errWithRetry } from '../../test/chat'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: AuditState = auditState()) => {
  const server = fakeServer(auditHandler(st))
  await startStore(server)
  wrapper = mount(VerifyPanel)
  await flushPromises()
  return { w: wrapper, server }
}

const check = async (w: VueWrapper) => {
  await w.find('button').trigger('click')
  await flushPromises()
}

describe('VerifyPanel', () => {
  it('always says what the check cannot detect', async () => {
    const { w } = await setup()
    expect(w.find('.agent-verify-note').text()).toBe('This check cannot detect removal of the newest events.')
  })

  it('reports an intact log with the number of verified events', async () => {
    const { w, server } = await setup(auditState({ events: eventsUpTo(12) }))
    expect(server.count('GET', '/audit/verify')).toBe(0) // only on request
    await check(w)
    expect(w.find('[role="status"]').text()).toBe('Verified 12 events: no recorded event was changed.')
    expect(w.find('[role="status"]').classes()).toContain('agent-chip--success')
  })

  it('counts a single verified event', async () => {
    const { w } = await setup(auditState({ events: eventsUpTo(1) }))
    await check(w)
    expect(w.find('[role="status"]').text()).toBe('Verified 1 event: it was not changed.')
  })

  it('says where the chain breaks with a fixed text, never the server message', async () => {
    const { w } = await setup(
      auditState({ verify: json({ ok: false, message: 'SERVER TEXT', limitation: 'x', checked: 41, broken_at: 42 }) })
    )
    await check(w)
    const s = w.find('[role="status"]').text()
    expect(s).toBe(
      'The chain breaks at event 42: that event was changed, or events before it were removed. The 41 events before it are intact.'
    )
    expect(w.find('.agent-verify-broken').exists()).toBe(true)
    expect(w.text()).not.toContain('SERVER TEXT')
    expect(w.text()).not.toMatch(/corrupt/i)
  })

  it('says only where the chain breaks when no event before it verified', async () => {
    const { w } = await setup(auditState({ verify: json({ ok: false, message: 'm', limitation: 'x', checked: 0, broken_at: 1 }) }))
    await check(w)
    expect(w.find('[role="status"]').text()).toBe(
      'The chain breaks at event 1: that event was changed, or events before it were removed.'
    )
  })

  it('reports a broken chain without a position', async () => {
    const { w } = await setup(auditState({ verify: json({ ok: false, message: 'm', limitation: 'x', checked: 0 }) }))
    await check(w)
    expect(w.find('[role="status"]').text()).toBe('The chain is broken: events in the log were changed or removed.')
  })

  it('shows when to try again after a 429', async () => {
    const { w } = await setup(auditState({ verify: errWithRetry(429, 'rate_limited', 5) }))
    await check(w)
    expect(w.find('[role="alert"]').text()).toBe('A check is already running. Try again in 5s.')
  })

  it('disables the button while checking', async () => {
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const server = fakeServer((c) =>
      c.path === '/audit/verify' ? gate.then(() => json({ ok: true, limitation: 'x', checked: 0 })) : undefined
    )
    await startStore(server)
    wrapper = mount(VerifyPanel)
    await wrapper.find('button').trigger('click')
    await flushPromises()
    expect(wrapper.find('button').attributes('disabled')).toBeDefined()
    release()
    await flushPromises()
    expect(wrapper.find('button').attributes('disabled')).toBeUndefined()
  })
})
