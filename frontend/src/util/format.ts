const isCount = (n: unknown): n is number => typeof n === 'number' && Number.isFinite(n) && n >= 0

const UNITS: [string, number][] = [
  ['d', 86_400_000],
  ['h', 3_600_000],
  ['m', 60_000],
  ['s', 1_000]
]

// formatDuration renders milliseconds as at most the two largest non-zero
// units ("1h 5m", "45s"); invalid input gives an empty string.
export const formatDuration = (ms: number): string => {
  if (!isCount(ms)) return ''
  let rest = Math.floor(ms)
  const parts: string[] = []
  for (const [unit, size] of UNITS) {
    const n = Math.floor(rest / size)
    rest -= n * size
    if (n > 0 && parts.length < 2) parts.push(`${n}${unit}`)
    else if (parts.length > 0) break
  }
  return parts.length > 0 ? parts.join(' ') : '0s'
}

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB']

// formatBytes renders a byte count in binary units with at most one
// decimal; invalid input gives an empty string.
export const formatBytes = (n: number): string => {
  if (!isCount(n)) return ''
  let v = n
  let i = 0
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024
    i++
  }
  const shown = i === 0 ? String(Math.floor(v)) : String(Math.round(v * 10) / 10)
  return `${shown} ${BYTE_UNITS[i]}`
}

const dateTimeFormat = (locale?: string): Intl.DateTimeFormat => {
  const opts: Intl.DateTimeFormatOptions = { dateStyle: 'medium', timeStyle: 'short' }
  try {
    return new Intl.DateTimeFormat(locale, opts)
  } catch {
    return new Intl.DateTimeFormat(undefined, opts)
  }
}

// formatTime renders an ISO timestamp in the given (or the browser's)
// locale; an invalid date is returned as it is.
export const formatTime = (iso: string, locale?: string): string => {
  if (typeof iso !== 'string' || iso === '') return ''
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return dateTimeFormat(locale).format(d)
}

// shortId returns the first eight characters of an id for compact display.
export const shortId = (id: string): string => {
  if (typeof id !== 'string') return ''
  return Array.from(id).slice(0, 8).join('')
}
