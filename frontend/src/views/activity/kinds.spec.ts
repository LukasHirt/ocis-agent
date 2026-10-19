import { describe, expect, it } from 'vitest'
import { credLabel, diffLines, kindLabel, KNOWN_KINDS, memoryEdit } from './kinds'

const t = (s: string) => s

describe('activity kinds', () => {
  it('labels every kind the sidecar writes', () => {
    const written = [
      'approval_requested',
      'approval_decided',
      'approval_expired',
      'tool_call',
      'action_executed',
      'policy_denied',
      'tampered',
      'memory_changed',
      'memory_edited',
      'job_finished',
      'job_cancelled',
      'chat_message',
      'conversation_deleted',
      'agent_paused',
      'agent_resumed',
      'agent_dormant',
      'policy_changed',
      'token_created',
      'token_renewed',
      'token_deleted',
      'token_label_closed',
      'token_revoked_all',
      'token_invalid',
      'token_expired',
      'token_anomaly',
      'unenrolled'
    ]
    expect([...KNOWN_KINDS].sort()).toEqual([...written].sort())
    for (const k of written) expect(kindLabel(k, t)).toMatch(/^[A-Z]/)
  })

  it('has no label for an unknown kind, also not for inherited names', () => {
    expect(kindLabel('brand_new', t)).toBeNull()
    expect(kindLabel('constructor', t)).toBeNull()
    expect(kindLabel('__proto__', t)).toBeNull()
  })

  it('names who acted', () => {
    expect(credLabel('oidc', t)).toBe('You')
    expect(credLabel('app', t)).toBe('The agent')
    expect(credLabel('', t)).toBeNull()
    expect(credLabel('other', t)).toBeNull()
  })

  it('classes diff lines by their first character only', () => {
    expect(diffLines('+a\n-b\n~ trailing newline changed\n c\n+\n')).toEqual([
      { kind: 'add', text: '+a' },
      { kind: 'del', text: '-b' },
      { kind: 'note', text: '~ trailing newline changed' },
      { kind: 'context', text: ' c' },
      { kind: 'add', text: '+' }
    ])
    expect(diffLines('x-+')).toEqual([{ kind: 'context', text: 'x-+' }])
  })

  it('splits a memory edit detail into name and diff', () => {
    expect(memoryEdit('name=notes/a.md; diff=+x\n-y\n')).toEqual({ name: 'notes/a.md', diff: '+x\n-y\n' })
    expect(memoryEdit('name=profile.md; diff=+a; diff=b')).toEqual({ name: 'profile.md', diff: '+a; diff=b' })
    expect(memoryEdit('something else')).toBeNull()
  })
})
