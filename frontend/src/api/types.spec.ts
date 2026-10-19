import { describe, expect, it } from 'vitest'
import type { Approval, ApprovalState, JobState, PauseReason, Risk, Status, TokenStatus } from './types'

// These checks run in `pnpm check:types` (vue-tsc): an enumeration field
// still accepts a value the client does not know, its known members stay
// visible to the type checker, and a label checked with `satisfies` against
// the enumeration is a type error when it is misspelt.
describe('api types: open enumerations', () => {
  it('keeps the known members and accepts unknown values', () => {
    const risk: Approval['risk'] = 'some_future_risk'
    const state: Approval['state'] = 'pending'
    const reason: Status['pause_reason'] = 'credential_expired'
    // the field type is not collapsed to string: its members are still a union
    type Known<T> = T extends string ? (string extends T ? never : T) : never
    const members: Known<Approval['risk']>[] = ['read', 'write', 'destructive', 'outward']
    expect([risk, state, reason, members.length]).toEqual(['some_future_risk', 'pending', 'credential_expired', 4])
  })

  it('refuses misspelt labels checked with satisfies', () => {
    const labels = [
      'destructive' satisfies Risk,
      'executing' satisfies ApprovalState,
      'waiting_approval' satisfies JobState,
      'token_anomaly' satisfies PauseReason,
      'revoked' satisfies TokenStatus,
      // @ts-expect-error a typo is not a Risk
      'destructve' satisfies Risk,
      // @ts-expect-error a typo is not an ApprovalState
      'executd' satisfies ApprovalState,
      // @ts-expect-error a typo is not a PauseReason
      'credential_revokd' satisfies PauseReason,
      // @ts-expect-error a typo is not a TokenStatus
      'revokd' satisfies TokenStatus
    ]
    expect(labels).toHaveLength(9)
  })
})
