import { inject, provide, reactive, type InjectionKey } from 'vue'
import { useGettext } from 'vue3-gettext'
import { usePlatform } from '../../platform'
import { useAgentStore } from '../../stores/agent'
import { ApiError, describeError, isAbortError } from '../../api/errors'
import type { Approval, Decision } from '../../api/types'
import { visibleText } from '../../util/visibleText'
import { targetsOf } from './targets'

// The most approvals one bulk decision may carry (the server's limit).
export const MAX_BULK = 100

// Risks whose approval needs an explicit confirmation. Unknown risks are
// treated like the dangerous ones.
export const needsConfirm = (risk: string): boolean => risk !== 'write' && risk !== 'read'

export interface Decisions {
  busy(id: string): boolean
  decide(a: Approval, d: Decision): Promise<void>
  decideAll(list: Approval[], d: Decision): Promise<void>
}

const KEY: InjectionKey<Decisions> = Symbol('ocis-agent-decisions')

// createDecisions sends the user's decisions through the store. Every
// decision carries the args_hash of the approval as it was displayed when
// the user clicked; an approval already being decided is never sent again.
// Failures become fixed toasts (the store refetches what is stale).
const createDecisions = (): Decisions => {
  const store = useAgentStore()
  const platform = usePlatform()
  const { $gettext, interpolate } = useGettext()
  const inflight = reactive(new Set<string>())

  const riskLabel = (risk: string) => {
    switch (risk) {
      case 'destructive':
        return $gettext('Destructive')
      case 'outward':
        return $gettext('Outward')
      case 'write':
        return $gettext('Write')
      case 'read':
        return $gettext('Read')
    }
    return interpolate($gettext('Unknown (%{risk})'), { risk: visibleText(risk).text }, true)
  }

  // the values are already plain text with visible control characters,
  // and the dialog shows text: no HTML escaping
  const line = (msgid: string, ctx: Record<string, string | number>) => interpolate(msgid, ctx, true)

  const describe = (a: Approval) =>
    [
      line($gettext('Tool: %{tool}'), { tool: visibleText(a.tool).text }),
      line($gettext('Risk: %{risk}'), { risk: riskLabel(a.risk) }),
      ...targetsOf(a.preview, a.args).map((t) => line($gettext('Affects: %{target}'), { target: t }))
    ].join('\n')

  const report = (e: unknown) => {
    if (isAbortError(e)) return
    if (e instanceof ApiError && e.code === 'stale_arguments') {
      platform.notify('error', $gettext('The action changed; review the updated request'))
      return
    }
    if (e instanceof ApiError && e.code === 'rate_limited' && e.retryAfter !== undefined) {
      platform.notify(
        'error',
        describeError(e, $gettext),
        line($gettext('Try again in %{seconds} seconds.'), { seconds: e.retryAfter })
      )
      return
    }
    platform.notify('error', describeError(e, $gettext))
  }

  const decide = async (a: Approval, d: Decision) => {
    if (inflight.has(a.id) || a.job_ended || a.state !== 'pending') return
    const id = a.id
    const hash = a.args_hash
    inflight.add(id)
    try {
      if (d === 'approve' && needsConfirm(a.risk)) {
        const ok = await platform.confirm({
          title: $gettext('Approve this action?'),
          message: describe(a),
          confirmText: $gettext('Approve'),
          danger: true
        })
        if (!ok) return
      }
      await store.decide(id, d, hash)
      platform.notify('success', d === 'approve' ? $gettext('Approved') : $gettext('Rejected'))
    } catch (e) {
      report(e)
    } finally {
      inflight.delete(id)
    }
  }

  const decideAll = async (list: Approval[], d: Decision) => {
    const items = list.filter((a) => !a.job_ended && a.state === 'pending')
    if (items.length === 0 || items.length > MAX_BULK) return
    if (items.some((a) => inflight.has(a.id))) return
    if (new Set(items.map((a) => a.job_id)).size !== 1) return
    const sent = items.map((a) => ({ id: a.id, args_hash: a.args_hash }))
    for (const it of sent) inflight.add(it.id)
    try {
      const dangerous = items.filter((a) => needsConfirm(a.risk))
      if (d === 'approve' && dangerous.length > 0) {
        const ok = await platform.confirm({
          title: line($gettext('Approve all %{count} actions?'), { count: items.length }),
          message: [
            line($gettext('%{count} of them cannot be undone or are visible to other people:'), {
              count: dangerous.length
            }),
            ...dangerous.map(describe)
          ].join('\n\n'),
          confirmText: line($gettext('Approve all %{count}'), { count: items.length }),
          danger: true
        })
        if (!ok) return
      }
      await store.bulkDecide(d, sent)
      platform.notify(
        'success',
        d === 'approve'
          ? line($gettext('Approved %{count} actions'), { count: sent.length })
          : line($gettext('Rejected %{count} actions'), { count: sent.length })
      )
    } catch (e) {
      report(e)
    } finally {
      for (const it of sent) inflight.delete(it.id)
    }
  }

  return { busy: (id) => inflight.has(id), decide, decideAll }
}

// provideDecisions shares one set of decisions (and the in-flight state)
// with every card and group below the calling component.
export const provideDecisions = (): Decisions => {
  const d = createDecisions()
  provide(KEY, d)
  return d
}

// useDecisions returns the provided decisions, or its own outside a page.
export const useDecisions = (): Decisions => inject(KEY, null) ?? createDecisions()
