import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createGettext } from 'vue3-gettext'
import MessageBubble from './MessageBubble.vue'
import { answer, messageOf, userMessage } from '../../test/chat'
import type { MessageJob } from '../../api/types'

const job = (state: string, error = ''): MessageJob => ({ id: 'j1', state, error, partial: '', partial_state: '' })

const mountBubble = (props: Record<string, unknown>) => mount(MessageBubble, { props: props as never })

const BUDGET = 'The agent stopped because the usage budget is used up.'
const BUDGET_DE = 'Der Agent hat angehalten, weil das Budget aufgebraucht ist.'
const mountGerman = (props: Record<string, unknown>) =>
  mount(MessageBubble, {
    props: props as never,
    global: {
      plugins: [
        createGettext({
          availableLanguages: { de: 'Deutsch' },
          defaultLanguage: 'de',
          translations: { de: { [BUDGET]: BUDGET_DE } }
        })
      ]
    }
  })

describe('MessageBubble', () => {
  it('shows no state under a question, whatever its job does', () => {
    for (const state of ['queued', 'running', 'waiting_approval', 'succeeded', 'failed', 'cancelled', 'weird']) {
      const w = mountBubble({ message: userMessage('m1', job(state, 'boom')) })
      expect(w.find('.agent-chip').exists()).toBe(false)
      expect(w.text()).not.toContain('Answered')
      expect(w.find('.agent-msg').classes()).toContain('agent-msg-user')
      expect(w.find('.agent-msg-error').exists()).toBe(false)
    }
  })

  it('shows an avatar for assistant and system messages, none for the user', () => {
    expect(mountBubble({ message: answer('a1') }).find('.agent-msg-avatar').exists()).toBe(true)
    expect(mountBubble({ message: userMessage('m1', null) }).find('.agent-msg-avatar').exists()).toBe(false)
    const sys = mountBubble({ message: { ...answer('s1'), role: 'system' } })
    expect(sys.find('.agent-msg-avatar').exists()).toBe(true)
  })

  it('keeps the content inside the body', () => {
    const w = mountBubble({ message: answer('a1') })
    expect(w.find('.agent-msg-body .agent-msg-content').exists()).toBe(true)
  })

  it('translates a known system message like the tasks screen', () => {
    const notice = mountGerman({ message: messageOf('m2', { role: 'system', content: BUDGET }) })
    expect(notice.find('.agent-msg-content').text()).toBe(BUDGET_DE)
  })

  it('shows an unknown system message as raw text', () => {
    const notice = mountGerman({ message: messageOf('m2', { role: 'system', content: 'odd\u202e notice' }) })
    expect(notice.find('.agent-msg-content').text()).toBe('odd\\u{202E} notice')
  })

  it('never translates an answer or a question', () => {
    const w = mountGerman({ message: answer('m3', BUDGET) })
    expect(w.find('.agent-msg-content').text()).toBe(BUDGET)
    const q = mountGerman({ message: messageOf('m4', { content: BUDGET }) })
    expect(q.find('.agent-msg-content').text()).toBe(BUDGET)
  })

  it('renders answers as safe markdown', () => {
    const hostile = '<img src=x onerror=alert(1)> **bold** https://evil.example\n\u202eevil'
    const w = mountBubble({ message: answer('m2', hostile) })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('a').exists()).toBe(false) // URLs are not linkified
    const text = w.find('.agent-msg-content')
    expect(text.text()).toContain('<img src=x onerror=alert(1)>')
    expect(text.find('strong').text()).toBe('bold')
    expect(text.find('.agent-ctl').text()).toBe('\\u{202E}')
    expect(w.find('.agent-msg').classes()).toContain('agent-msg-assistant')
  })

  it('never renders a user question as markdown', () => {
    const w = mountBubble({ message: userMessage('m1', null, '**x**') })
    expect(w.find('.agent-msg-content strong').exists()).toBe(false)
    expect(w.find('.agent-msg-content').text()).toBe('**x**')
  })

  it('marks system messages visually and by text', () => {
    const w = mountBubble({ message: messageOf('m3', { role: 'system', content: 'The agent stopped.' }) })
    expect(w.find('.agent-msg').classes()).toContain('agent-msg-system')
    expect(w.find('.agent-msg-role').text()).toBe('Notice')
    expect(w.find('[data-icon]').exists()).toBe(true)
  })

  it('shows hostile user text as text', () => {
    const w = mountBubble({ message: userMessage('m1', null, '<script>x</script>\u0007') })
    expect(w.find('script').exists()).toBe(false)
    expect(w.find('.agent-msg-content').text()).toContain('<script>x</script>')
    expect(w.find('.agent-chip').exists()).toBe(false)
  })
})
