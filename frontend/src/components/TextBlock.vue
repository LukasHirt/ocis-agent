<script lang="ts">
import { computed, defineComponent, h } from 'vue'
import { useGettext } from 'vue3-gettext'
import { visibleSegments } from '../util/visibleText'

// TextBlock renders untrusted text as text nodes only. Control and bidi
// characters become visible \u{XXXX} escapes in a span.agent-ctl, so the
// DOM never contains anything but text nodes and those spans.
export default defineComponent({
  name: 'TextBlock',
  props: {
    text: { type: String, required: true },
    mono: { type: Boolean, default: false },
    maxLines: { type: Number, default: undefined }
  },
  setup(props) {
    const { $gettext, interpolate } = useGettext()
    const segments = computed(() => visibleSegments(props.text))
    const style = computed(() => {
      const s: Record<string, string> = { whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }
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
        segments.value.map((seg) =>
          seg.ctl
            ? h(
                'span',
                {
                  class: 'agent-ctl',
                  title: interpolate(
                    $gettext('Invisible or control character %{code}, shown as its code'),
                    { code: seg.code }
                  )
                },
                seg.text
              )
            : seg.text
        )
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
