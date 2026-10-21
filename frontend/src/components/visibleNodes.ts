import { h, type VNodeChild } from 'vue'
import { visibleSegments } from '../util/visibleText'

type Gettext = {
  $gettext: (s: string) => string
  interpolate: (s: string, p: Record<string, string>) => string
}

// visibleNodes turns untrusted text into text children and span.agent-ctl
// escapes for control characters; nothing else.
export const visibleNodes = (text: string, g: Gettext): VNodeChild[] =>
  visibleSegments(text).map((seg) =>
    seg.ctl
      ? h(
          'span',
          {
            class: 'agent-ctl',
            title: g.interpolate(
              g.$gettext('Invisible or control character %{code}, shown as its code'),
              { code: seg.code }
            )
          },
          seg.text
        )
      : seg.text
  )
