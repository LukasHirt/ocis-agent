// prettyJson re-indents valid JSON text with two spaces and returns null
// for invalid JSON. Strings, escapes and numbers are copied exactly as
// stored, so big numbers keep every digit and key order stays as it is.
export const prettyJson = (raw: string): string | null => {
  try {
    JSON.parse(raw)
  } catch {
    return null
  }
  let out = ''
  let depth = 0
  const indent = () => '\n' + '  '.repeat(depth)
  for (let i = 0; i < raw.length; i++) {
    const c = raw[i]
    if (c === '"') {
      let j = i + 1
      while (raw[j] !== '"') j += raw[j] === '\\' ? 2 : 1
      out += raw.slice(i, j + 1)
      i = j
    } else if (c === '{' || c === '[') {
      // keep empty containers on one line
      let j = i + 1
      while (j < raw.length && ' \t\n\r'.includes(raw[j])) j++
      if (raw[j] === (c === '{' ? '}' : ']')) {
        out += c + raw[j]
        i = j
      } else {
        depth++
        out += c + indent()
      }
    } else if (c === '}' || c === ']') {
      depth--
      out += indent() + c
    } else if (c === ',') {
      out += ',' + indent()
    } else if (c === ':') {
      out += ': '
    } else if (!' \t\n\r'.includes(c)) {
      out += c
    }
  }
  return out
}

// stringifyValue renders a value as two-space indented JSON, or as plain
// text when it cannot be serialised (undefined, cycles, BigInt).
export const stringifyValue = (value: unknown): string => {
  try {
    const s = JSON.stringify(value, null, 2)
    if (typeof s === 'string') return s
  } catch {
    // fall through
  }
  return String(value)
}
