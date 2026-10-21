// A small, safe Markdown subset for agent answers. The parser is a line and
// character scanner (no regular expressions with nested quantifiers) and every
// scan stops at the next opener of its kind or at a fixed length, so each
// character is visited a bounded number of times. It produces a plain AST;
// nothing here knows about HTML. Links and images are only data: the renderer
// shows their targets as text.

export type MdInline =
  | { t: 'text'; v: string }
  | { t: 'code'; v: string }
  | { t: 'strong' | 'em' | 'del'; c: MdInline[] }
  | { t: 'link'; label: MdInline[]; url: string; title?: string }
  | { t: 'image'; alt: string; url: string; title?: string }
  | { t: 'br' }
export type MdAlign = 'left' | 'center' | 'right' | null
export type MdBlock =
  | { t: 'p'; c: MdInline[] }
  | { t: 'h'; level: 3 | 4 | 5; c: MdInline[] }
  | { t: 'ul'; items: MdBlock[][] }
  | { t: 'ol'; start: number; items: MdBlock[][] }
  | { t: 'quote'; c: MdBlock[] }
  | { t: 'pre'; v: string }
  | { t: 'hr' }
  | { t: 'table'; align: MdAlign[]; head: MdInline[][]; rows: MdInline[][][] }

export const MAX_MD_CHARS = 65536
export const MAX_MD_NODES = 20000
const MAX_BLOCK_DEPTH = 4
const MAX_INLINE_DEPTH = 8
const MAX_DELIMS = 64
const MAX_COLS = 30
const MAX_ROWS = 500
const MAX_LABEL = 1000
const MAX_URL = 2048
const MAX_SPACES = 64

const OVER = Symbol('markdown too large')
type Ctx = { nodes: number }
const count = (ctx: Ctx) => {
  if (++ctx.nodes > MAX_MD_NODES) throw OVER
}

// ---------------------------------------------------------------- characters

const isWs = (c: string): boolean => {
  const n = c.charCodeAt(0)
  return (
    n === 32 ||
    (n >= 9 && n <= 13) ||
    n === 0xa0 ||
    n === 0x1680 ||
    (n >= 0x2000 && n <= 0x200a) ||
    n === 0x2028 ||
    n === 0x2029 ||
    n === 0x202f ||
    n === 0x205f ||
    n === 0x3000
  )
}
const ALNUM = /[\p{L}\p{N}]/u
const isAlnum = (c: string): boolean => ALNUM.test(c)
const isPunct = (c: string): boolean => {
  const n = c.charCodeAt(0)
  return (n >= 33 && n <= 47) || (n >= 58 && n <= 64) || (n >= 91 && n <= 96) || (n >= 123 && n <= 126)
}
const isDigit = (c: string | undefined): boolean => c !== undefined && c >= '0' && c <= '9'

// trimAscii strips only spaces and tabs from both ends; String.trim() would
// also remove U+FEFF, U+2028 and U+2029, which must stay for the escaping.
const trimAscii = (s: string): string => {
  let a = 0
  let b = s.length
  while (a < b && (s[a] === ' ' || s[a] === '\t')) a++
  while (b > a && (s[b - 1] === ' ' || s[b - 1] === '\t')) b--
  return s.slice(a, b)
}

// ----------------------------------------------------------------- inlines

type Run = { t: 'run'; ch: string; rem: number }
type Item = MdInline | Run

type LinkScan =
  | { k: 'ok'; label: string; url: string; title?: string; end: number }
  | { k: 'noclose' } // no closing bracket before the end of the text
  | { k: 'end'; label: string } // `[label](ur` cut off by the end of the text
  | { k: 'fail' }

