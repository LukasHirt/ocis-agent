import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

// Everything the sidecar, the files or the model produced is untrusted text.
// It is rendered as text only, so our sources never use raw HTML sinks.
const FORBIDDEN = [/v-html/, /innerHTML/, /outerHTML/, /insertAdjacentHTML/]

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name)
    if (e.isDirectory()) return sources(p)
    return /\.(vue|ts)$/.test(e.name) && !/\.spec\.ts$/.test(e.name) ? [p] : []
  })
}

describe('no raw HTML sinks', () => {
  const files = sources(__dirname)

  it('scans the sources', () => {
    expect(files.length).toBeGreaterThan(20)
    expect(files.some((f) => f.endsWith('.vue'))).toBe(true)
  })

  it.each(FORBIDDEN.map((r) => [r.source, r] as const))('has no %s', (_n, re) => {
    const hits = files.filter((f) => re.test(readFileSync(f, 'utf8')))
    expect(hits).toEqual([])
  })
})
