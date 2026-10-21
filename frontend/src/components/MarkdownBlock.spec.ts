import { describe, expect, it } from 'vitest'
import { mount } from '@vue/test-utils'
import MarkdownBlock from './MarkdownBlock.vue'
import { MAX_MD_CHARS } from '../util/markdown'

const TAGS = new Set('div p h3 h4 h5 ul ol li blockquote pre code hr table thead tbody tr th td strong em del br span'.split(' '))
const ATTRS = new Set(['class', 'start', 'title', 'aria-hidden'])

// every element and attribute must be on the allow-list
const walk = (root: Element) => {
  const all = [root, ...Array.from(root.querySelectorAll('*'))]
  for (const el of all) {
    expect(TAGS.has(el.tagName.toLowerCase()), `tag ${el.tagName}`).toBe(true)
    for (const a of Array.from(el.attributes)) {
      expect(ATTRS.has(a.name), `attribute ${a.name} on ${el.tagName}`).toBe(true)
      if (a.name === 'title') expect(el.classList.contains('agent-ctl')).toBe(true)
      if (a.name === 'aria-hidden') expect(el.classList.contains('agent-md-caret')).toBe(true)
      if (a.name === 'start') expect(el.tagName).toBe('OL')
    }
  }
  expect(root.querySelector('a, img, script, style, iframe, svg, form, input')).toBeNull()
}

const render = (text: string, props: Record<string, unknown> = {}) =>
  mount(MarkdownBlock, { props: { text, ...props } })

