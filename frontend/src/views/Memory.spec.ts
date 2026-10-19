import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Memory from './Memory.vue'
import { PLATFORM_KEY } from '../platform'
import { fakeServer, startStore } from '../test/server'
import { memoryHandler, memoryState, setFile, type MemoryState } from '../test/memory'
import { useMemoryStore } from '../stores/memory'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: MemoryState, props: { name?: string } = {}) => {
  const server = fakeServer(memoryHandler(st))
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
    expect(w.find('h2').text()).toBe('notes/a.md')
    expect(w.find('pre.agent-text').text()).toBe('hello')
    expect(w.find('.agent-memory-file-name').exists()).toBe(false)
    expect(w.findAll('button').some((b) => /delete/i.test(b.text()))).toBe(false)
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
    await button(w, 'Create')!.trigger('click')
    await flushPromises()
    expect(st.files['notes/trip.md']).toBeDefined()
    expect(push).toHaveBeenCalledWith({ name: 'agent-memory-file', params: { name: ['notes', 'trip.md'] } })
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
