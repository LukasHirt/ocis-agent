// How long an object URL of a saved file stays valid: the browser has
// started the download long before.
export const REVOKE_DELAY_MS = 10_000

// saveBlob hands a file to the browser's download: a temporary object URL
// and a hidden <a download> that is clicked and removed at once; the URL is
// revoked after REVOKE_DELAY_MS.
export const saveBlob = (blob: Blob, filename: string): void => {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  a.rel = 'noopener'
  a.style.display = 'none'
  document.body.appendChild(a)
  try {
    a.click()
  } finally {
    a.remove()
    setTimeout(() => URL.revokeObjectURL(url), REVOKE_DELAY_MS)
  }
}