// scanLink looks at `[label](url "title")` starting at the `[` at index open.
const scanLink = (text: string, open: number): LinkScan => {
  const n = text.length
  let q = open + 1
  for (; q < n; q++) {
    const c = text[q]
    if (c === ']') break
    if (c === '[' || c === '\n') return { k: 'fail' }
    if (q - open > MAX_LABEL) return { k: 'fail' }
  }
  if (q >= n) return { k: 'noclose' }
  const label = text.slice(open + 1, q)
  if (q + 1 >= n) return { k: 'fail' }
  if (text[q + 1] !== '(') return { k: 'fail' }
  const p = q + 2
  let u = p
  let parens = 0
  while (u < n) {
    const c = text[u]
    if (isWs(c)) break
    if (c === '(') {
      if (parens >= 1) return { k: 'fail' }
      parens++
    } else if (c === ')') {
      if (parens === 0) break
      parens--
    }
    u++
    if (u - p > MAX_URL) return { k: 'fail' }
  }
  if (u >= n) return { k: 'end', label }
  const url = text.slice(p, u)
  if (url === '') return { k: 'fail' }
  if (text[u] === ')') return { k: 'ok', label, url, end: u + 1 }
  // an optional "title", kept
  let w = u
  while (w < n && text[w] === ' ' && w - u < MAX_SPACES) w++
  if (w >= n) return { k: 'end', label }
  if (text[w] !== '"') return { k: 'fail' }
  let t = w + 1
  while (t < n && text[t] !== '"' && text[t] !== '\n' && t - w <= MAX_URL) t++
  if (t >= n) return { k: 'end', label }
  if (text[t] !== '"') return { k: 'fail' }
  let x = t + 1
  while (x < n && text[x] === ' ' && x - t < MAX_SPACES) x++
  if (x >= n) return { k: 'end', label }
  if (text[x] !== ')') return { k: 'fail' }
  return { k: 'ok', label, url, title: text.slice(w + 1, t), end: x + 1 }
}

const pushText = (out: MdInline[], v: string, ctx: Ctx) => {
  if (v === '') return
  const last = out[out.length - 1]
  if (last && last.t === 'text') {
    last.v += v
    return
  }
  count(ctx)
  out.push({ t: 'text', v })
}

// finalize merges adjacent text, counts nodes and flattens emphasis nested
// deeper than MAX_INLINE_DEPTH into its parent. It is iterative: emphasis can
// be nested thousands deep before flattening.
const finalize = (src: Item[], ctx: Ctx, baseDepth: number): MdInline[] => {
  type Frame = {
    src: Item[]
    i: number
    out: MdInline[]
    kind: 'strong' | 'em' | 'del' | null
    depth: number
    flat: boolean
  }
  const root: MdInline[] = []
  const st: Frame[] = [{ src, i: 0, out: root, kind: null, depth: baseDepth, flat: false }]
  while (st.length > 0) {
    const f = st[st.length - 1]
    if (f.i >= f.src.length) {
      st.pop()
      if (f.kind && !f.flat && f.out.length > 0) {
        count(ctx)
        st[st.length - 1].out.push({ t: f.kind, c: f.out })
      }
      continue
    }
    const it = f.src[f.i++]
    switch (it.t) {
      case 'text':
        pushText(f.out, it.v, ctx)
        break
      case 'run':
        pushText(f.out, it.ch.repeat(it.rem), ctx)
        break
      case 'strong':
      case 'em':
      case 'del': {
        const depth = f.depth + 1
        const flat = depth > MAX_INLINE_DEPTH
        st.push({ src: it.c, i: 0, out: flat ? f.out : [], kind: it.t, depth, flat })
        break
      }
      case 'code':
      case 'link':
      case 'image':
      case 'br':
        count(ctx)
        f.out.push(it)
        break
    }
  }
  return root
}