describe('MarkdownBlock', () => {
  const hostile = [
    '<script>alert(1)</script>',
    '<img src=x onerror=alert(1)>',
    '[click](javascript:alert(1))',
    '![a](https://evil/x.png)',
    '<a href=x>y</a>',
    '{{ 1+1 }}',
    '| a | b |\n|:-|-:|\n| <b>1</b> | [x](y) |',
    '***a **b *c ~~d~~ c* b** a***',
    '**a‮b​c** [l‮](u​) `c‮​`',
    '# h\n- a\n  - b\n> q\n1. one\n---\n```\ncode‮\n```'
  ]

  it.each(hostile)('only builds allow-listed elements and attributes for %j', (src) => {
    const w = render(src)
    walk(w.element)
    const w2 = render(src, { partial: true, caret: true })
    walk(w2.element)
  })

  it('shows hostile strings as text', () => {
    expect(render('<script>alert(1)</script>').text()).toBe('<script>alert(1)</script>')
    expect(render('<img src=x onerror=alert(1)>').text()).toBe('<img src=x onerror=alert(1)>')
    expect(render('<a href=x>y</a>').text()).toBe('<a href=x>y</a>')
    expect(render('{{ 1+1 }}').text()).toBe('{{ 1+1 }}')
  })

  it('never makes links clickable and shows label and url as text', () => {
    const w = render('[click](javascript:alert(1))')
    expect(w.find('a').exists()).toBe(false)
    expect(w.find('.agent-md-link').text()).toBe('click')
    expect(w.find('.agent-md-url').element.textContent).toBe(' (javascript:alert(1))')
    expect(w.text()).toBe('click (javascript:alert(1))')
    expect(w.element.querySelector('[href], [src]')).toBeNull()
  })

  it('shows the title of a link as part of the muted text, and extra table cells', () => {
    const w = render('[click](https://e.x/y "the title")')
    expect(w.find('.agent-md-url').element.textContent).toBe(' (https://e.x/y "the title")')
    const t = render('| a |\n|---|\n| 1 | extra |')
    expect(t.findAll('td').map((c) => c.text())).toEqual(['1', 'extra'])
  })

  it('shows the invisible characters at the edges of a line as escapes', () => {
    expect(render('\uFEFFhello').text()).toBe('\\u{FEFF}hello')
  })

  it('shows only the url when the label equals it', () => {
    const w = render('[https://e.x](https://e.x)')
    expect(w.find('.agent-md-link').exists()).toBe(false)
    expect(w.text()).toBe('https://e.x')
  })

  it('never loads images', () => {
    const w = render('![alt text](https://evil/x.png) ![](https://evil/y.png)')
    expect(w.find('img').exists()).toBe(false)
    const imgs = w.findAll('.agent-md-image')
    expect(imgs[0].text()).toBe('alt text (https://evil/x.png)')
    expect(imgs[1].text()).toBe('https://evil/y.png')
  })

  it('shows control and bidi characters as visible escapes in bold, code, labels and urls', () => {
    const w = render('**a‮b** `c​d` [l‮](u​)')
    expect(w.find('strong .agent-ctl').text()).toBe('\\u{202E}')
    expect(w.find('code .agent-ctl').text()).toBe('\\u{200B}')
    expect(w.find('.agent-md-link .agent-ctl').text()).toBe('\\u{202E}')
    expect(w.find('.agent-md-url .agent-ctl').text()).toBe('\\u{200B}')
    expect(w.element.textContent).not.toContain('‮')
    expect(w.element.textContent).not.toContain('​')
  })

  it('renders lists, headings, code and tables with the expected elements', () => {
    const w = render('# T\n\n- a\n- b\n\n3. x\n4. y\n\n```\nq\n```\n\n| h | i |\n|:-:|--:|\n| 1 | 2 |\n\n> quote\n\n---')
    expect(w.find('h3.agent-md-h3').text()).toBe('T')
    expect(w.findAll('ul > li')).toHaveLength(2)
    expect(w.find('ol').attributes('start')).toBe('3')
    expect(w.find('pre > code').text()).toBe('q')
    expect(w.find('th.agent-md-align-center').text()).toBe('h')
    expect(w.find('td.agent-md-align-right').text()).toBe('2')
    expect(w.find('blockquote').text()).toBe('quote')
    expect(w.find('hr').exists()).toBe(true)
    expect(w.find('li > p').exists()).toBe(false)
  })

  it('renders bold and inline code', () => {
    const w = render('**Files:** use `ls`')
    expect(w.find('strong').text()).toBe('Files:')
    expect(w.find('code.agent-md-code').text()).toBe('ls')
  })

  it('falls back to one pre-wrapped text block over the cap', () => {
    const src = 'a**'.repeat(MAX_MD_CHARS)
    const w = render(src.slice(0, MAX_MD_CHARS + 1))
    const texts = w.findAll('.agent-text')
    expect(texts).toHaveLength(1)
    expect(texts[0].attributes('style')).toContain('pre-wrap')
    expect(w.find('strong').exists()).toBe(false)
  })

  it('puts the caret inside the last paragraph and the last list item', () => {
    const p = render('one\n\ntwo', { caret: true })
    expect(p.findAll('.agent-md-caret')).toHaveLength(1)
    expect(p.findAll('p')[1].find('.agent-md-caret').exists()).toBe(true)
    expect(p.find('.agent-md-caret').attributes('aria-hidden')).toBe('true')
    const l = render('- a\n- b', { caret: true })
    expect(l.findAll('li')[1].find('.agent-md-caret').exists()).toBe(true)
    expect(l.findAll('li')[0].find('.agent-md-caret').exists()).toBe(false)
    const c = render('```\nx\n```', { caret: true })
    expect(c.find('pre code .agent-md-caret').exists()).toBe(true)
    const e = render('---', { caret: true })
    expect(e.element.lastElementChild?.classList.contains('agent-md-caret')).toBe(true)
    expect(render('x').find('.agent-md-caret').exists()).toBe(false)
  })

  it('passes partial through to the parser', () => {
    expect(render('**bol', { partial: true }).find('strong').text()).toBe('bol')
    expect(render('**bol').find('strong').exists()).toBe(false)
    expect(render('**bol').text()).toBe('**bol')
  })
})
