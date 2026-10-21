import { describe, expect, it } from 'vitest'
import { MAX_MD_CHARS, parseMarkdown, type MdBlock, type MdInline } from './markdown'

const parse = (s: string, partial = false): MdBlock[] => {
  const r = parseMarkdown(s, { partial })
  expect(r).not.toBeNull()
  return r as MdBlock[]
}
const T = (v: string): MdInline => ({ t: 'text', v })

// plain text of an inline list, markers excluded
const plain = (c: MdInline[]): string =>
  c
    .map((n) => {
      switch (n.t) {
        case 'text':
        case 'code':
          return n.v
        case 'strong':
        case 'em':
        case 'del':
          return plain(n.c)
        case 'link':
          return plain(n.label) + '(' + n.url + ')'
        case 'image':
          return n.alt + '(' + n.url + ')'
        case 'br':
          return '\n'
      }
    })
    .join('')

describe('parseMarkdown blocks', () => {
  it('parses a paragraph with line breaks', () => {
    expect(parse('a\nb')).toEqual([{ t: 'p', c: [T('a'), { t: 'br' }, T('b')] }])
  })

  it('splits paragraphs on blank lines and drops a trailing CR, spaces and backslash', () => {
    expect(parse('a  \r\n\r\nb\\')).toEqual([
      { t: 'p', c: [T('a')] },
      { t: 'p', c: [T('b')] }
    ])
  })

  it('maps heading levels to h3, h4, h5', () => {
    expect(parse('# a\n## b\n### c\n#### d\n###### e ##')).toEqual([
      { t: 'h', level: 3, c: [T('a')] },
      { t: 'h', level: 4, c: [T('b')] },
      { t: 'h', level: 5, c: [T('c')] },
      { t: 'h', level: 5, c: [T('d')] },
      { t: 'h', level: 5, c: [T('e')] }
    ])
    expect(parse('#nospace')).toEqual([{ t: 'p', c: [T('#nospace')] }])
  })

  it('parses fenced code verbatim, also unclosed', () => {
    expect(parse('```js\n**x**\n<b>\n```\nafter')).toEqual([
      { t: 'pre', v: '**x**\n<b>' },
      { t: 'p', c: [T('after')] }
    ])
    expect(parse('~~~\na\n```\nb')).toEqual([{ t: 'pre', v: 'a\n```\nb' }])
    expect(parse('```\na\n````\nb')).toEqual([{ t: 'pre', v: 'a' }, { t: 'p', c: [T('b')] }])
  })

  it('parses bullet lists', () => {
    expect(parse('- a\n* b\n+ c')).toEqual([
      { t: 'ul', items: [[{ t: 'p', c: [T('a')] }]] },
      { t: 'ul', items: [[{ t: 'p', c: [T('b')] }]] },
      { t: 'ul', items: [[{ t: 'p', c: [T('c')] }]] }
    ])
    expect(parse('- a\n- b')).toEqual([
      { t: 'ul', items: [[{ t: 'p', c: [T('a')] }], [{ t: 'p', c: [T('b')] }]] }
    ])
  })

  it('parses numbered lists with their start', () => {
    expect(parse('3) a\n4) b')).toEqual([
      { t: 'ol', start: 3, items: [[{ t: 'p', c: [T('a')] }], [{ t: 'p', c: [T('b')] }]] }
    ])
    expect(parse('1. a')).toMatchObject([{ t: 'ol', start: 1 }])
  })

  it('keeps task list brackets literal', () => {
    expect(parse('- [ ] a')).toEqual([{ t: 'ul', items: [[{ t: 'p', c: [T('[ ] a')] }]] }])
  })

  it('nests lists by indentation', () => {
    const r = parse('- a\n  - b\n- c')
    expect(r).toEqual([
      {
        t: 'ul',
        items: [
          [{ t: 'p', c: [T('a')] }, { t: 'ul', items: [[{ t: 'p', c: [T('b')] }]] }],
          [{ t: 'p', c: [T('c')] }]
        ]
      }
    ])
  })

  it('lets a list interrupt a paragraph', () => {
    expect(parse('intro\n- a')).toMatchObject([{ t: 'p' }, { t: 'ul' }])
  })

  it('caps block nesting at 4 and keeps deeper markers as text', () => {
    const src = Array.from({ length: 10 }, (_, k) => '  '.repeat(k) + '- L' + k).join('\n')
    let b: MdBlock = parse(src)[0]
    let depth = 0
    while (b.t === 'ul') {
      depth++
      const item: MdBlock[] = b.items[0]
      const nested = item.find((x) => x.t === 'ul')
      if (!nested) {
        expect(JSON.stringify(item)).toContain('- L4')
        break
      }
      b = nested
    }
    expect(depth).toBe(4)
  })

  it('nests quotes by repeated markers', () => {
    expect(parse('> > x')).toEqual([{ t: 'quote', c: [{ t: 'quote', c: [{ t: 'p', c: [T('x')] }] }] }])
    expect(parse('> a\nb')).toEqual([
      { t: 'quote', c: [{ t: 'p', c: [T('a')] }] },
      { t: 'p', c: [T('b')] }
    ])
  })

  it('parses horizontal rules, and --- after a paragraph is a rule, not a heading', () => {
    expect(parse('a\n---\n* * *\n___')).toEqual([{ t: 'p', c: [T('a')] }, { t: 'hr' }, { t: 'hr' }, { t: 'hr' }])
  })

  it('parses a table with alignment, missing and extra cells, escaped pipes', () => {
    const r = parse('| a | b | c |\n|:--|:-:|--:|\n| 1 | 2 |\n| x \\| y | 3 | 4 | 5 |')
    expect(r).toEqual([
      {
        t: 'table',
        align: ['left', 'center', 'right'],
        head: [[T('a')], [T('b')], [T('c')]],
        rows: [
          [[T('1')], [T('2')], []],
          [[T('x | y')], [T('3')], [T('4')]]
        ]
      }
    ])
  })

  it('parses inline markup in table cells', () => {
    const r = parse('a|b\n-|-\n**x**|`y`')
    expect(r[0]).toMatchObject({ t: 'table', rows: [[[{ t: 'strong' }], [{ t: 'code', v: 'y' }]]] })
  })

  it('turns a table over the column or row cap into a paragraph', () => {
    const cols = Array.from({ length: 31 }, () => 'a')
    const t31 = `|${cols.join('|')}|\n|${cols.map(() => '-').join('|')}|\n`
    expect(parse(t31)[0].t).toBe('p')
    const rows = '| a |\n|---|\n' + '| x |\n'.repeat(501)
    expect(parse(rows)[0].t).toBe('p')
    const ok = '| a |\n|---|\n' + '| x |\n'.repeat(500)
    expect(parse(ok)[0].t).toBe('table')
  })

  it('does not support setext headings, indented code or html blocks', () => {
    expect(parse('    code')).toEqual([{ t: 'p', c: [T('code')] }])
    expect(parse('<div>x</div>')).toEqual([{ t: 'p', c: [T('<div>x</div>')] }])
  })
})