const parseInline = (text: string, ctx: Ctx, closeOpen: boolean, baseDepth: number): MdInline[] => {
  const n = text.length
  const items: Item[] = []
  const stack: number[] = [] // indexes in items of open delimiter runs
  let ts = 0 // start of the plain text not yet pushed
  let i = 0

  // backtick runs of the block, by length, with a forward-only cursor each
  let table: Map<number, number[]> | null = null
  const cursor = new Map<number, number>()
  const buildTable = () => {
    table = new Map()
    let p = 0
    while (p < n) {
      if (text[p] !== '`') {
        p++
        continue
      }
      let e = p
      while (e < n && text[e] === '`') e++
      const list = table.get(e - p)
      if (list) list.push(p)
      else table.set(e - p, [p])
      p = e
    }
  }
  const findCloser = (pos: number, len: number): number => {
    if (!table) buildTable()
    const list = table!.get(len)
    if (!list) return -1
    let c = cursor.get(len) ?? 0
    while (c < list.length && list[c] <= pos) c++
    cursor.set(len, c)
    return c < list.length ? list[c] : -1
  }

  const flush = (to: number) => {
    if (to > ts) items.push({ t: 'text', v: text.slice(ts, to) })
  }
  const literal = (idx: number) => {
    const r = items[idx] as Run
    items[idx] = { t: 'text', v: r.ch.repeat(r.rem) }
  }
  const wrap = (a: number, use: number, ch: string) => {
    const content = items.splice(a + 1) as MdInline[]
    const kind = use === 2 ? (ch === '~' ? 'del' : 'strong') : 'em'
    items.push({ t: kind, c: content })
  }
  const useOf = (a: number, b: number) => (a === 3 && b === 3 ? 1 : a >= 2 && b >= 2 ? 2 : 1)

  while (i < n) {
    const ch = text[i]
    switch (ch) {
      case '\\': {
        const next = text[i + 1]
        if (next === '\n') {
          flush(i)
          items.push({ t: 'br' })
          i += 2
          ts = i
        } else if (next !== undefined && isPunct(next)) {
          flush(i)
          items.push({ t: 'text', v: next })
          i += 2
          ts = i
        } else {
          i++
        }
        break
      }
      case '\n':
        flush(i)
        items.push({ t: 'br' })
        i++
        ts = i
        break
      case '`': {
        let e = i
        while (e < n && text[e] === '`') e++
        const len = e - i
        flush(i)
        const close = findCloser(i, len)
        if (close >= 0) {
          items.push({ t: 'code', v: codeContent(text.slice(e, close)) })
          i = close + len
        } else if (closeOpen) {
          items.push({ t: 'code', v: codeContent(text.slice(e)) })
          i = n
        } else {
          items.push({ t: 'text', v: text.slice(i, e) })
          i = e
        }
        ts = i
        break
      }
      case '*':
      case '_':
      case '~': {
        let e = i
        while (e < n && text[e] === ch) e++
        const len = e - i
        const prev = i > 0 ? text[i - 1] : ' '
        const next = e < n ? text[e] : ' '
        let canOpen = !isWs(next)
        let canClose = !isWs(prev)
        if (ch === '_' && isAlnum(prev) && isAlnum(next)) canOpen = canClose = false
        if (ch === '~' && len !== 2) canOpen = canClose = false
        flush(i)
        let rem = len
        if (canClose) {
          while (rem > 0) {
            let k = stack.length - 1
            while (k >= 0 && (items[stack[k]] as Run).ch !== ch) k--
            if (k < 0) break
            for (let q = stack.length - 1; q > k; q--) literal(stack[q])
            stack.length = k + 1
            const a = stack[k]
            const op = items[a] as Run
            const use = useOf(op.rem, rem)
            wrap(a, use, ch)
            op.rem -= use
            rem -= use
            if (op.rem === 0) {
              items[a] = { t: 'text', v: '' }
              stack.length = k
            }
          }
        }
        if (rem > 0) {
          if (canOpen && stack.length < MAX_DELIMS) {
            items.push({ t: 'run', ch, rem })
            stack.push(items.length - 1)
          } else {
            items.push({ t: 'text', v: ch.repeat(rem) })
          }
        }
        i = e
        ts = i
        break
      }
      case '[': {
        const sc = scanLink(text, i)
        if (sc.k === 'ok') {
          flush(i)
          items.push({
            t: 'link',
            label: sc.label === '' ? [] : parseInline(sc.label, ctx, false, 1),
            url: sc.url,
            ...(sc.title === undefined ? {} : { title: sc.title })
          })
          i = sc.end
          ts = i
        } else if (closeOpen && sc.k === 'noclose') {
          flush(i)
          i++
          ts = i
        } else if (closeOpen && sc.k === 'end') {
          flush(i)
          for (const x of parseInline(sc.label, ctx, false, 1)) items.push(x)
          i = n
          ts = i
        } else {
          i++
        }
        break
      }
      case '!': {
        if (text[i + 1] !== '[') {
          i++
          break
        }
        const sc = scanLink(text, i + 1)
        if (sc.k === 'ok') {
          flush(i)
          items.push({
            t: 'image',
            alt: sc.label,
            url: sc.url,
            ...(sc.title === undefined ? {} : { title: sc.title })
          })
          i = sc.end
          ts = i
        } else if (closeOpen && sc.k === 'noclose') {
          flush(i)
          i += 2
          ts = i
        } else if (closeOpen && sc.k === 'end') {
          flush(i)
          items.push({ t: 'text', v: sc.label })
          i = n
          ts = i
        } else {
          i++
        }
        break
      }
      default:
        i++
    }
  }
  flush(n)

  if (closeOpen) {
    while (stack.length > 0) {
      const a = stack.pop()!
      const op = items[a] as Run
      while (op.rem > 0) {
        const use = op.rem === 3 ? 1 : op.rem >= 2 ? 2 : 1
        wrap(a, use, op.ch)
        op.rem -= use
      }
      items[a] = { t: 'text', v: '' }
    }
  } else {
    for (const a of stack) literal(a)
  }
  return finalize(items, ctx, baseDepth)
}

