import { afterEach, describe, expect, it } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import ConversationList from './ConversationList.vue'
import { PLATFORM_KEY } from '../../platform'
import { apiErr } from '../../test/fixtures'
import { fakeServer, startStore, type Handler } from '../../test/server'
import { chatHandler, chatState, conversationOf, type ChatState } from '../../test/chat'
import { useChatStore } from '../../stores/chat'

let wrapper: VueWrapper | undefined
let detach: (() => void) | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
  detach?.()
})

const setup = async (st: ChatState, extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? chatHandler(st)(c))
  const { platform, store } = await startStore(server)
  const chat = useChatStore()
  detach = chat.attach()
  await chat.loadConversations()
  wrapper = mount(ConversationList, {
    global: { provide: { [PLATFORM_KEY as symbol]: platform } },
    attachTo: document.body
  })
  await flushPromises()
  return { w: wrapper, chat, server, platform, store }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

describe('ConversationList', () => {
  it('lists titles as text, with a fallback for an empty title, and links each', async () => {
    const { w } = await setup(
      chatState({
        conversations: [
          conversationOf('c1', { title: '<b>bold</b>\u202e' }),
          conversationOf('c2', { title: '' })
        ]
      })
    )
    const items = w.findAll('.agent-conv')
    expect(items).toHaveLength(2)
    expect(w.find('b').exists()).toBe(false)
    expect(items[0].find('.agent-conv-title').text()).toContain('<b>bold</b>')
    expect(items[0].find('.agent-ctl').exists()).toBe(true)
    expect(items[1].find('.agent-conv-title').text()).toBe('New conversation')
    const to = JSON.parse(items[0].find('a').attributes('data-to')!)
    expect(to).toEqual({ name: 'agent-chat-conversation', params: { id: 'c1' } })
    expect(items[0].find('time').exists()).toBe(true)
  })

  it('marks the open conversation', async () => {
    const { w, chat } = await setup(chatState({ conversations: [conversationOf('c1'), conversationOf('c2')] }))
    chat.current = 'c2'
    await flushPromises()
    const links = w.findAll('.agent-conv a')
    expect(links[1].attributes('aria-current')).toBe('page')
    expect(links[0].attributes('aria-current')).toBeUndefined()
  })

  it('shows an empty state', async () => {
    const { w } = await setup(chatState())
    expect(w.text()).toContain('No conversations yet')
  })

  it('shows a load error with a retry', async () => {
    let fail = true
    const st = chatState({ conversations: [conversationOf('c1')] })
    const { w } = await setup(st, (c) => (fail && c.path === '/conversations' ? apiErr(503, 'unavailable') : undefined))
    expect(w.find('[role=alert]').text()).toBe('The agent service is not reachable right now. Try again later.')
    fail = false
    await button(w, 'Try again')!.trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-conv')).toHaveLength(1)
    expect(w.find('[role=alert]').exists()).toBe(false)
  })

  it('loads more with the cursor', async () => {
    const convs = Array.from({ length: 55 }, (_, i) => conversationOf('c' + i))
    const { w } = await setup(chatState({ conversations: convs }))
    expect(w.findAll('.agent-conv')).toHaveLength(50)
    await button(w, 'Load more')!.trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-conv')).toHaveLength(55)
    expect(button(w, 'Load more')).toBeUndefined()
  })

  it('renames inline, with Enter to save and the server limits checked', async () => {
    const st = chatState({ conversations: [conversationOf('c1', { title: 'Old' })] })
    const { w, server } = await setup(st)
    await w.find('button[aria-label="Rename Old"]').trigger('click')
    const input = w.find('.agent-conv-rename input')
    expect((input.element as HTMLInputElement).value).toBe('Old')
    expect(document.activeElement).toBe(input.element)
    await input.setValue('x'.repeat(101))
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(w.find('.agent-conv-rename [role=alert]').text()).toBe('The title can have at most 100 characters.')
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(0)
    await input.setValue('New name')
    await input.trigger('keydown', { key: 'Enter' })
    await flushPromises()
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(1)
    expect(w.find('.agent-conv-rename').exists()).toBe(false)
    expect(w.find('.agent-conv-title').text()).toBe('New name')
  })

  it('does not save a rename on the Enter of an IME composition', async () => {
    const st = chatState({ conversations: [conversationOf('c1', { title: 'Old' })] })
    const { w, server } = await setup(st)
    await w.find('button[aria-label="Rename Old"]').trigger('click')
    const input = w.find('.agent-conv-rename input')
    await input.setValue('漢字')
    await input.trigger('keydown', { key: 'Enter', isComposing: true })
    const commit = new KeyboardEvent('keydown', { key: 'Enter', bubbles: true })
    Object.defineProperty(commit, 'keyCode', { value: 229 })
    input.element.dispatchEvent(commit)
    await flushPromises()
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(0)
    expect(w.find('.agent-conv-rename').exists()).toBe(true)
  })

  it('cancels a rename with Escape or the button', async () => {
    const st = chatState({ conversations: [conversationOf('c1', { title: 'Old' })] })
    const { w, server } = await setup(st)
    await w.find('button[aria-label="Rename Old"]').trigger('click')
    await w.find('.agent-conv-rename input').setValue('changed')
    await w.find('.agent-conv-rename input').trigger('keydown', { key: 'Escape' })
    expect(w.find('.agent-conv-rename').exists()).toBe(false)
    await w.find('button[aria-label="Rename Old"]').trigger('click')
    await button(w, 'Cancel')!.trigger('click')
    expect(w.find('.agent-conv-rename').exists()).toBe(false)
    expect(server.calls.filter((c) => c.method === 'PUT')).toHaveLength(0)
  })

  it('deletes after confirming and tells the parent', async () => {
    const st = chatState({ conversations: [conversationOf('c1', { title: 'Plans' }), conversationOf('c2')] })
    const { w, platform } = await setup(st)
    platform.confirm.mockResolvedValueOnce(false)
    await w.find('button[aria-label="Delete Plans"]').trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-conv')).toHaveLength(2)
    expect(w.emitted('deleted')).toBeUndefined()
    await w.find('button[aria-label="Delete Plans"]').trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-conv')).toHaveLength(1)
    expect(w.emitted('deleted')).toEqual([['c1']])
  })

  it('shows a failed delete as a fixed message', async () => {
    const st = chatState({ conversations: [conversationOf('c1', { title: 'Plans' })] })
    const { w } = await setup(st, (c) => (c.method === 'DELETE' ? apiErr(503, 'unavailable') : undefined))
    await w.find('button[aria-label="Delete Plans"]').trigger('click')
    await flushPromises()
    expect(w.findAll('.agent-conv')).toHaveLength(1)
    expect(w.find('.agent-convs-action-error').text()).toBe(
      'The agent service is not reachable right now. Try again later.'
    )
  })
})
