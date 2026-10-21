<script lang="ts">
import { computed, defineComponent, h, type VNodeChild } from 'vue'
import { useGettext } from 'vue3-gettext'
import TextBlock from './TextBlock.vue'
import { visibleNodes } from './visibleNodes'
import { parseMarkdown, type MdAlign, type MdBlock, type MdInline } from '../util/markdown'

type G = ReturnType<typeof useGettext>

// MarkdownBlock renders the parsed Markdown subset with h() as a fixed set of
// elements and classes. Links are never clickable (their target is shown as
// muted text), images are never loaded, raw HTML is literal, and every text
// node goes through visibleNodes, so control and bidi characters stay visible.
// Text that is too large for the parser is shown as plain text.
const plainOf = (c: MdInline[]): string =>
  c
    .map((n) => {
      switch (n.t) {
        case 'text':
        case 'code':
          return n.v
        case 'strong':
        case 'em':
        case 'del':
          return plainOf(n.c)
        default:
          return ''
      }
    })
    .join('')

const renderInline = (nodes: MdInline[], g: G): VNodeChild[] => {
  const out: VNodeChild[] = []
  for (const n of nodes) {
    switch (n.t) {
      case 'text':
        out.push(...visibleNodes(n.v, g))
        break
      case 'code':
        out.push(h('code', { class: 'agent-md-code' }, visibleNodes(n.v, g)))
        break
      case 'strong':
      case 'em':
      case 'del':
        out.push(h(n.t, { class: `agent-md-${n.t}` }, renderInline(n.c, g)))
        break
      case 'link': {
        const same = n.label.length === 0 || plainOf(n.label) === n.url
        if (!same) out.push(h('span', { class: 'agent-md-link' }, renderInline(n.label, g)))
        const shown = n.title === undefined ? n.url : `${n.url} "${n.title}"`
        out.push(h('span', { class: 'agent-md-url' }, visibleNodes(same ? shown : ` (${shown})`, g)))
        break
      }
      case 'image': {
        const target = n.title === undefined ? n.url : `${n.url} "${n.title}"`
        out.push(
          h('span', { class: 'agent-md-image' }, visibleNodes(n.alt === '' ? target : `${n.alt} (${target})`, g))
        )
        break
      }
      case 'br':
        out.push(h('br'))
        break
    }
  }
  return out
}

const caretNode = () => h('span', { class: 'agent-md-caret', 'aria-hidden': 'true' })

// hosts: does the block have a place for the caret
const hosts = (b: MdBlock): boolean => {
  switch (b.t) {
    case 'p':
    case 'h':
    case 'pre':
    case 'table':
      return true
    case 'ul':
    case 'ol': {
      const last = b.items[b.items.length - 1]
      return last !== undefined && last.some(hosts)
    }
    case 'quote':
      return b.c.some(hosts)
  }
  return false
}
const lastHost = (blocks: MdBlock[]): number => {
  for (let k = blocks.length - 1; k >= 0; k--) if (hosts(blocks[k])) return k
  return -1
}

const renderItem = (item: MdBlock[], g: G, caret: boolean): VNodeChild => {
  if (item.length === 1 && item[0].t === 'p') {
    const kids = renderInline(item[0].c, g)
    if (caret) kids.push(caretNode())
    return h('li', { class: 'agent-md-li' }, kids)
  }
  return h('li', { class: 'agent-md-li' }, renderBlocks(item, g, caret))
}

const alignClass = (a: MdAlign) => (a ? `agent-md-align-${a}` : undefined)

const renderBlock = (b: MdBlock, g: G, caret: boolean): VNodeChild => {
  switch (b.t) {
    case 'p': {
      const kids = renderInline(b.c, g)
      if (caret) kids.push(caretNode())
      return h('p', { class: 'agent-md-p' }, kids)
    }
    case 'h': {
      const kids = renderInline(b.c, g)
      if (caret) kids.push(caretNode())
      const tag = b.level === 3 ? 'h3' : b.level === 4 ? 'h4' : 'h5'
      return h(tag, { class: `agent-md-h agent-md-${tag}` }, kids)
    }
    case 'ul':
    case 'ol': {
      const items = b.items.map((it, k) => renderItem(it, g, caret && k === b.items.length - 1))
      if (b.t === 'ul') return h('ul', { class: 'agent-md-ul' }, items)
      const props: Record<string, unknown> = { class: 'agent-md-ol' }
      if (b.start !== 1 && Number.isInteger(b.start) && b.start >= 0 && b.start <= 999999999) props.start = b.start
      return h('ol', props, items)
    }
    case 'quote':
      return h('blockquote', { class: 'agent-md-quote' }, renderBlocks(b.c, g, caret))
    case 'pre': {
      const kids = visibleNodes(b.v, g)
      if (caret) kids.push(caretNode())
      return h('pre', { class: 'agent-md-pre' }, [h('code', null, kids)])
    }
    case 'hr':
      return h('hr', { class: 'agent-md-hr' })
    case 'table': {
      const cell = (tag: 'th' | 'td', c: MdInline[], k: number, withCaret: boolean) => {
        const kids = renderInline(c, g)
        if (withCaret) kids.push(caretNode())
        return h(tag, { class: alignClass(b.align[k] ?? null) }, kids)
      }
      const lastRow = b.rows[b.rows.length - 1]
      const head = h(
        'thead',
        null,
        h(
          'tr',
          null,
          b.head.map((c, k) => cell('th', c, k, caret && !lastRow && k === b.head.length - 1))
        )
      )
      const body = h(
        'tbody',
        null,
        b.rows.map((r, ri) =>
          h(
            'tr',
            null,
            r.map((c, k) => cell('td', c, k, caret && ri === b.rows.length - 1 && k === r.length - 1))
          )
        )
      )
      return h('div', { class: 'agent-md-table-wrap' }, [h('table', { class: 'agent-md-table' }, [head, body])])
    }
  }
  return null
}