const codeContent = (raw: string): string => {
  let s = raw.indexOf('\n') >= 0 ? raw.split('\n').join(' ') : raw
  if (s.length >= 2 && s[0] === ' ' && s[s.length - 1] === ' ' && trimAscii(s) !== '') s = s.slice(1, -1)
  return s
}

// ------------------------------------------------------------------ blocks

const indentOf = (l: string): number => {
  let n = 0
  while (n < l.length && l.charCodeAt(n) === 32) n++
  return n
}
const isBlank = (l: string): boolean => indentOf(l) === l.length

const prepLines = (src: string): string[] => {
  const lines = src.split('\n')
  for (let k = 0; k < lines.length; k++) {
    let l = lines[k]
    if (l.endsWith('\r')) l = l.slice(0, -1)
    // expand tabs in the leading whitespace
    let w = 0
    while (w < l.length && (l[w] === ' ' || l[w] === '\t')) w++
    if (w > 0 && l.slice(0, w).includes('\t')) {
      let col = 0
      for (let q = 0; q < w; q++) col += l[q] === '\t' ? 4 - (col % 4) : 1
      l = ' '.repeat(col) + l.slice(w)
    }
    lines[k] = l
  }
  return lines
}

// a line holding only whitespace and characters that start a block
const onlyMarkers = (l: string): boolean => {
  let i = 0
  while (i < l.length) {
    const c = l[i]
    if (' \t#-*+>`~|_='.includes(c)) {
      i++
    } else if (isDigit(c)) {
      let j = i
      while (isDigit(l[j])) j++
      if (l[j] === '.' || l[j] === ')') i = j + 1
      else return false
    } else {
      return false
    }
  }
  return true
}

const fenceOpen = (l: string, ind: number): { ch: string; n: number } | null => {
  const ch = l[ind]
  if (ch !== '`' && ch !== '~') return null
  let e = ind
  while (e < l.length && l[e] === ch) e++
  if (e - ind < 3) return null
  if (ch === '`' && l.indexOf('`', e) >= 0) return null
  return { ch, n: e - ind }
}
const fenceCloses = (l: string, f: { ch: string; n: number }): boolean => {
  const ind = indentOf(l)
  if (ind > 3) return false
  let e = ind
  while (e < l.length && l[e] === f.ch) e++
  return e - ind >= f.n && isBlank(l.slice(e))
}

const isHr = (l: string, ind: number): boolean => {
  const ch = l[ind]
  if (ch !== '-' && ch !== '*' && ch !== '_') return false
  let n = 0
  for (let k = ind; k < l.length; k++) {
    if (l[k] === ch) n++
    else if (l[k] !== ' ') return false
  }
  return n >= 3
}

const headingOf = (l: string, ind: number): { level: 3 | 4 | 5; text: string } | null => {
  let e = ind
  while (e < l.length && l[e] === '#') e++
  const k = e - ind
  if (k < 1 || k > 6 || l[e] !== ' ') return null
  const t = trimAscii(l.slice(e + 1))
  let end = t.length
  while (end > 0 && t[end - 1] === '#') end--
  let text = t
  if (end === 0) text = ''
  else if (t[end - 1] === ' ') text = t.slice(0, end).trimEnd()
  if (text === '') return null
  return { level: k === 1 ? 3 : k === 2 ? 4 : 5, text }
}

