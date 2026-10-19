import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import MessageBubble from './MessageBubble.vue'
import { answer, messageOf, userMessage } from '../../test/chat'
import { approvalOf } from '../../test/fixtures'
import type { MessageJob } from '../../api/types'
import type { JobApprovals } from '../../stores/chat'

const job = (state: string, error = ''): MessageJob => ({ id: 'j1', state, error })

const mountBubble = (props: Record<string, unknown>) => mount(MessageBubble, { props: props as never })

describe('MessageBubble', () => {
  it.each([
    ['queued', 'Waiting'],
    ['running', 'Working'],
    ['waiting_approval', 'Needs your approval'],
    ['succeeded', 'Answered'],
    ['failed', 'Could not answer'],
    ['cancelled', 'Cancelled']
  ])('labels a question whose job is %s as %s', (state, label) => {
    const w = mountBubble({ message: userMessage('m1', job(state)) })
    const status = w.find('.agent-msg-status')
    expect(status.text()).toContain(label)
    expect(status.find('[data-icon]').exists()).toBe(true) // never colour alone
    expect(w.find('.agent-msg').classes()).toContain('agent-msg-user')
  })

  it('shows an unknown job state raw, marked unknown', () => {
    const w = mountBubble({ message: userMessage('m1', job('weird\u202e')) })
    const status = w.find('.agent-msg-status')
    expect(status.text()).toContain('Unknown state')
    expect(status.text()).toContain('weird\\u{202E}')
  })

  it('shows the fixed error of a failed question', () => {
    const w = mountBubble({
      message: userMessage('m1', job('failed', 'The agent stopped because the usage budget is used up.'))
    })
    expect(w.find('.agent-msg-error').text()).toBe('The agent stopped because the usage budget is used up.')
  })

  it('links a question that needs an approval to the inbox filtered by its job', () => {
    const w = mountBubble({ message: userMessage('m1', job('waiting_approval')) })
    const link = w.find('.agent-msg-status a')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-approvals', query: { job: 'j1' } })
  })

  it('lists the pending approvals of the job inline, without decisions', () => {
    const approvals: JobApprovals = {
      items: [
        approvalOf('a1', { job_id: 'j1', tool: 'files.write', risk: 'write' }),
        approvalOf('a2', { job_id: 'j1', tool: 'files.delete', risk: 'destructive' })
      ],
      more: true,
      error: null
    }
    const w = mountBubble({ message: userMessage('m1', job('waiting_approval')), approvals })
    const rows = w.findAll('.agent-msg-approvals li')
    expect(rows).toHaveLength(2)
    expect(rows[0].text()).toContain('files.write')
    expect(rows[0].text()).toContain('Write')
    expect(rows[1].text()).toContain('Destructive')
    expect(rows[0].find('a').text()).toBe('Review')
    expect(w.text()).toContain('More actions are waiting')
    expect(w.findAll('button').filter((b) => /Approve|Reject/.test(b.text()))).toHaveLength(0)
  })

  it('shows why the agent does not answer yet while the question waits', () => {
    const w = mountBubble({ message: userMessage('m1', job('queued')), notice: 'paused' })
    expect(w.find('.agent-msg-notice').text()).toBe('The agent is paused; it will answer when resumed.')
    const done = mountBubble({ message: userMessage('m1', job('succeeded')), notice: 'paused' })
    expect(done.find('.agent-msg-notice').exists()).toBe(false)
  })

  it.each([
    ['killed', 'The agent is switched off by the operator; it will answer when it is switched on again.'],
    ['credentials', 'The agent has lost access to your files; reconnect it in Settings to get an answer.']
  ])('explains the %s notice', (notice, text) => {
    const w = mountBubble({ message: userMessage('m1', job('queued')), notice })
    expect(w.find('.agent-msg-notice').text()).toBe(text)
  })

  it('renders answers as plain pre-wrapped text, never as markup', () => {
    const hostile = '<img src=x onerror=alert(1)> **bold** https://evil.example\n\u202eevil'
    const w = mountBubble({ message: answer('m2', hostile) })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('a').exists()).toBe(false) // URLs are not linkified
    const text = w.find('.agent-msg-content')
    expect(text.text()).toContain('<img src=x onerror=alert(1)>')
    expect(text.text()).toContain('**bold**')
    expect(text.find('.agent-ctl').text()).toBe('\\u{202E}')
    expect(text.find('.agent-text').attributes('style')).toContain('pre-wrap')
    expect(w.find('.agent-msg').classes()).toContain('agent-msg-assistant')
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
    expect(w.find('.agent-msg-status').exists()).toBe(false) // no job yet
  })
})
