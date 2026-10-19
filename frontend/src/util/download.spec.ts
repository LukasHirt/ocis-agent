import { afterEach, describe, expect, it, vi } from 'vitest'
import { REVOKE_DELAY_MS, saveBlob } from './download'

afterEach(() => {
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('saveBlob', () => {
  it('clicks a temporary download link to an object URL and revokes the URL afterwards', () => {
    vi.useFakeTimers()
    const create = vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:https://ocis.test/x')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    const clicked: Array<{ href: string; download: string; attached: boolean }> = []
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      clicked.push({ href: this.href, download: this.download, attached: document.body.contains(this) })
    })
    const blob = new Blob(['{}\n'])

    saveBlob(blob, 'audit-0a1b2c3d.jsonl')

    expect(create).toHaveBeenCalledWith(blob)
    expect(click).toHaveBeenCalledTimes(1)
    expect(clicked).toEqual([{ href: 'blob:https://ocis.test/x', download: 'audit-0a1b2c3d.jsonl', attached: true }])
    expect(document.querySelectorAll('a[download]')).toHaveLength(0) // removed again
    expect(revoke).not.toHaveBeenCalled()
    vi.advanceTimersByTime(REVOKE_DELAY_MS)
    expect(revoke).toHaveBeenCalledWith('blob:https://ocis.test/x')
  })

  it('removes the link and revokes the URL when the click throws', () => {
    vi.useFakeTimers()
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:y')
    const revoke = vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {
      throw new Error('blocked')
    })
    expect(() => saveBlob(new Blob(['x']), 'a.jsonl')).toThrow('blocked')
    expect(document.querySelectorAll('a[download]')).toHaveLength(0)
    vi.advanceTimersByTime(REVOKE_DELAY_MS)
    expect(revoke).toHaveBeenCalledWith('blob:y')
  })
})
