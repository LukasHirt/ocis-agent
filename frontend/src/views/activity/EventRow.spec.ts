import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import EventRow from './EventRow.vue'
import { eventOf } from '../../test/audit'
import type { AuditEvent } from '../../api/types'

const row = (over: Partial<AuditEvent> = {}) => mount(EventRow, { props: { event: eventOf(7, over) } })

const HOSTILE = '<img src=x onerror=alert(1)>‮gnp.exe'

describe('EventRow', () => {
  it('shows the kind label, time, actor, tool, risk and outcome', () => {
    const w = row({ kind: 'approval_decided', cred: 'oidc', tool: 'files.write', risk: 'write', outcome: 'approved' })
    expect(w.find('.agent-event-kind').text()).toBe('Approval decided')
    expect(w.find('time').attributes('datetime')).toBe('2026-01-01T10:00:00.000Z')
    expect(w.find('.agent-event-actor').text()).toBe('You')
    expect(w.find('.agent-event-tool').text()).toBe('files.write')
    expect(w.find('.agent-risk-label').text()).toBe('Write')
    expect(w.find('.agent-event-outcome').text()).toContain('approved')
  })

  it('leads with a kind icon, red for a refused action', () => {
    const ok = row({ kind: 'job_finished' })
    expect(ok.find('.agent-event-icon [data-stub=OcIcon]').attributes('data-icon')).toBe('task')
    expect(ok.find('.agent-event-icon').classes()).not.toContain('agent-event-icon--danger')
    const bad = row({ kind: 'policy_denied' })
    expect(bad.find('.agent-event-icon').classes()).toContain('agent-event-icon--danger')
    expect(bad.classes()).toContain('agent-row')
  })

  it('marks an unknown kind and shows it as text', () => {
    const w = row({ kind: HOSTILE })
    const k = w.find('.agent-event-kind')
    expect(k.text()).toContain('Unknown event:')
    expect(k.text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.find('img').exists()).toBe(false)
    expect(k.find('.agent-ctl').text()).toBe('\\u{202E}')
  })

  it('shows an unknown actor as text', () => {
    const w = row({ cred: '<b>x</b>' })
    expect(w.find('.agent-event-actor').text()).toBe('<b>x</b>')
    expect(w.find('.agent-event-actor b').exists()).toBe(false)
  })

  it('names no actor for an event without credentials', () => {
    const w = row({ kind: 'memory_changed', cred: '' })
    expect(w.find('.agent-event-by').exists()).toBe(false)
    expect(w.text()).not.toContain('By')
  })

  it('leaves out empty tool, risk and job', () => {
    const w = row({ tool: '', risk: '', job_id: '' })
    expect(w.find('.agent-event-tool').exists()).toBe(false)
    expect(w.find('.agent-risk').exists()).toBe(false)
    expect(w.find('.agent-event-job').exists()).toBe(false)
  })

  it('links the task of the event', () => {
    const w = row({ job_id: 'j1' })
    expect(JSON.parse(w.find('.agent-event-job a').attributes('data-to')!)).toEqual({
      name: 'agent-tasks-job',
      params: { id: 'j1' }
    })
  })

  it('shows the detail collapsed, as text only', () => {
    const w = row({ detail: HOSTILE })
    const d = w.find('details.agent-event-detail')
    expect(d.exists()).toBe(true)
    expect(d.attributes('open')).toBeUndefined()
    expect(d.find('summary').text()).toBe('Details')
    expect(d.find('pre').text()).toContain('<img src=x onerror=alert(1)>')
    expect(w.find('img').exists()).toBe(false)
    expect(d.find('.agent-ctl').exists()).toBe(true)
  })

  it('has no detail section without a detail', () => {
    expect(row({ detail: '' }).find('details').exists()).toBe(false)
  })

  it('shows the diff of an agent memory change line by line, hostile text as text', () => {
    const w = row({ kind: 'memory_changed', detail: '+added <b>bold</b>\n-removed‮\n~ trailing newline changed\n' })
    const lines = w.findAll('.agent-diff-line')
    expect(lines.map((l) => l.classes().find((c) => c.startsWith('agent-diff-') && c !== 'agent-diff-line'))).toEqual([
      'agent-diff-add',
      'agent-diff-del',
      'agent-diff-note'
    ])
    expect(lines[0].text()).toBe('+added <b>bold</b>')
    expect(w.find('.agent-diff b').exists()).toBe(false)
    expect(lines[1].find('.agent-ctl').exists()).toBe(true)
    expect(w.find('details summary').text()).toBe('Changes')
  })

  it('shows the file and the diff of a memory edit', () => {
    const w = row({ kind: 'memory_edited', cred: 'oidc', detail: 'name=notes/<i>x</i>.md; diff=+new\n-old\n' })
    expect(w.find('.agent-event-file').text()).toBe('notes/<i>x</i>.md')
    expect(w.find('.agent-event-file i').exists()).toBe(false)
    expect(w.findAll('.agent-diff-line').map((l) => l.text())).toEqual(['+new', '-old'])
  })

  it('shows a memory edit detail of another shape as plain text', () => {
    const w = row({ kind: 'memory_edited', detail: 'odd' })
    expect(w.find('.agent-diff').exists()).toBe(false)
    expect(w.find('details pre').text()).toBe('odd')
  })
})
