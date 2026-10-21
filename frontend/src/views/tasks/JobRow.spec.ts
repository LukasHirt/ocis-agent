import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import JobRow from './JobRow.vue'
import { jobOf } from '../../test/jobs'
import type { Job } from '../../api/types'

const row = (over: Partial<Job> = {}) => mount(JobRow, { props: { job: jobOf('j1', over) } })

describe('JobRow', () => {
  it.each([
    ['queued', 'Waiting', 'time'],
    ['running', 'Working', 'loader-4'],
    ['waiting_approval', 'Needs your approval', 'hourglass'],
    ['succeeded', 'Done', 'checkbox-circle'],
    ['failed', 'Failed', 'error-warning'],
    ['cancelled', 'Cancelled', 'close-circle']
  ])('labels the state %s with text and an icon', (state, label, icon) => {
    const w = row({ state })
    const s = w.find('.agent-job-state')
    expect(s.text()).toBe(label)
    expect(s.find('[data-icon]').attributes('data-icon')).toBe(icon)
  })

  it('shows an unknown state as text next to a generic label', () => {
    const w = row({ state: '<b>new</b>' })
    expect(w.find('.agent-job-state').text()).toBe('Unknown state: <b>new</b>')
    expect(w.find('.agent-job-state b').exists()).toBe(false)
  })

  it('links the goal to the job detail and keeps hostile text as text', () => {
    const goal = '<img src=x onerror=alert(1)>‮gnp.exe ' + 'x'.repeat(5000)
    const w = row({ goal })
    const link = w.find('.agent-job-link')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-tasks-job', params: { id: 'j1' } })
    expect(w.find('img').exists()).toBe(false)
    expect(w.find('.agent-job-goal .agent-ctl').text()).toBe('\\u{202E}')
    expect(w.find('.agent-job-goal').text()).toContain('x'.repeat(5000)) // cut by CSS only
  })

  it('names an empty goal', () => {
    expect(row({ goal: '' }).find('.agent-job-goal').text()).toBe('(no description)')
  })

  it('labels the trigger and shows an unknown one as text', () => {
    expect(row({ trigger_kind: 'chat' }).find('.agent-job-trigger').text()).toBe('Chat question')
    expect(row({ trigger_kind: 'file_event' }).find('.agent-job-trigger').text()).toBe('file_event')
  })

  it('shows when a job was created, and when it ended once it ended', () => {
    const live = row({ state: 'running' })
    expect(live.find('.agent-job-created time').attributes('datetime')).toBe('2026-01-01T10:00:00Z')
    expect(live.find('.agent-job-changed').text()).toContain('Updated')
    expect(live.find('.agent-job-changed time').attributes('datetime')).toBe('2026-01-01T10:05:00Z')
    const ended = row({ state: 'failed', updated_at: '2026-01-01T11:00:00Z', ended_at: '2026-01-01T10:30:00Z' })
    expect(ended.find('.agent-job-changed').text()).toContain('Ended')
    expect(ended.find('.agent-job-changed time').attributes('datetime')).toBe('2026-01-01T10:30:00Z')
  })

  it('shows the tokens a job used, and nothing for none', () => {
    expect(row({ tokens_in: 1200, tokens_out: 34 }).find('.agent-job-tokens').text()).toBe('1200 in, 34 out')
    expect(row().find('.agent-job-tokens').exists()).toBe(false)
  })

  it('links a chat job to its conversation', () => {
    const w = row({ trigger_kind: 'chat', trigger_ref: 'conv-1' })
    const link = w.find('.agent-job-conversation')
    expect(JSON.parse(link.attributes('data-to')!)).toEqual({ name: 'agent-chat-conversation', params: { id: 'conv-1' } })
    expect(link.text()).toBe('Open the conversation')
    expect(row({ trigger_kind: 'chat', trigger_ref: '' }).find('.agent-job-conversation').exists()).toBe(false)
    expect(row({ trigger_kind: 'file_event', trigger_ref: 'x' }).find('.agent-job-conversation').exists()).toBe(false)
  })

  it('counts the steps', () => {
    expect(row({ steps_count: 3 }).find('.agent-job-steps').text()).toBe('3 steps')
    expect(row({ steps_count: 1 }).find('.agent-job-steps').text()).toBe('1 step')
  })

  it('shows a known error reason and an unknown one as text', () => {
    const known = row({ state: 'failed', error: 'The task grew too large to continue.' })
    expect(known.find('.agent-job-error').text()).toBe('The task grew too large to continue.')
    const odd = row({ state: 'failed', error: 'odd <i>reason</i>' })
    expect(odd.find('.agent-job-error').text()).toBe('odd <i>reason</i>')
    expect(odd.find('.agent-job-error i').exists()).toBe(false)
    expect(row({ error: '' }).find('.agent-job-error').exists()).toBe(false)
  })
})
