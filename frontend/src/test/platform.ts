import { vi } from 'vitest'
import type { Platform } from '../platform'

// A fake Platform for tests: every function is a spy, the clock and
// random source are fixed so timing logic is deterministic.
export type FakePlatform = Platform & {
  getToken: ReturnType<typeof vi.fn<() => string | undefined>>
  fetch: ReturnType<typeof vi.fn<typeof fetch>>
  notify: ReturnType<typeof vi.fn<Platform['notify']>>
  confirm: ReturnType<typeof vi.fn<Platform['confirm']>>
  now: ReturnType<typeof vi.fn<() => number>>
  random: ReturnType<typeof vi.fn<() => number>>
}

export const makePlatform = (overrides: Partial<Platform> = {}): FakePlatform => {
  const base = {
    getToken: vi.fn<() => string | undefined>(() => 'test-token'),
    backendUrl: '/agent-api/v1',
    fetch: vi.fn<typeof fetch>(async () => new Response('{}', { status: 200 })),
    notify: vi.fn<Platform['notify']>(),
    confirm: vi.fn<Platform['confirm']>(async () => true),
    now: vi.fn<() => number>(() => Date.UTC(2026, 0, 1, 12, 0, 0)),
    random: vi.fn<() => number>(() => 0.5)
  }
  return Object.assign(base, overrides) as FakePlatform
}
