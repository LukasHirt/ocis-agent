import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Activity from './Activity.vue'
import { PLATFORM_KEY } from '../platform'
import { apiErr } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import { auditHandler, auditState, eventOf, eventsUpTo, type AuditState } from '../test/audit'
import { AUDIT_PAGE, useAuditStore } from '../stores/audit'

const saved = vi.hoisted(() => [] as Array<{ blob: Blob; filename: string }>)
vi.mock('../util/download', () => ({
  saveBlob: (blob: Blob, filename: string) => {
    saved.push({ blob, filename })
  }
}))

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  saved.length = 0
})

const setup = async (st: AuditState = auditState(), extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? auditHandler(st)(c))
  const agent = await startStore(server)
  wrapper = mount(Activity, {
    global: { provide: { [PLATFORM_KEY as symbol]: agent.platform }, mocks: { $router: { push: vi.fn() } } }
  })
  await flushPromises()
  return { w: wrapper, server, st, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)
const kinds = (w: VueWrapper) => w.findAll('.agent-event-kind').map((k) => k.text())

describe('Activity', () => {
  it('lists the events newest first', async () => {
    const events = [eventOf(1, { kind: 'chat_message' }), eventOf(2, { kind: 'job_finished' })]
    const { w } = await setup(auditState({ events }))
    expect(w.find('h1').text()).toBe('Activity')
    expect(kinds(w)).toEqual(['Task ended', 'Chat question'])
  })

  it('says when there is no activity', async () => {
    const { w } = await setup()
    expect(w.find('.agent-activity-empty').text()).toBe('No activity yet.')
  })

  it('loads older events', async () => {
    const { w } = await setup(auditState({ events: eventsUpTo(AUDIT_PAGE + 5) }))
    expect(w.findAll('.agent-event')).toHaveLength(AUDIT_PAGE)
    await button(w, 'Load older')!.trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-event')).toHaveLength(AUDIT_PAGE + 5)
    expect(button(w, 'Load older')).toBeUndefined()
  })

  it('asks the server for the events of one kind', async () => {
    const events = [
      ...eventsUpTo(AUDIT_PAGE + 10),
      eventOf(AUDIT_PAGE + 11, { kind: 'chat_message' }),
      eventOf(AUDIT_PAGE + 12, { kind: 'job_finished' })
    ]
    events[0] = eventOf(1, { kind: 'chat_message' }) // older than the first page
    const { w, server } = await setup(auditState({ events }))
    const select = w.find('[data-testid="agent-activity-kind"] select')
    const labels = select.findAll('option').map((o) => o.text())
    expect(labels[0]).toBe('All events')
    expect(labels).toContain('Chat question')
    expect(labels).toContain('Outcome check confirmed by you')
    await select.setValue(String(labels.indexOf('Chat question')))
    await flushPromises()
    expect(kinds(w)).toEqual(['Chat question', 'Chat question'])
    expect(server.calls.some((c) => c.path === '/audit' && c.query.get('kind') === 'chat_message')).toBe(true)
    expect(w.text()).not.toContain('Only the events loaded so far')
    await select.setValue(String(labels.indexOf('Approval expired')))
    await flushPromises()
    expect(w.find('.agent-activity-empty').text()).toBe('No events of this kind.')
  })

  it('shows an error with a retry', async () => {
    let fail = true
    const { w } = await setup(auditState({ events: eventsUpTo(1) }), (c) =>
      fail && c.path === '/audit' ? apiErr(500, 'internal') : undefined
    )
    expect(w.find('[role="alert"] .agent-state-title').text()).toBe('Something went wrong in the agent service.')
    fail = false
    await button(w, 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-event')).toHaveLength(1)
  })

  it('adds new events on an activity change', async () => {
    const st = auditState({ events: eventsUpTo(1) })
    const { w, events } = await setup(st)
    st.events.push(eventOf(2, { kind: 'agent_paused' }))
    await events().onChanged(['activity'])
    await flushPromises()
    expect(kinds(w)[0]).toBe('Agent paused')
    expect(w.findAll('.agent-event')).toHaveLength(2)
  })

  it('exports the log as a file', async () => {
    const { w } = await setup(auditState({ events: eventsUpTo(2) }))
    await button(w, 'Export')!.trigger('click')
    await flushPromises()
    expect(saved.map((s) => s.filename)).toEqual(['audit-0a1b2c3d.jsonl'])
  })

  it('shows the progress of an export, can cancel it and aborts it when the screen is left', async () => {
    let push!: ReadableStreamDefaultController<Uint8Array>
    const signals: AbortSignal[] = []
    const st = auditState({
      exportRes: () =>
        new Response(
          new ReadableStream<Uint8Array>({
            start(c) {
              push = c
            }
          }),
          { status: 200 }
        )
    })
    const { w, server } = await setup(st)
    server.fetch.mockImplementationOnce(async (input, init) => {
      if (init?.signal) signals.push(init.signal)
      return st.exportRes!()
    })
    await button(w, 'Export')!.trigger('click')
    await flushPromises()
    push.enqueue(new TextEncoder().encode('x'.repeat(2047) + '\n'))
    await flushPromises()
    expect(w.find('.agent-activity-export-progress').text()).toBe('Downloading… 2 KiB')
    expect(button(w, 'Export')).toBeUndefined()
    expect(button(w, 'Cancel export')).toBeDefined()

    w.unmount()
    wrapper = undefined
    await flushPromises()
    expect(signals[0].aborted).toBe(true)
    expect(saved).toHaveLength(0)
  })

  it('cancels an export on request', async () => {
    const st = auditState({
      exportRes: () => new Response(new ReadableStream<Uint8Array>({ start() {} }), { status: 200 })
    })
    const { w } = await setup(st)
    await button(w, 'Export')!.trigger('click')
    await flushPromises()
    await button(w, 'Cancel export')!.trigger('click')
    await flushPromises()
    expect(button(w, 'Export')).toBeDefined()
    expect(saved).toHaveLength(0)
    expect(w.find('.agent-activity-export-error').exists()).toBe(false)
  })

  it('never shows the events of an earlier session, not even for the first render', async () => {
    const st = auditState({ events: eventsUpTo(3) })
    const { w, store, server, platform } = await setup(st)
    expect(kinds(w)).toHaveLength(3)
    w.unmount()
    wrapper = undefined
    store.stop()
    st.events = []
    await startStore(server, platform)
    wrapper = mount(Activity, {
      global: { provide: { [PLATFORM_KEY as symbol]: platform }, mocks: { $router: { push: vi.fn() } } }
    })
    expect(kinds(wrapper)).toHaveLength(0)
  })
})

describe('Activity without a session', () => {
  it('shows a spinner instead of a blank screen', async () => {
    wrapper = mount(Activity, { global: { mocks: { $router: { push: vi.fn() } } } })
    await flushPromises()
    expect(wrapper.find('[data-stub=OcSpinner]').exists()).toBe(true)
  })
})

describe('Activity during an outage', () => {
  it('keeps the events and shows no inline error while the service is down', async () => {
    let down = false
    const { w, store } = await setup(auditState({ events: eventsUpTo(1) }), (c) =>
      down && c.path === '/audit' ? apiErr(503, 'unavailable') : undefined
    )
    down = true
    await useAuditStore().refresh()
    await flushPromises()
    expect(store.serviceDown).toBe(true)
    expect(w.find('.agent-activity-error').exists()).toBe(false)
    expect(w.find('[role=alert]').exists()).toBe(false)
    expect(w.findAll('.agent-event-kind')).toHaveLength(1)
  })

  it('shows exactly one error card with no data', async () => {
    const { w } = await setup(auditState(), (c) => (c.path === '/audit' ? apiErr(503, 'unavailable') : undefined))
    expect(w.findAll('[role=alert]')).toHaveLength(1)
    expect(w.find('[role=alert] .agent-state-title').text()).toBe('The agent service is not reachable right now. Try again later.')
    expect(w.find('[role=alert] [data-stub=OcIcon]').attributes('data-icon')).toBe('wifi-off')
  })

  it('shows an empty state with an icon', async () => {
    const { w } = await setup()
    expect(w.find('.agent-activity-empty [data-stub=OcIcon]').attributes('data-icon')).toBe('history')
  })
})
