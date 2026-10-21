import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Memory from './Memory.vue'
import { PLATFORM_KEY } from '../platform'
import { fakeServer, startStore, type Handler } from '../test/server'
import { apiErr, gatewayErr } from '../test/fixtures'
import { memoryHandler, memoryState, setFile, type MemoryState } from '../test/memory'
import { useMemoryStore } from '../stores/memory'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: MemoryState, props: { name?: string } = {}, extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? memoryHandler(st)(c))
  const agent = await startStore(server)
  const push = vi.fn()
  wrapper = mount(Memory, {
    props,
    global: {
      provide: { [PLATFORM_KEY as symbol]: agent.platform },
      mocks: { $router: { push, beforeEach: vi.fn(() => () => {}) } }
    }
  })
  await flushPromises()
  return { w: wrapper, push, server, st, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('Memory', () => {
  it('lists the files and explains editing, activity and deleting', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a', 'profile.md': 'p' }))
    expect(w.find('h1').text()).toBe('Memory')
    expect(w.findAll('.agent-memory-file-name').map((n) => n.text())).toEqual(['profile.md', 'notes/a.md'])
    expect(w.text()).toContain('Changes you make are recorded in Activity.')
    expect(w.text()).toContain('.ocis-agent/memory')
    expect(w.findAll('button').some((b) => /delete/i.test(b.text()))).toBe(false)
  })

  it('shows the file of the route', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'hello' }), { name: 'notes/a.md' })
    expect(w.find('.agent-memory-viewer-name').text()).toBe('notes/a.md')
    expect(w.find('pre.agent-text').text()).toBe('hello')
    expect(w.findAll('button').some((b) => /delete/i.test(b.text()))).toBe(false)
  })

  it('keeps the list visible next to the open file and marks the current row', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a', 'notes/b.md': 'b' }), { name: 'notes/a.md' })
    expect(w.classes()).toContain('agent-memory--open')
    expect(w.findAll('.agent-memory-side .agent-memory-file-name')).toHaveLength(2)
    const current = w.findAll('.agent-memory-file-current')
    expect(current).toHaveLength(1)
    expect(current[0].text()).toContain('notes/a.md')
    expect(current[0].find('a, [aria-current]').attributes('aria-current')).toBe('page')
    expect(w.find('.agent-memory-detail .agent-memory-viewer').exists()).toBe(true)
  })

  it('shows an empty detail pane when nothing is open', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a' }))
    expect(w.classes()).not.toContain('agent-memory--open')
    expect(w.find('.agent-memory-detail').text()).toContain('Choose a file to read or edit it.')
  })

  it('renders the new note form in the detail pane', async () => {
    const { w } = await setup(memoryState())
    await button(w, 'New note')!.trigger('click')
    expect(w.classes()).toContain('agent-memory--open')
    expect(w.find('.agent-memory-detail .agent-memory-new').exists()).toBe(true)
    expect(button(w, 'New note')).toBeUndefined()
  })

  it('switches between files and back to the list', async () => {
    const st = memoryState({ 'notes/a.md': 'A', 'notes/b.md': 'B' })
    const { w } = await setup(st, { name: 'notes/a.md' })
    await w.setProps({ name: 'notes/b.md' })
    await flushPromises()
    expect(w.find('pre.agent-text').text()).toBe('B')
    await w.setProps({ name: undefined })
    await flushPromises()
    expect(w.findAll('.agent-memory-file-name')).toHaveLength(2)
    expect(useMemoryStore().openName).toBeNull()
  })

  it('creates a note and opens it', async () => {
    const st = memoryState()
    const { w, push } = await setup(st)
    await button(w, 'New note')!.trigger('click')
    await w.find('.agent-memory-new input').setValue('trip')
    await w.findAll('.agent-memory-new button').find((b) => b.text() === 'Create')!.trigger('click')
    await flushPromises()
    expect(st.files['notes/trip.md']).toBeDefined()
    expect(push).toHaveBeenCalledWith({ name: 'agent-memory-file', params: { name: ['notes', 'trip.md'] } })
  })

  it('leaves the new-note form when a file is opened from the list', async () => {
    const st = memoryState({ 'notes/a.md': 'A' })
    const { w } = await setup(st)
    await button(w, 'New note')!.trigger('click')
    expect(w.find('.agent-memory-new').exists()).toBe(true)
    await w.setProps({ name: 'notes/a.md' })
    await flushPromises()
    await w.setProps({ name: undefined })
    await flushPromises()
    expect(w.find('.agent-memory-new').exists()).toBe(false)
  })

  it('updates the open file when the agent changed it', async () => {
    const st = memoryState({ 'notes/a.md': 'old' })
    const { w, events, platform } = await setup(st, { name: 'notes/a.md' })
    setFile(st, 'notes/a.md', 'by the agent')
    await events().onChanged(['activity'])
    await flushPromises()
    expect(w.find('pre.agent-text').text()).toBe('by the agent')
    expect(platform.notify).toHaveBeenCalledWith('info', 'Updated by the agent')
  })

  it('stops listening when it is left', async () => {
    const st = memoryState({ 'notes/a.md': 'old' })
    const { w, events, server } = await setup(st)
    w.unmount()
    wrapper = undefined
    const n = server.count('GET', '/memory')
    await events().onChanged(['activity'])
    await flushPromises()
    expect(server.count('GET', '/memory')).toBe(n)
  })
})

describe('Memory without a session', () => {
  it('shows a spinner instead of a blank list', async () => {
    wrapper = mount(Memory, { global: { mocks: { $router: { push: vi.fn(), beforeEach: vi.fn(() => () => {}) } } } })
    await flushPromises()
    expect(wrapper.find('[data-stub=OcSpinner]').exists()).toBe(true)
  })
})

describe('Memory during an outage', () => {
  it('keeps the files and shows no inline error while the service is down', async () => {
    const st = memoryState({ 'notes/a.md': 'a' })
    let down = false
    const { w, store } = await setup(st, {}, (c) => (down && (c.path === '/memory' || c.path === '/status') ? gatewayErr() : undefined))
    down = true
    await useMemoryStore().refreshList()
    await flushPromises()
    expect(store.serviceDown).toBe(true)
    expect(w.find('.agent-memory-error').exists()).toBe(false)
    expect(w.find('[role=alert]').exists()).toBe(false)
    expect(w.findAll('.agent-memory-file-name')).toHaveLength(1)
  })

  it('shows exactly one error card with no data', async () => {
    const { w } = await setup(memoryState(), {}, (c) => (c.path === '/memory' || c.path === '/status' ? gatewayErr() : undefined))
    expect(w.findAll('[role=alert]')).toHaveLength(1)
    expect(w.find('[role=alert] [data-stub=OcIcon]').attributes('data-icon')).toBe('wifi-off')
  })

  it('shows an empty state with an icon when there are no notes', async () => {
    const { w } = await setup(memoryState())
    expect(w.find('.agent-memory-empty [data-stub=OcIcon]').attributes('data-icon')).toBe('file-text')
  })
})