function renderBlocks(blocks: MdBlock[], g: G, caret: boolean): VNodeChild[] {
  const host = caret ? lastHost(blocks) : -1
  const out = blocks.map((b, k) => renderBlock(b, g, k === host))
  if (caret && host < 0) out.push(caretNode())
  return out
}

export default defineComponent({
  name: 'MarkdownBlock',
  props: {
    text: { type: String, required: true },
    partial: { type: Boolean, default: false },
    caret: { type: Boolean, default: false }
  },
  setup(props) {
    const gettext = useGettext()
    const ast = computed(() => {
      try {
        return parseMarkdown(props.text, { partial: props.partial })
      } catch {
        return null
      }
    })
    return () => {
      const blocks = ast.value
      if (blocks === null) {
        const kids: VNodeChild[] = [h(TextBlock, { text: props.text, wrap: 'pre' })]
        if (props.caret) kids.push(caretNode())
        return h('div', { class: 'agent-md' }, kids)
      }
      return h('div', { class: 'agent-md' }, renderBlocks(blocks, gettext, props.caret))
    }
  }
})
</script>

<style>
.agent-md { line-height: 1.5; overflow-wrap: anywhere; min-width: 0; }
:where(.agent-md) :where(.agent-md-p, .agent-md-h, .agent-md-ul, .agent-md-ol, .agent-md-quote, .agent-md-pre, .agent-md-hr, .agent-md-table-wrap) { margin: 0; }
.agent-md > * + *, .agent-md-quote > * + * { margin-top: 0.6em; }
.agent-md-li > * + * { margin-top: 0.25em; }
.agent-md-h { font-weight: 600; line-height: 1.3; }
.agent-md > .agent-md-h:not(:first-child) { margin-top: 0.9em; }
.agent-md-h3 { font-size: var(--agent-font-l); }
.agent-md-h4 { font-size: var(--agent-font-m); }
.agent-md-h5 { font-size: var(--agent-font-s); }
.agent-md-ul, .agent-md-ol { padding-left: 1.5em; }
.agent-md-li + .agent-md-li { margin-top: 0.25em; }
.agent-md-code, .agent-md-pre { font-family: var(--agent-font-mono); font-size: 0.9em; background: var(--oc-color-background-default); border: var(--agent-line); border-radius: var(--agent-radius-s); }
.agent-md-code { padding: 0 0.3em; }
.agent-md-pre { padding: var(--agent-space-2) var(--agent-space-3); overflow-x: auto; white-space: pre; }
.agent-md-pre code { font: inherit; background: none; border: 0; padding: 0; }
.agent-md-quote { padding-left: var(--agent-space-3); border-left: 3px solid var(--oc-color-border); color: var(--oc-color-text-muted); }
.agent-md-hr { border: 0; border-top: var(--agent-line); }
.agent-md-table-wrap { overflow-x: auto; }
.agent-md-table { border-collapse: collapse; }
.agent-md-table :is(th, td) { border: var(--agent-line); padding: var(--agent-space-1) var(--agent-space-2); text-align: left; vertical-align: top; }
.agent-md-table th { font-weight: 600; background: var(--oc-color-background-default); }
.agent-md-table .agent-md-align-center { text-align: center; }
.agent-md-table .agent-md-align-right { text-align: right; }
.agent-md-url { color: var(--oc-color-text-muted); }
.agent-md-del { text-decoration: line-through; }
.agent-md-caret { display: inline-block; width: 0.45em; height: 1em; margin-left: 1px; vertical-align: text-bottom; background: currentColor; opacity: 0.5; animation: agent-caret 1s steps(1) infinite; }
@keyframes agent-caret { 50% { opacity: 0; } }
@media (prefers-reduced-motion: reduce) { .agent-md-caret { animation: none; } }
</style>
