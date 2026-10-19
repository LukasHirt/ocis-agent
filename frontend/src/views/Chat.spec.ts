import { afterEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import Chat from './Chat.vue'
import { PLATFORM_KEY } from '../platform'
import { apiErr, json, statusOf } from '../test/fixtures'
import { fakeServer, startStore, type Handler } from '../test/server'
import {
  answer,
  chatHandler,
  chatState,
  conversationOf,
  errWithRetry,
  userMessage,
  type ChatState
} from '../test/chat'
import { useChatStore } from '../stores/chat'

let wrapper: VueWrapper | undefined
afterEach(() => {
  wrapper?.unmount()
  wrapper = undefined
})

const setup = async (st: ChatState, props: { id?: string } = {}, extra?: Handler) => {
  const server = fakeServer((c) => extra?.(c) ?? chatHandler(st)(c))
  const agent = await startStore(server)
  const push = vi.fn()
  wrapper = mount(Chat, {
    props,
    global: { provide: { [PLATFORM_KEY as symbol]: agent.platform }, mocks: { $router: { push } } },
    attachTo: document.body
  })
  await flushPromises()
  return { w: wrapper, server, push, chat: useChatStore(), ...agent }
}

const button = (w: VueWrapper, label: string) => w.findAll('button').find((b) => b.text() === label)

const withThread = () =>
  chatState({
    conversations: [conversationOf('c1', { title: 'Plans' }), conversationOf('c2', { title: 'Other' })],
    messages: {
      c1: [answer('m2', 'Here you go'), userMessage('m1', { id: 'j1', state: 'succeeded', error: '' }, 'Summarise')],
      c2: []
    }
  })

describe('Chat', () => {
  it('lists the conversations and asks to pick one without an id', async () => {
    const { w } = await setup(withThread())
    expect(w.findAll('.agent-conv')).toHaveLength(2)
    expect(w.text()).toContain('Choose a conversation or start a new one.')
    expect(w.find('textarea').exists()).toBe(false)
  })

  it('opens the conversation of the route and shows its thread', async () => {
    const { w } = await setup(withThread(), { id: 'c1' })
    expect(w.find('.agent-chat-title').text()).toBe('Plans')
    expect(w.findAll('.agent-msg-content').map((m) => m.text())).toEqual(['Summarise', 'Here you go'])
    expect(w.find('textarea').exists()).toBe(true)
  })

  it('switches the thread when the route id changes', async () => {
    const { w } = await setup(withThread(), { id: 'c1' })
    await w.setProps({ id: 'c2' })
    await flushPromises()
    expect(w.find('.agent-chat-title').text()).toBe('Other')
    expect(w.text()).toContain('No messages yet')
  })

  it('shows a missing conversation', async () => {
    const { w } = await setup(withThread(), { id: 'nope' })
    expect(w.text()).toContain('This conversation no longer exists.')
    expect(w.find('textarea').exists()).toBe(false)
  })

  it('creates a conversation and navigates to it', async () => {
    const { w, push } = await setup(withThread())
    await button(w, 'New conversation')!.trigger('click')
    await flushPromises()
    expect(push).toHaveBeenCalledWith({ name: 'agent-chat-conversation', params: { id: 'c100' } })
  })

  it('shows the conversation limit when creating is refused', async () => {
    const { w, push } = await setup(withThread(), {}, (c) =>
      c.method === 'POST' && c.path === '/conversations' ? apiErr(409, 'limit_reached') : undefined
    )
    await button(w, 'New conversation')!.trigger('click')
    await flushPromises()
    expect(push).not.toHaveBeenCalled()
    expect(w.find('[role=alert]').text()).toBe(
      'You have 200 conversations, the most there can be. Delete one to start a new one.'
    )
  })

  it('sends the draft, clears it, shows the server message and keeps the focus', async () => {
    const st = withThread()
    const { w, server } = await setup(st, { id: 'c2' })
    const ta = w.find('textarea')
    await ta.setValue('What changed?')
    ;(ta.element as HTMLTextAreaElement).focus()
    ta.element.dispatchEvent(new KeyboardEvent('keydown', { key: 'Enter', cancelable: true }))
    await flushPromises()
    expect(server.calls.find((c) => c.method === 'POST')!.body).toEqual({ content: 'What changed?' })
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('')
    expect(w.findAll('.agent-msg-content').map((m) => m.text())).toEqual(['What changed?'])
    expect(w.find('.agent-msg-status').text()).toContain('Waiting')
    expect(document.activeElement).toBe(w.find('textarea').element)
  })

  it('brings the sent message into view even when the reader had scrolled up', async () => {
    const proto = HTMLElement.prototype
    const saved = Object.getOwnPropertyDescriptor(proto, 'scrollHeight')
    Object.defineProperty(proto, 'scrollHeight', {
      configurable: true,
      get(this: HTMLElement) {
        return this.querySelectorAll('.agent-msg').length * 200
      }
    })
    try {
      const { w } = await setup(withThread(), { id: 'c1' })
      const el = w.find('.agent-thread-scroll').element as HTMLElement
      el.scrollTop = 0
      el.dispatchEvent(new Event('scroll'))
      await w.find('textarea').setValue('next question')
      await w.find('button.agent-composer-send').trigger('click')
      await flushPromises()
      expect(w.findAll('.agent-msg')).toHaveLength(3)
      expect(el.scrollTop).toBe(600)
    } finally {
      if (saved) Object.defineProperty(proto, 'scrollHeight', saved)
      else delete (proto as unknown as Record<string, unknown>).scrollHeight
    }
  })

  it('keeps the draft per conversation in memory', async () => {
    const { w } = await setup(withThread(), { id: 'c1' })
    await w.find('textarea').setValue('draft one')
    await w.setProps({ id: 'c2' })
    await flushPromises()
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('')
    await w.setProps({ id: 'c1' })
    await flushPromises()
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('draft one')
  })

  it('keeps the draft and shows the Retry-After on a refused send', async () => {
    const st = withThread()
    st.failSend = errWithRetry(429, 'rate_limited', 3)
    const { w } = await setup(st, { id: 'c2' })
    await w.find('textarea').setValue('again')
    await w.find('button.agent-composer-send').trigger('click')
    await flushPromises()
    expect((w.find('textarea').element as HTMLTextAreaElement).value).toBe('again')
    expect(w.find('.agent-composer [role=alert]').text()).toBe(
      'You are sending messages too quickly. Try again in 3s.'
    )
  })

  it('offers to enable the agent after a not_enrolled refusal', async () => {
    const st = withThread()
    st.failSend = apiErr(409, 'not_enrolled')
    const { w, server } = await setup(st, { id: 'c2' })
    await w.find('textarea').setValue('hi')
    await w.find('button.agent-composer-send').trigger('click')
    await flushPromises()
    expect(w.find('textarea').attributes('disabled')).toBeDefined()
    await button(w, 'Enable agent')!.trigger('click')
    await flushPromises()
    expect(server.calls.some((c) => c.method === 'POST' && c.path === '/enroll')).toBe(true)
  })

  it('disables sending while the status says the agent is not set up', async () => {
    const { w, store } = await setup(withThread(), { id: 'c2' })
    store.status = statusOf({ enrolled: false })
    await flushPromises()
    expect(w.find('textarea').attributes('disabled')).toBeDefined()
    expect(w.find('.agent-composer-hint').text()).toBe('Set up the agent to ask it questions.')
  })

  it('updates the thread on change events while mounted only', async () => {
    const st = withThread()
    const { w, events, server } = await setup(st, { id: 'c2' })
    st.messages.c2.unshift(answer('m9', 'live answer'))
    await events().onChanged(['messages'])
    await flushPromises()
    expect(w.text()).toContain('live answer')
    w.unmount()
    wrapper = undefined
    const n = server.calls.length
    await events().onChanged(['messages'])
    await flushPromises()
    expect(server.calls.length).toBe(n)
  })

  it('navigates back to the list after deleting the open conversation', async () => {
    const { w, push } = await setup(withThread(), { id: 'c1' })
    await w.find('button[aria-label="Delete Plans"]').trigger('click')
    await flushPromises()
    expect(push).toHaveBeenCalledWith({ name: 'agent-chat' })
  })

  it('shows the inline approvals of a waiting question', async () => {
    const st = chatState({
      conversations: [conversationOf('c1')],
      messages: { c1: [userMessage('m1', { id: 'j1', state: 'waiting_approval', error: '' })] }
    })
    const { w } = await setup(st, { id: 'c1' }, (c) =>
      c.path === '/approvals' && c.query.get('job_id') === 'j1'
        ? json({
            approvals: [
              {
                id: 'a1',
                job_id: 'j1',
                tool: 'files.write',
                risk: 'write',
                state: 'pending'
              }
            ],
            next_cursor: null
          })
        : undefined
    )
    expect(w.find('.agent-msg-approvals').text()).toContain('files.write')
  })
})
