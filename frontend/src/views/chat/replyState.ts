import type { Message } from '../../api/types'

export type ReplyKind = 'waiting' | 'working' | 'approval' | 'error' | 'cancelled' | 'unknown'

export interface Reply {
  kind: ReplyKind
  text: string // untrusted reason, '' if none
  rawState: string
  jobId: string
  createdAt: string | null
}

// replyForQuestion is the state to show right after a question, or null when
// nothing is to be shown: no job, a reply of the job already in the thread, a
// partial answer shown, or a finished answer.
export const replyForQuestion = (q: Message, replied: ReadonlySet<string>, hasPartial: boolean): Reply | null => {
  const job = q.job
  if (!job || replied.has(job.id) || hasPartial) return null
  const reply = (kind: ReplyKind, text = ''): Reply => ({
    kind,
    text,
    rawState: job.state,
    jobId: job.id,
    createdAt: null
  })
  switch (job.state) {
    case 'succeeded':
      return null
    case 'failed':
      return reply('error', String(job.error ?? ''))
    case 'cancelled':
      return reply('cancelled')
    case 'waiting_approval':
      return reply('approval')
    case 'running':
      return reply('working')
    case 'queued':
      return reply('waiting')
  }
  return reply('unknown')
}

// replyForSystem is the reply shown in place of a system message of a job.
export const replyForSystem = (m: Message, stateByJob: ReadonlyMap<string, string>): Reply => ({
  kind: stateByJob.get(m.job_id) === 'cancelled' ? 'cancelled' : 'error',
  text: String(m.content ?? ''),
  rawState: stateByJob.get(m.job_id) ?? '',
  jobId: m.job_id,
  createdAt: m.created_at
})
