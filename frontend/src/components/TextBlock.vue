<script lang="ts">
import { computed, defineComponent, h, type PropType } from 'vue'
import { useGettext } from 'vue3-gettext'
import { visibleNodes } from './visibleNodes'

// TextBlock renders untrusted text as text nodes only. Control and bidi
// characters become visible \u{XXXX} escapes in a span.agent-ctl, so the
// DOM never contains anything but text nodes and those spans.
export default defineComponent({
  name: 'TextBlock',
  props: {
    text: { type: String, required: true },
    mono: { type: Boolean, default: false },
    maxLines: { type: Number, default: undefined },
    wrap: { type: String as PropType<'pre' | 'normal' | 'nowrap'>, default: 'pre' }
  },
  setup(props) {
    const gettext = useGettext()
    const style = computed(() => {
      const s: Record<string, string> =
        props.wrap === 'nowrap'
          ? { whiteSpace: 'nowrap' }
          : props.wrap === 'normal'
            ? { whiteSpace: 'normal', overflowWrap: 'anywhere' }
            : { whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }
      if (props.maxLines && props.maxLines > 0) {
        s.display = 'block'
        s.maxHeight = `calc(${Math.floor(props.maxLines)} * 1lh)`
        s.overflowY = 'auto'
      }
      return s
    })
    return () =>
      h(
        props.mono ? 'pre' : 'span',
        { class: ['agent-text', { 'agent-text-mono': props.mono }], style: style.value },
        visibleNodes(props.text, gettext)
      )
  }
})
</script>

<style>
.agent-text-mono {
  font-family: var(--oc-font-family-mono, monospace);
  margin: 0;
}
.agent-ctl {
  font-family: var(--oc-font-family-mono, monospace);
  color: var(--oc-color-swatch-danger-default, #c00);
  border: 1px dashed currentColor;
  border-radius: 2px;
  padding: 0 1px;
}
</style>