describe('parseMarkdown inlines', () => {
  const inl = (s: string, partial = false) => (parse(s, partial)[0] as { c: MdInline[] }).c

  it('parses strong, em and del', () => {
    expect(inl('**a** __b__ *c* _d_ ~~e~~')).toEqual([
      { t: 'strong', c: [T('a')] },
      T(' '),
      { t: 'strong', c: [T('b')] },
      T(' '),
      { t: 'em', c: [T('c')] },
      T(' '),
      { t: 'em', c: [T('d')] },
      T(' '),
      { t: 'del', c: [T('e')] }
    ])
  })

  it('renders ***x*** as strong containing em', () => {
    expect(inl('***x***')).toEqual([{ t: 'strong', c: [{ t: 'em', c: [T('x')] }] }])
  })

  it('keeps snake_case_name and spaced asterisks as text', () => {
    expect(inl('snake_case_name')).toEqual([T('snake_case_name')])
    expect(inl('2 * 3 * 4')).toEqual([T('2 * 3 * 4')])
  })

  it('does not throw on crossed emphasis and keeps every letter', () => {
    const c = inl('**a *b** c*')
    const p = plain(c)
    for (const l of ['a', 'b', 'c']) expect(p).toContain(l)
  })

  it('parses code spans with backtick runs', () => {
    expect(inl('`a` ``b ` c`` ` x `')).toEqual([
      { t: 'code', v: 'a' },
      T(' '),
      { t: 'code', v: 'b ` c' },
      T(' '),
      { t: 'code', v: 'x' }
    ])
    expect(inl('`**x**`')).toEqual([{ t: 'code', v: '**x**' }])
    expect(inl('`unclosed')).toEqual([T('`unclosed')])
  })

  it('parses links as data, any scheme, title dropped', () => {
    expect(inl('[a](javascript:alert(1))')).toEqual([
      { t: 'link', label: [T('a')], url: 'javascript:alert(1)' }
    ])
    expect(inl('[a](https://e.x/y "t")')).toEqual([{ t: 'link', label: [T('a')], url: 'https://e.x/y' }])
    expect(inl('[a] (x)')).toEqual([T('[a] (x)')])
    expect(inl('[a](b c)')).toEqual([T('[a](b c)')])
  })

  it('parses images as data', () => {
    expect(inl('![x](https://e/x.png)')).toEqual([{ t: 'image', alt: 'x', url: 'https://e/x.png' }])
  })

  it('keeps raw html, entities, bare urls and autolinks as text', () => {
    expect(inl('<script>alert(1)</script>')).toEqual([T('<script>alert(1)</script>')])
    expect(inl('&amp; <https://e.x> https://e.x')).toEqual([T('&amp; <https://e.x> https://e.x')])
  })

  it('handles backslash escapes', () => {
    expect(inl('\\*x\\*')).toEqual([T('*x*')])
    expect(inl('a\\b')).toEqual([T('a\\b')])
  })

  it('caps inline nesting at 8 without losing text', () => {
    const src = '*a '.repeat(12) + 'z' + ' a*'.repeat(12)
    const depthOf = (c: MdInline[]): number =>
      Math.max(0, ...c.map((n) => (n.t === 'em' || n.t === 'strong' || n.t === 'del' ? 1 + depthOf(n.c) : 0)))
    const c = inl(src)
    expect(depthOf(c)).toBeLessThanOrEqual(8)
    expect(plain(c)).toContain('z')
  })

  it('treats delimiters beyond the stack limit as text', () => {
    const src = '*a'.repeat(100) + ' ' + 'b* '.repeat(100)
    expect(() => parse(src)).not.toThrow()
  })
})

