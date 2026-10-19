import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Viewer from './Viewer.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr, json } from '../../test/fixtures'
import { fakeServer, startStore } from '../../test/server'
import { MAX_FILE, memoryHandler, memoryState, type MemoryState } from '../../test/memory'
import { useMemoryStore } from '../../stores/memory'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: MemoryState, name: string) => {
  const server = fakeServer(memoryHandler(st))
  const agent = await startStore(server)
  const mem = useMemoryStore()
  await mem.open(name)
  wrapper = mount(Viewer, {
    global: {
      provide: { [PLATFORM_KEY as symbol]: agent.platform },
      mocks: { $router: { push: vi.fn(), beforeEach: vi.fn(() => () => {}) } }
    }
  })
  await flushPromises()
  return { w: wrapper, mem, st, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('Viewer', () => {
  it('shows the name and the content as plain text', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'line 1\nline 2' }), 'notes/a.md')
    expect(w.find('h2').text()).toBe('notes/a.md')
    expect(w.find('pre.agent-text').text()).toBe('line 1\nline 2')
    expect(button(w, 'Edit')).toBeDefined()
  })

  it('renders hostile content as text with control characters visible', async () => {
    const hostile = '<img src=x onerror="alert(1)"><b>bold</b>\u202Egnp.exe\u0007' + 'x'.repeat(5000)
    const { w } = await setup(memoryState({ 'notes/a.md': hostile }), 'notes/a.md')
    const pre = w.find('pre.agent-text')
    expect(pre.find('img').exists()).toBe(false)
    expect(pre.find('b').exists()).toBe(false)
    expect(pre.text()).toContain('<img src=x onerror="alert(1)">')
    expect(pre.findAll('.agent-ctl').map((c) => c.text())).toEqual(['\\u{202E}', '\\u{0007}'])
    expect(pre.attributes('style')).toContain('overflow-wrap: anywhere')
  })

  it('shows a truncated file read only with a banner', async () => {
    const { w } = await setup(memoryState({ 'profile.md': 'a'.repeat(MAX_FILE + 5) }), 'profile.md')
    expect(w.find('.agent-memory-banner').text()).toBe('Showing the first part; this file cannot be edited here.')
    expect(button(w, 'Edit')).toBeUndefined()
  })

  it('offers no edit for a file marked not editable', async () => {
    const st = memoryState()
    st.get = { 'profile.md': json({ name: 'profile.md', content: 'x', etag: '"1"', truncated: false, editable: false }) }
    const { w } = await setup(st, 'profile.md')
    expect(w.find('.agent-memory-banner').text()).toBe('This file cannot be edited here.')
    expect(button(w, 'Edit')).toBeUndefined()
  })

  it('says when a file is too large to open', async () => {
    const st = memoryState()
    st.get = { 'profile.md': apiErr(422, 'file_too_large') }
    const { w } = await setup(st, 'profile.md')
    expect(w.find('[role="alert"]').text()).toBe('The file is too large to open.')
    expect(w.find('pre').exists()).toBe(false)
  })

  it('says when a file does not exist', async () => {
    const { w } = await setup(memoryState(), 'notes/gone.md')
    expect(w.find('.agent-memory-missing').text()).toBe('This file does not exist (any more).')
  })

  it('opens the editor', async () => {
    const { w, mem } = await setup(memoryState({ 'notes/a.md': 'old' }), 'notes/a.md')
    await button(w, 'Edit')!.trigger('click')
    expect(mem.editing).toBe(true)
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('old')
    expect(w.find('pre.agent-memory-content').exists()).toBe(false)
  })
})
