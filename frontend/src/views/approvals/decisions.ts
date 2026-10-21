import { inject, provide, reactive, type InjectionKey } from 'vue'
import { useGettext } from 'vue3-gettext'
import { usePlatform } from '../../platform'
import { useAgentStore } from '../../stores/agent'
import { ApiError, describeError, isAbortError } from '../../api/errors'
import type { Approval, ApprovalState, Decision, Risk } from '../../api/types'
import { quote, targetsOf } from './targets'

// The most approvals one bulk decision may carry (the server's limit).
export const MAX_BULK = 100
// The most items a bulk confirmation lists.
export const MAX_CONFIRM_ITEMS = 10

// inflight holds the ids of the approvals being decided, shared by every
// page and card, so a page mounted again during a decision keeps them
// locked.
const inflight = reactive(new Set<string>())

// Risks whose approval needs an explicit confirmation. Unknown risks are
// treated like the dangerous ones.
export const needsConfirm = (risk: Approval['risk']): boolean =>
  risk !== ('write' satisfies Risk) && risk !== ('read' satisfies Risk)

export interface Decisions {
  busy(id: string): boolean
  // decide and decideAll resolve true only when the request was sent and
  // the server accepted it; false for a refused, locked, cancelled or
  // failed call.
  decide(a: Approval, d: Decision): Promise<boolean>
  decideAll(list: Approval[], d: Decision): Promise<boolean>
  // acknowledge records that the user checked the outcome of an action
  // that needs verification (no confirmation: it changes no file).
  acknowledge(a: Approval): Promise<void>
}

const KEY: InjectionKey<Decisions> = Symbol('ocis-agent-decisions')

// createDecisions sends the user's decisions (and acknowledgements)
// through the store. Every
// decision carries the args_hash of the approval as it was displayed when
// the user clicked; an approval already being decided is never sent again.
// Failures become fixed toasts (the store refetches what is stale).
const createDecisions = (): Decisions => {
  const store = useAgentStore()
  const platform = usePlatform()
  const { $gettext, interpolate } = useGettext()

  const riskLabel = (risk: Approval['risk']) => {
    switch (risk) {
      case 'destructive' satisfies Risk:
        return $gettext('Destructive')
      case 'outward' satisfies Risk:
        return $gettext('Outward')
      case 'write' satisfies Risk:
        return $gettext('Write')
      case 'read' satisfies Risk:
        return $gettext('Read')
    }
    return interpolate($gettext('Unknown (%{risk})'), { risk: quote(risk) }, true)
  }

  // the values are already plain text with visible control characters,
  // and the dialog shows text: no HTML escaping
  const line = (msgid: string, ctx: Record<string, string | number>) => interpolate(msgid, ctx, true)

  // describe is one line (the dialog collapses newlines): fixed labels and
  // separators, every untrusted value quoted.
  const describe = (a: Approval) => {
    const parts = [
      line($gettext('Tool: %{tool}'), { tool: quote(a.tool) }),
      line($gettext('Risk: %{risk}'), { risk: riskLabel(a.risk) })
    ]
    const targets = targetsOf(a.preview, a.args)
    if (targets.length > 0) {
      parts.push(line($gettext('Affects: %{targets}'), { targets: targets.map(quote).join(', ') }))
    }
    return parts.join(' — ')
  }

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

  const decide = async (a: Approval, d: Decision): Promise<boolean> => {
    if (inflight.has(a.id) || a.job_ended || a.state !== ('pending' satisfies ApprovalState)) return false
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
        if (!ok) return false
      }
      await store.decide(id, d, hash)
      platform.notify('success', d === 'approve' ? $gettext('Approved') : $gettext('Rejected'))
      return true
    } catch (e) {
      report(e)
      return false
    } finally {
      inflight.delete(id)
    }
  }

  const decideAll = async (list: Approval[], d: Decision): Promise<boolean> => {
    const items = list.filter((a) => !a.job_ended && a.state === ('pending' satisfies ApprovalState))
    if (items.length === 0 || items.length > MAX_BULK) return false
    if (items.some((a) => inflight.has(a.id))) return false
    if (new Set(items.map((a) => a.job_id)).size !== 1) return false
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
            ...dangerous.slice(0, MAX_CONFIRM_ITEMS).map((a, i) => line($gettext('[%{n}] %{action}'), { n: i + 1, action: describe(a) })),
            ...(dangerous.length > MAX_CONFIRM_ITEMS
              ? [line($gettext('… and %{count} more'), { count: dangerous.length - MAX_CONFIRM_ITEMS })]
              : [])
          ].join('  '),
          confirmText: line($gettext('Approve all %{count}'), { count: items.length }),
          danger: true
        })
        if (!ok) return false
      }
      await store.bulkDecide(d, sent)
      platform.notify(
        'success',
        d === 'approve'
          ? line($gettext('Approved %{count} actions'), { count: sent.length })
          : line($gettext('Rejected %{count} actions'), { count: sent.length })
      )
      return true
    } catch (e) {
      report(e)
      return false
    } finally {
      for (const it of sent) inflight.delete(it.id)
    }
  }

  const acknowledge = async (a: Approval) => {
    if (inflight.has(a.id) || !a.needs_verification) return
    const id = a.id
    inflight.add(id)
    try {
      await store.acknowledge(id)
      platform.notify('success', $gettext('Marked as checked'))
    } catch (e) {
      report(e)
    } finally {
      inflight.delete(id)
    }
  }

  return { busy: (id) => inflight.has(id), decide, decideAll, acknowledge }
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
