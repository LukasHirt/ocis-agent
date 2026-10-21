import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import FileList from './FileList.vue'
import { PLATFORM_KEY } from '../../platform'
import { fakeServer, startStore } from '../../test/server'
import { MAX_FILE, memoryHandler, memoryState, type MemoryState } from '../../test/memory'
import { useMemoryStore } from '../../stores/memory'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: MemoryState) => {
  const server = fakeServer(memoryHandler(st))
  const agent = await startStore(server)
  const mem = useMemoryStore()
  await mem.refreshList()
  const push = vi.fn()
  wrapper = mount(FileList, {
    global: { provide: { [PLATFORM_KEY as symbol]: agent.platform }, mocks: { $router: { push } } }
  })
  await flushPromises()
  return { w: wrapper, push, server, mem, ...agent }
}

const rowNames = (w: VueWrapper, group: string) =>
  w.findAll(`[data-testid="${group}"] .agent-memory-file-name`).map((n) => n.text())

describe('FileList', () => {
  it('does not create a missing core file while a draft is unsaved', async () => {
    const { w, mem, server } = await setup(memoryState({ 'notes/a.md': 'a' }))
    const create = () => w.findAll('[data-testid="agent-memory-core"] button').find((b) => b.text() === 'Create')!
    expect(create().attributes('disabled')).toBeUndefined()
    await mem.open('notes/a.md')
    mem.startEdit()
    mem.draft = 'changed'
    await flushPromises()
    expect(create().attributes('disabled')).toBeDefined()
    await create().trigger('click')
    expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  })

  it('groups profile and projects before the notes', async () => {
    const st = memoryState({ 'notes/b.md': 'b', 'projects.md': 'p', 'notes/a.md': 'a', 'profile.md': 'x' })
    const { w } = await setup(st)
    expect(rowNames(w, 'agent-memory-core')).toEqual(['profile.md', 'projects.md'])
    expect(rowNames(w, 'agent-memory-notes')).toEqual(['notes/a.md', 'notes/b.md'])
  })

  it('links every file to its route', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a' }))
    const link = w.find('[data-testid="agent-memory-notes"] a')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-memory-file', params: { name: ['notes', 'a.md'] } })
  })

  it('shows size and marks a file too large to edit', async () => {
    const st = memoryState({ 'profile.md': 'abc', 'projects.md': 'x' })
    st.files['projects.md'].size = MAX_FILE + 1
    const { w } = await setup(st)
    const rows = w.findAll('[data-testid="agent-memory-core"] li')
    expect(rows[0].text()).toContain('3 B')
    expect(rows[0].text()).not.toContain('Read only')
    expect(rows[1].text()).toContain('Read only: too large to edit here')
  })

  it('says when there are no notes and offers to create a missing core file', async () => {
    const st = memoryState({ 'projects.md': 'p' })
    const { w, push } = await setup(st)
    expect(w.find('.agent-memory-empty').text()).toBe('No notes yet.')
    const missing = w.find('[data-testid="agent-memory-missing"]')
    expect(missing.text()).toContain('profile.md')
    await missing.find('button').trigger('click')
    await flushPromises()
    expect(st.files['profile.md'].content).toBe('')
    expect(push).toHaveBeenCalledWith({ name: 'agent-memory-file', params: { name: ['profile.md'] } })
  })
})
