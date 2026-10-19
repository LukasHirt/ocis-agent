import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Editor from './Editor.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr } from '../../test/fixtures'
import { fakeServer, startStore } from '../../test/server'
import { memoryHandler, memoryState, setFile, type MemoryState } from '../../test/memory'
import { useMemoryStore } from '../../stores/memory'

let wrapper: VueWrapper | undefined
let detach: (() => void) | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  detach?.()
  detach = undefined
})

type Guard = (to: { fullPath: string }, from: { fullPath: string }) => boolean | Promise<boolean>

const setup = async (st: MemoryState = memoryState({ 'notes/a.md': 'old' })) => {
  const server = fakeServer(memoryHandler(st))
  const agent = await startStore(server)
  const mem = useMemoryStore()
  detach = mem.attach()
  await mem.refreshList()
  await mem.open('notes/a.md')
  mem.startEdit()
  const removeGuard = vi.fn()
  const guards: Guard[] = []
  const beforeEach = vi.fn((g: Guard) => {
    guards.push(g)
    return removeGuard
  })
  wrapper = mount(Editor, {
    global: { provide: { [PLATFORM_KEY as symbol]: agent.platform }, mocks: { $router: { push: vi.fn(), beforeEach } } }
  })
  await flushPromises()
  return { w: wrapper, mem, st, server, guards, removeGuard, beforeEach, ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)
const type = async (w: VueWrapper, text: string) => {
  await w.find('textarea').setValue(text)
}

describe('Editor', () => {
  it('edits the content and saves it', async () => {
    const { w, st, mem } = await setup()
    expect(w.find('label').text()).toBe('Content of notes/a.md')
    expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
    await type(w, 'new')
    expect(button(w, 'Save')!.attributes('disabled')).toBeUndefined()
    await button(w, 'Save')!.trigger('click')
    await flushPromises()
    expect(st.files['notes/a.md'].content).toBe('new')
    expect(mem.editing).toBe(false)
  })

  it('does not submit on Enter', async () => {
    const { w } = await setup()
    await type(w, 'new')
    await w.find('textarea').trigger('keydown', { key: 'Enter' })
    expect(w.findComponent({ name: 'OcTextarea' }).emitted('change')).toBeUndefined()
  })

  it('counts the size with the CRLF line endings that are saved', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a\r\n' }))
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('a\n')
    expect(w.find('.agent-memory-size').text()).toBe('3 B of 64 KiB')
    expect(w.find('.agent-memory-eol').exists()).toBe(false)
  })

  it('warns that mixed line endings are saved as LF', async () => {
    const { w } = await setup(memoryState({ 'notes/a.md': 'a\r\nb\n' }))
    expect(w.find('.agent-memory-eol').text()).toBe(
      'This file mixes line endings. Saving it changes them all to LF (Unix style).'
    )
  })

  it('shows the size used of the limit', async () => {
    const { w } = await setup()
    await type(w, '€€')
    expect(w.find('.agent-memory-size').text()).toBe('6 B of 64 KiB')
  })

  it('explains a NUL character while typing and blocks saving', async () => {
    const { w } = await setup()
    await type(w, 'a\u0000')
    expect(w.find('textarea').attributes('aria-invalid')).toBe('true')
    expect(w.find('.agent-memory-draft').text()).toContain(
      'The text contains a NUL character, which a memory file cannot hold.'
    )
    expect(button(w, 'Save')!.attributes('disabled')).toBeDefined()
  })

  it('shows the size message on a 413 and keeps the text', async () => {
    const { w, st } = await setup()
    st.put = apiErr(413, 'too_large')
    await type(w, 'new')
    await button(w, 'Save')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-memory-save-error').text()).toBe(
      'A memory file can hold at most 64 KiB. Shorten the text and save again.'
    )
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('new')
  })

  it('shows the 428 message and keeps the text', async () => {
    const { w, st } = await setup()
    st.put = apiErr(428, 'precondition_required')
    await type(w, 'new')
    await button(w, 'Save')!.trigger('click')
    await flushPromises()
    expect(w.find('.agent-memory-save-error').text()).toBe('The file version is missing. Reload the file and try again.')
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('new')
  })

  describe('a conflict', () => {
    const conflicted = async () => {
      const s = await setup()
      setFile(s.st, 'notes/a.md', 'theirs <b>x</b>')
      await type(s.w, 'mine')
      await button(s.w, 'Save')!.trigger('click')
      await flushPromises()
      return s
    }

    it('shows your version and the current one side by side', async () => {
      const { w } = await conflicted()
      const view = w.find('.agent-memory-conflict')
      expect(view.find('[role="alert"]').text()).toBe(
        'The file changed since you opened it. Compare the versions and choose which to keep.'
      )
      const cols = view.findAll('.agent-memory-version')
      expect(cols.map((c) => c.find('h3').text())).toEqual(['Your version', 'Current version'])
      expect(cols.map((c) => c.find('pre').text())).toEqual(['mine', 'theirs <b>x</b>'])
      expect(view.find('b').exists()).toBe(false)
      expect(w.find('textarea').exists()).toBe(false)
    })

    it('overwrites with mine after a confirm', async () => {
      const { w, st, platform } = await conflicted()
      await button(w, 'Overwrite with mine')!.trigger('click')
      await flushPromises()
      expect(platform.confirm).toHaveBeenCalledTimes(1)
      expect(st.files['notes/a.md'].content).toBe('mine')
    })

    it('uses the current version', async () => {
      const { w, mem } = await conflicted()
      await button(w, 'Use current')!.trigger('click')
      await flushPromises()
      expect(mem.editing).toBe(false)
      expect(mem.file!.content).toBe('theirs <b>x</b>')
    })

    it('goes back to the draft to keep editing and compares again on save', async () => {
      const { w, st } = await conflicted()
      await button(w, 'Keep editing')!.trigger('click')
      expect(w.find('.agent-memory-conflict').exists()).toBe(false)
      expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('mine')
      expect(w.find('.agent-memory-changed').exists()).toBe(true)
      await type(w, 'merged')
      await button(w, 'Save')!.trigger('click')
      await flushPromises()
      expect(st.files['notes/a.md'].content).toBe('theirs <b>x</b>')
      expect(w.find('.agent-memory-conflict').exists()).toBe(true)
    })

    it('offers to create a deleted file again', async () => {
      const s = await setup()
      delete s.st.files['notes/a.md']
      s.st.put = apiErr(404, 'not_found')
      await type(s.w, 'mine')
      await button(s.w, 'Save')!.trigger('click')
      await flushPromises()
      s.st.put = undefined
      expect(s.w.find('.agent-memory-conflict [role="alert"]').text()).toBe(
        'The file was deleted since you opened it. Your version is kept below.'
      )
      expect(s.w.find('.agent-memory-version pre').text()).toBe('mine')
      await button(s.w, 'Create it again with my version')!.trigger('click')
      await flushPromises()
      expect(s.st.files['notes/a.md'].content).toBe('mine')
    })
  })

  it('shows a banner when the file changed on the server while editing', async () => {
    const { w, st, events } = await setup()
    await type(w, 'mine')
    setFile(st, 'notes/a.md', 'by the agent')
    await events().onChanged(['activity'])
    await flushPromises()
    expect(w.find('.agent-memory-changed').text()).toBe(
      'Changed on the server. Your text is kept; saving shows both versions so you can choose.'
    )
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('mine')
  })

  it('asks before discarding changes on cancel', async () => {
    const { w, mem, platform } = await setup()
    await type(w, 'mine')
    platform.confirm.mockResolvedValueOnce(false)
    await button(w, 'Cancel')!.trigger('click')
    await flushPromises()
    expect(mem.editing).toBe(true)
    await button(w, 'Cancel')!.trigger('click')
    await flushPromises()
    expect(mem.editing).toBe(false)
  })

  describe('the leave guard', () => {
    it('asks on the host router before leaving with changes', async () => {
      const { w, guards, platform } = await setup()
      expect(guards).toHaveLength(1)
      const guard = guards[0]
      expect(await guard({ fullPath: '/agent/tasks' }, { fullPath: '/agent/memory/notes/a.md' })).toBe(true)
      expect(platform.confirm).not.toHaveBeenCalled()
      await type(w, 'mine')
      platform.confirm.mockResolvedValueOnce(false)
      expect(await guard({ fullPath: '/agent/tasks' }, { fullPath: '/agent/memory/notes/a.md' })).toBe(false)
      expect(await guard({ fullPath: '/agent/tasks' }, { fullPath: '/agent/memory/notes/a.md' })).toBe(true)
      // a navigation that stays on the same location is not a leave
      const n = platform.confirm.mock.calls.length
      expect(await guard({ fullPath: '/agent/memory/notes/a.md' }, { fullPath: '/agent/memory/notes/a.md' })).toBe(true)
      expect(platform.confirm.mock.calls.length).toBe(n)
    })

    it('lets the host sign-in and sign-out routes through, and asks for every other route', async () => {
      const { w, guards, platform } = await setup()
      await type(w, 'mine')
      const from = { fullPath: '/agent/memory/notes/a.md' }
      for (const name of ['logout', 'login', 'oidcCallback', 'oidcSilentRedirect', 'accessDenied']) {
        expect(await guards[0]({ name, fullPath: '/' + name } as never, from)).toBe(true)
      }
      expect(platform.confirm).not.toHaveBeenCalled()
      platform.confirm.mockResolvedValueOnce(false)
      expect(await guards[0]({ name: 'account', fullPath: '/account' } as never, from)).toBe(false)
      expect(platform.confirm).toHaveBeenCalledTimes(1)
    })

    it('removes the guard when the editor closes', async () => {
      const { w, removeGuard } = await setup()
      w.unmount()
      wrapper = undefined
      expect(removeGuard).toHaveBeenCalledTimes(1)
    })

    it('asks the browser before unloading with changes', async () => {
      const { w } = await setup()
      const clean = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(clean)
      expect(clean.defaultPrevented).toBe(false)
      await type(w, 'mine')
      const dirty = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(dirty)
      expect(dirty.defaultPrevented).toBe(true)
      w.unmount()
      wrapper = undefined
      const after = new Event('beforeunload', { cancelable: true })
      window.dispatchEvent(after)
      expect(after.defaultPrevented).toBe(false)
    })
  })
})
