import { readdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { describe, expect, it } from 'vitest'

// The host's vue3-gettext instance does not expose these as globals, so a
// template using them throws at render. Use useGettext() instead.
const FORBIDDEN = [/\$gettextInterpolate/, /\$pgettext/, /\$npgettext/, /this\.\$gettext/]

function sources(dir: string): string[] {
  return readdirSync(dir, { withFileTypes: true }).flatMap((e) => {
    const p = join(dir, e.name)
    if (e.isDirectory()) return sources(p)
    return /\.(vue|ts)$/.test(e.name) && !/\.spec\.ts$/.test(e.name) ? [p] : []
  })
}

describe('no global gettext helpers', () => {
  const files = sources(__dirname)

  it.each(FORBIDDEN.map((r) => [r.source, r] as const))('has no %s', (_n, re) => {
    const hits = files.filter((f) => re.test(readFileSync(f, 'utf8')))
    expect(hits).toEqual([])
  })
})