describe('parseMarkdown partial mode', () => {
  const inl = (s: string, partial: boolean) => (parse(s, partial)[0] as { c: MdInline[] }).c

  it('closes unclosed inline openers of the last block only when partial', () => {
    expect(inl('**bol', true)).toEqual([{ t: 'strong', c: [T('bol')] }])
    expect(inl('*it', true)).toEqual([{ t: 'em', c: [T('it')] }])
    expect(inl('_it', true)).toEqual([{ t: 'em', c: [T('it')] }])
    expect(inl('~~x', true)).toEqual([{ t: 'del', c: [T('x')] }])
    expect(inl('`co', true)).toEqual([{ t: 'code', v: 'co' }])
    expect(inl('**bol', false)).toEqual([T('**bol')])
    expect(inl('`co', false)).toEqual([T('`co')])
  })

  it('does not close openers of earlier blocks', () => {
    const r = parse('**a\n\nb', true)
    expect(r[0]).toEqual({ t: 'p', c: [T('**a')] })
  })

  it('closes inside the last list item', () => {
    const r = parse('- one\n- **tw', true)
    expect(r[0]).toMatchObject({ t: 'ul', items: [[{ t: 'p' }], [{ t: 'p', c: [{ t: 'strong', c: [T('tw')] }] }]] })
  })

  it('shows only the label of an incomplete link', () => {
    expect(inl('see [lab', true)).toEqual([T('see lab')])
    expect(inl('see [lab](ht', true)).toEqual([T('see lab')])
    expect(inl('see [lab](https://e.x/p "ti', true)).toEqual([T('see lab')])
    expect(inl('see [lab](ht', false)).toEqual([T('see [lab](ht')])
    expect(inl('see [lab', false)).toEqual([T('see [lab')])
  })

  it('does not render a last line that only holds markers', () => {
    expect(parse('a\n\n##', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n- ', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n1.', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n> ', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n```', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n---', true)).toEqual([{ t: 'p', c: [T('a')] }])
    expect(parse('a\n\n-', false)).toMatchObject([{ t: 'p' }, { t: 'p' }])
    expect(parse('a\n\n1', true)).toMatchObject([{ t: 'p' }, { t: 'p' }])
  })

  it('shows a header without its delimiter row as a paragraph', () => {
    expect(parse('| a | b |', true)[0].t).toBe('p')
    expect(parse('| a | b |\n|---|---|\n| 1 | 2 |', true)[0].t).toBe('table')
  })
})

describe('parseMarkdown limits', () => {
  it('returns null over the character cap', () => {
    expect(parseMarkdown('a'.repeat(MAX_MD_CHARS))).not.toBeNull()
    expect(parseMarkdown('a'.repeat(MAX_MD_CHARS + 1))).toBeNull()
  })

  it('returns null over the node cap', () => {
    expect(parseMarkdown('*a* '.repeat(12000))).toBeNull()
  })

  it('returns null for a huge table', () => {
    const row = '|' + Array.from({ length: 30 }, () => 'x').join('|') + '|\n'
    const src = row + '|' + Array.from({ length: 30 }, () => '-').join('|') + '|\n' + row.repeat(499)
    expect(src.length).toBeLessThan(MAX_MD_CHARS)
    expect(parseMarkdown(src)).toBeNull()
  })

  it('keeps a hostile script tag as one text node', () => {
    const s = '<script>alert(1)</script>'
    expect(parse(s)).toEqual([{ t: 'p', c: [T(s)] }])
  })

  it('keeps bidi override characters in the text', () => {
    const r = parse('**a‮b**')
    expect(JSON.stringify(r)).toContain('\u202e')
  })

  it('treats link and url length limits as literal', () => {
    const long = 'x'.repeat(3000)
    expect(parse(`[a](${long})`)).toEqual([{ t: 'p', c: [T(`[a](${long})`)] }])
    const label = 'y'.repeat(1100)
    expect(parse(`[${label}](u)`)).toEqual([{ t: 'p', c: [T(`[${label}](u)`)] }])
  })
})

describe('parseMarkdown performance', () => {
  const N = MAX_MD_CHARS
  const fill = (unit: string) => unit.repeat(Math.floor(N / unit.length))
  const cases: [string, string][] = [
    ['backticks', fill('`')],
    ['asterisks', fill('*')],
    ['underscores', fill('_')],
    ['brackets', fill('[')],
    ['link closers', fill('](')],
    ['tildes', fill('~')],
    ['pipes', fill('|')],
    ['quotes', fill('>')],
    ['dash lines', fill('- \n')],
    ['indented dash lines', fill('  - \n')],
    ['asterisk sandwich', '*'.repeat(30000) + 'a' + '*'.repeat(30000)],
    ['bracket bomb', '['.repeat(30000) + '](' .repeat(10000)],
    ['open links', fill('[a](')],
    ['spaced backticks', fill('` a ')],
    ['mixed delimiters', fill('*a _b ~~c ')],
    ['nested emphasis', '*a '.repeat(10000) + 'z' + ' a*'.repeat(10000)],
    ['table rows', '|a|b|\n|-|-|\n' + fill('|x|y|\n')],
    ['nested lists', fill('  - x\n')],
    ['blank lines', fill('\n')],
    ['quote lines', fill('> a\n')]
  ]
  it.each(cases)('parses 64 KiB of %s quickly', (_name, src) => {
    for (const partial of [false, true]) {
      const t0 = performance.now()
      parseMarkdown(src, { partial })
      expect(performance.now() - t0).toBeLessThan(250)
    }
  })
})