type ListMarker = { ordered: boolean; delim: string; num: number; col: number; first: string; empty: boolean }
const listMarker = (l: string, ind: number): ListMarker | null => {
  const c = l[ind]
  let p: number
  let ordered = false
  let num = 0
  let delim = c
  if (c === '-' || c === '*' || c === '+') {
    p = ind + 1
  } else if (isDigit(c)) {
    let q = ind
    while (isDigit(l[q])) q++
    if (q - ind > 9) return null
    const d = l[q]
    if (d !== '.' && d !== ')') return null
    ordered = true
    num = Number(l.slice(ind, q))
    delim = d
    p = q + 1
  } else {
    return null
  }
  if (l[p] !== ' ') return null
  let s = p
  while (s < l.length && l[s] === ' ') s++
  const empty = s >= l.length
  const col = empty || s - p > 4 ? p + 1 : s
  return { ordered, delim, num, col, first: l.slice(col), empty }
}

type Delim = ('left' | 'center' | 'right' | null)[]

// splitRow splits a table row on unescaped pipes (`\|` becomes `|`), keeping
// at most max cells.
const splitRow = (line: string, max: number): string[] => {
  let s = trimAscii(line)
  if (s[0] === '|') s = s.slice(1)
  const cells: string[] = []
  let cur = ''
  let seg = 0
  let i = 0
  while (i < s.length) {
    const c = s[i]
    if (c === '\\' && s[i + 1] === '|') {
      cur += s.slice(seg, i) + '|'
      i += 2
      seg = i
    } else if (c === '|') {
      cells.push(trimAscii(cur + s.slice(seg, i)))
      cur = ''
      i++
      seg = i
      if (cells.length >= max) return cells
    } else {
      i++
    }
  }
  if (seg < s.length) cells.push(trimAscii(cur + s.slice(seg)))
  return cells
}

const delimCell = (c: string): 'left' | 'center' | 'right' | null | undefined => {
  let a = 0
  let b = c.length
  const l = c[0] === ':'
  if (l) a++
  const r = b > a && c[b - 1] === ':'
  if (r) b--
  if (b <= a) return undefined
  for (let k = a; k < b; k++) if (c[k] !== '-') return undefined
  return l && r ? 'center' : l ? 'left' : r ? 'right' : null
}

// tableStart: the header at lines[j] followed by a delimiter row.
const tableStart = (lines: string[], j: number): { head: string[]; align: Delim } | null => {
  if (j + 1 >= lines.length) return null
  const h = lines[j]
  const d = lines[j + 1]
  if (!h.includes('|') || !d.includes('|') || indentOf(d) > 3) return null
  const head = splitRow(h, MAX_COLS + 1)
  if (head.length === 0 || head.length > MAX_COLS) return null
  const dc = splitRow(d, MAX_COLS + 1)
  if (dc.length !== head.length) return null
  const align: Delim = []
  for (const c of dc) {
    const a = delimCell(c)
    if (a === undefined) return null
    align.push(a)
  }
  return { head, align }
}

const interrupts = (lines: string[], j: number, depth: number): boolean => {
  const l = lines[j]
  const ind = indentOf(l)
  if (ind > 3) return false
  if (fenceOpen(l, ind) || isHr(l, ind) || headingOf(l, ind)) return true
  if (depth < MAX_BLOCK_DEPTH) {
    if (l[ind] === '>') return true
    const m = listMarker(l, ind)
    if (m && !m.empty && (!m.ordered || m.num === 1)) return true
  }
  return l.includes('|') && tableStart(lines, j) !== null
}

const cleanLine = (l: string): string => {
  const t = trimAscii(l)
  return t.endsWith('\\') ? t.slice(0, -1) : t
}

