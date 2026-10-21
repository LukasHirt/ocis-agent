import { describe, expect, it } from 'vitest'
import { cleanTitlePart, documentTitleSegments } from './documentTitle'

const base = { app: 'Agent', task: 'Task', enrolled: true }

describe('cleanTitlePart', () => {
  it('strips control and bidi characters entirely and collapses whitespace', () => {
    expect(cleanTitlePart('a\u202Eb\u0000c\u200Bd  \n e\t f')).toBe('abcd e f')
  })
  it('caps at 60 characters with an ellipsis', () => {
    const out = cleanTitlePart('x'.repeat(100))
    expect(out).toBe('x'.repeat(59) + '…')
    expect(cleanTitlePart('y'.repeat(60))).toBe('y'.repeat(60))
  })
  it('does not cut a surrogate pair', () => {
    const out = cleanTitlePart('\u{1F600}'.repeat(70))
    expect([...out].length).toBe(60)
    expect(out.endsWith('…')).toBe(true)
  })
  it('returns an empty string for blank input', () => {
    expect(cleanTitlePart(' \u202E ')).toBe('')
  })
})

describe('documentTitleSegments', () => {
  it('uses the section and the app', () => {
    expect(documentTitleSegments({ ...base, name: 'agent-chat', section: 'Chat' })).toEqual(['Chat', 'Agent'])
  })
  it('puts the conversation title first, and falls back to none', () => {
    const r = { ...base, name: 'agent-chat-conversation', section: 'Chat' }
    expect(documentTitleSegments({ ...r, conversationTitle: 'Weekday brief' })).toEqual(['Weekday brief', 'Chat', 'Agent'])
    expect(documentTitleSegments({ ...r, conversationTitle: ' \u202E ' })).toEqual(['Chat', 'Agent'])
    expect(documentTitleSegments(r)).toEqual(['Chat', 'Agent'])
  })
  it('labels a task with a fixed word, never its goal', () => {
    expect(documentTitleSegments({ ...base, name: 'agent-tasks-job', section: 'Tasks', params: { id: 'j1' } })).toEqual([
      'Task',
      'Tasks',
      'Agent'
    ])
  })
  it('uses the memory file name, joined and cleaned', () => {
    const r = { ...base, name: 'agent-memory-file', section: 'Memory' }
    expect(documentTitleSegments({ ...r, params: { name: ['notes', 'a\u202Eb.md'] } })).toEqual(['notes/ab.md', 'Memory', 'Agent'])
    expect(documentTitleSegments({ ...r, params: { name: 'profile.md' } })).toEqual(['profile.md', 'Memory', 'Agent'])
  })
  it('shows only the app on the home view, a missing section and the enrolment', () => {
    expect(documentTitleSegments({ ...base, name: 'agent-home', section: 'Agent' })).toEqual(['Agent'])
    expect(documentTitleSegments({ ...base, name: 'agent-chat', section: '' })).toEqual(['Agent'])
    expect(documentTitleSegments({ ...base, enrolled: false, name: 'agent-chat', section: 'Chat' })).toEqual(['Agent'])
  })
})
