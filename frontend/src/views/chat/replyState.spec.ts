import { describe, expect, it } from 'vitest'
import { replyForQuestion, replyForSystem } from './replyState'
import { jobOfMessage, messageOf, userMessage } from '../../test/chat'

const q = (state: string, error = '') => userMessage('m1', jobOfMessage('j1', state, { error }))
const none = new Set<string>()

describe('replyForQuestion', () => {
  it('shows nothing for a question without a job', () => {
    expect(replyForQuestion(userMessage('m1', null), none, false)).toBeNull()
  })

  it('shows nothing once a reply of the job is in the thread', () => {
    expect(replyForQuestion(q('failed', 'x'), new Set(['j1']), false)).toBeNull()
  })

  it('shows nothing while a partial answer is shown', () => {
    expect(replyForQuestion(q('running'), none, true)).toBeNull()
  })

  it('shows nothing for a succeeded job', () => {
    expect(replyForQuestion(q('succeeded'), none, false)).toBeNull()
  })

  it.each([
    ['failed', 'error'],
    ['cancelled', 'cancelled'],
    ['waiting_approval', 'approval'],
    ['running', 'working'],
    ['queued', 'waiting']
  ])('maps %s to %s', (state, kind) => {
    const r = replyForQuestion(q(state), none, false)
    expect(r?.kind).toBe(kind)
    expect(r?.jobId).toBe('j1')
  })

  it('carries the job error of a failed question and none for a cancelled one', () => {
    expect(replyForQuestion(q('failed', 'boom'), none, false)?.text).toBe('boom')
    expect(replyForQuestion(q('cancelled', 'ignored'), none, false)?.text).toBe('')
  })

  it('keeps the raw state of an unknown one', () => {
    expect(replyForQuestion(q('weird'), none, false)).toMatchObject({ kind: 'unknown', rawState: 'weird' })
  })

  it('shows a failed question once: the system message replaces the job error', () => {
    const sys = messageOf('s1', { role: 'system', content: 'Reason', job_id: 'j1' })
    expect(replyForQuestion(q('failed', 'Reason'), new Set(['j1']), false)).toBeNull()
    expect(replyForSystem(sys, new Map([['j1', 'failed']]))).toMatchObject({ kind: 'error', text: 'Reason', jobId: 'j1' })
  })
})

describe('replyForSystem', () => {
  const sys = messageOf('s1', { role: 'system', content: 'Stopped', job_id: 'j1', created_at: '2026-01-01T11:00:00Z' })

  it('is cancelled when the job was cancelled', () => {
    expect(replyForSystem(sys, new Map([['j1', 'cancelled']]))).toMatchObject({
      kind: 'cancelled',
      text: 'Stopped',
      createdAt: '2026-01-01T11:00:00Z'
    })
  })

  it('is an error otherwise, also for an unknown job', () => {
    expect(replyForSystem(sys, new Map()).kind).toBe('error')
    expect(replyForSystem(sys, new Map([['j1', 'failed']])).kind).toBe('error')
  })
})