const parseBlocks = (lines: string[], depth: number, last: boolean, ctx: Ctx): MdBlock[] => {
  const out: MdBlock[] = []
  let lastNB = lines.length - 1
  while (lastNB >= 0 && isBlank(lines[lastNB])) lastNB--

  const paragraph = (from: number, to: number): MdBlock => {
    const text = lines
      .slice(from, to)
      .map(cleanLine)
      .join('\n')
    count(ctx)
    return { t: 'p', c: parseInline(text, ctx, last && to > lastNB, 0) }
  }

  let i = 0
  while (i < lines.length) {
    const line = lines[i]
    if (isBlank(line)) {
      i++
      continue
    }
    const ind = indentOf(line)
    if (ind <= 3) {
      const f = fenceOpen(line, ind)
      if (f) {
        const content: string[] = []
        let j = i + 1
        while (j < lines.length) {
          if (fenceCloses(lines[j], f)) {
            j++
            break
          }
          content.push(lines[j])
          j++
        }
        count(ctx)
        out.push({ t: 'pre', v: content.join('\n') })
        i = j
        continue
      }
      if (isHr(line, ind)) {
        count(ctx)
        out.push({ t: 'hr' })
        i++
        continue
      }
      const hd = headingOf(line, ind)
      if (hd) {
        count(ctx)
        out.push({ t: 'h', level: hd.level, c: parseInline(hd.text, ctx, last && i + 1 > lastNB, 0) })
        i++
        continue
      }
      if (depth < MAX_BLOCK_DEPTH && line[ind] === '>') {
        const inner: string[] = []
        let j = i
        while (j < lines.length) {
          const l = lines[j]
          const k = indentOf(l)
          if (k > 3 || l[k] !== '>') break
          let s = l.slice(k + 1)
          if (s[0] === ' ') s = s.slice(1)
          inner.push(s)
          j++
        }
        count(ctx)
        out.push({ t: 'quote', c: parseBlocks(inner, depth + 1, last && j > lastNB, ctx) })
        i = j
        continue
      }
      const lm = depth < MAX_BLOCK_DEPTH ? listMarker(line, ind) : null
      if (lm) {
        const items: MdBlock[][] = []
        let cur: ListMarker = lm
        let j = i
        let end = i + 1
        for (;;) {
          const itemLines = [cur.first]
          let lastContent = j
          let k = j + 1
          while (k < lines.length) {
            const l = lines[k]
            if (isBlank(l)) {
              k++
              continue
            }
            if (indentOf(l) < cur.col) break
            for (let b = lastContent + 1; b < k; b++) itemLines.push('')
            itemLines.push(l.slice(cur.col))
            lastContent = k
            k++
          }
          // k is the next non-blank line that is not part of the item
          let next: ListMarker | null = null
          if (k < lines.length && indentOf(lines[k]) <= 3) {
            const m = listMarker(lines[k], indentOf(lines[k]))
            if (m && m.ordered === cur.ordered && m.delim === cur.delim && !isHr(lines[k], indentOf(lines[k]))) next = m
          }
          count(ctx)
          items.push(parseBlocks(itemLines, depth + 1, last && !next && lastContent >= lastNB, ctx))
          end = lastContent + 1
          if (!next) break
          cur = next
          j = k
        }
        count(ctx)
        out.push(lm.ordered ? { t: 'ol', start: lm.num, items } : { t: 'ul', items })
        i = end
        continue
      }
      const ts = line.includes('|') ? tableStart(lines, i) : null
      if (ts) {
        let j = i + 2
        const bodyStart = j
        while (j < lines.length && !isBlank(lines[j]) && lines[j].includes('|')) j++
        if (j - bodyStart > MAX_ROWS) {
          out.push(paragraph(i, j))
          i = j
          continue
        }
        const cols = ts.head.length
        const lastLine = last && j > lastNB
        count(ctx)
        const headCells = ts.head.map((c) => (count(ctx), parseInline(c, ctx, lastLine && j === bodyStart, 0)))
        const rows: MdInline[][][] = []
        for (let r = bodyStart; r < j; r++) {
          const raw = splitRow(lines[r], Infinity)
          const isLastRow = lastLine && r === j - 1
          const cells: MdInline[][] = []
          for (let ci = 0; ci < Math.max(cols, raw.length); ci++) {
            count(ctx)
            cells.push(parseInline(raw[ci] ?? '', ctx, isLastRow, 0))
          }
          count(ctx)
          rows.push(cells)
        }
        out.push({ t: 'table', align: ts.align, head: headCells, rows })
        i = j
        continue
      }
    }
    // paragraph: runs to a blank line or the next block start
    let j = i + 1
    while (j < lines.length && !isBlank(lines[j]) && !interrupts(lines, j, depth)) j++
    out.push(paragraph(i, j))
    i = j
  }
  return out
}

export const parseMarkdown = (src: string, opts?: { partial?: boolean }): MdBlock[] | null => {
  if (src.length > MAX_MD_CHARS) return null
  const partial = opts?.partial === true
  const ctx: Ctx = { nodes: 0 }
  try {
    let lines = prepLines(src)
    if (partial && lines.length > 0) {
      const l = lines[lines.length - 1]
      if (!isBlank(l) && onlyMarkers(l)) lines = lines.slice(0, -1)
    }
    return parseBlocks(lines, 0, partial, ctx)
  } catch (e) {
    if (e === OVER) return null
    throw e
  }
}
