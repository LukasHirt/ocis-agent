import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import NewFile from './NewFile.vue'
import { PLATFORM_KEY } from '../../platform'
import { fakeServer, startStore } from '../../test/server'
import { memoryHandler, memoryState, type MemoryState } from '../../test/memory'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: MemoryState = memoryState()) => {
  const server = fakeServer(memoryHandler(st))
  const agent = await startStore(server)
  wrapper = mount(NewFile, { global: { provide: { [PLATFORM_KEY as symbol]: agent.platform } } })
  await flushPromises()
  return { w: wrapper, st, server }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)!

describe('NewFile', () => {
  it('creates notes/<name>.md with the content and emits its name', async () => {
    const { w, st } = await setup()
    await w.find('input').setValue('trip-2026')
    expect(w.find('.agent-memory-new-name').text()).toBe('notes/trip-2026.md')
    await w.find('textarea').setValue('plan')
    await button(w, 'Create').trigger('click')
    await flushPromises()
    expect(st.files['notes/trip-2026.md'].content).toBe('plan')
    expect(w.emitted('created')).toEqual([['notes/trip-2026.md']])
  })

  it('explains an invalid name as typed and sends nothing', async () => {
    const { w, server } = await setup()
    await w.find('input').setValue('Trip 1')
    expect(w.find('[role="alert"]').text()).toBe(
      'Use 1 to 64 lower-case letters, digits and hyphens, starting with a letter or digit.'
    )
    expect(button(w, 'Create').attributes('disabled')).toBeDefined()
    await w.find('input').setValue('a'.repeat(65))
    expect(w.find('[role="alert"]').text()).toBe('A name can have at most 64 characters.')
    expect(server.calls.filter((c) => c.method === 'POST')).toHaveLength(0)
  })

  it('needs a name', async () => {
    const { w } = await setup()
    expect(button(w, 'Create').attributes('disabled')).toBeDefined()
  })

  it('says when the note exists already', async () => {
    const { w } = await setup(memoryState({ 'notes/trip.md': 'x' }))
    await w.find('input').setValue('trip')
    await button(w, 'Create').trigger('click')
    await flushPromises()
    expect(w.find('.agent-memory-new-error').text()).toBe('A file with this name already exists.')
    expect(w.emitted('created')).toBeUndefined()
  })

  it('can be cancelled', async () => {
    const { w } = await setup()
    await button(w, 'Cancel').trigger('click')
    expect(w.emitted('cancel')).toHaveLength(1)
  })
})
