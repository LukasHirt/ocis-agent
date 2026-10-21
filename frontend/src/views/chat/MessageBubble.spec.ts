import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import { createGettext } from 'vue3-gettext'
import MessageBubble from './MessageBubble.vue'
import { answer, messageOf, userMessage } from '../../test/chat'
import { approvalOf } from '../../test/fixtures'
import type { MessageJob } from '../../api/types'
import type { JobApprovals } from '../../stores/chat'

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

  it.each([
    ['queued', 'neutral'],
    ['running', 'info'],
    ['waiting_approval', 'warning'],
    ['succeeded', 'success'],
    ['failed', 'danger'],
    ['cancelled', 'neutral']
  ])('colours the chip of a %s job %s', (state, tone) => {
    const w = mountBubble({ message: userMessage('m1', job(state)) })
    const chip = w.find('.agent-msg-status')
    expect(chip.classes()).toContain('agent-chip')
    expect(chip.classes()).toContain(`agent-chip--${tone}`)
  })

  it('marks an unknown job state as a dashed danger chip', () => {
    const w = mountBubble({ message: userMessage('m1', job('weird')) })
    const chip = w.find('.agent-msg-status')
    expect(chip.classes()).toEqual(expect.arrayContaining(['agent-chip--danger', 'agent-chip--dashed']))
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

  it('translates a known job error and a known system message like the tasks screen', () => {
    const failed = mountGerman({ message: userMessage('m1', job('failed', BUDGET)) })
    expect(failed.find('.agent-msg-error').text()).toBe(BUDGET_DE)
    const notice = mountGerman({ message: messageOf('m2', { role: 'system', content: BUDGET }) })
    expect(notice.find('.agent-msg-content').text()).toBe(BUDGET_DE)
  })

  it('shows an unknown job error and system message as raw text', () => {
    const failed = mountGerman({ message: userMessage('m1', job('failed', 'odd\u202e error')) })
    expect(failed.find('.agent-msg-error').text()).toBe('odd\\u{202E} error')
    const notice = mountGerman({ message: messageOf('m2', { role: 'system', content: 'odd\u202e notice' }) })
    expect(notice.find('.agent-msg-content').text()).toBe('odd\\u{202E} notice')
  })

  it('never translates an answer or a question', () => {
    const w = mountGerman({ message: answer('m3', BUDGET) })
    expect(w.find('.agent-msg-content').text()).toBe(BUDGET)
    const q = mountGerman({ message: messageOf('m4', { content: BUDGET }) })
    expect(q.find('.agent-msg-content').text()).toBe(BUDGET)
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
    expect(w.find('.agent-msg-status').exists()).toBe(false) // no job yet
  })
})
