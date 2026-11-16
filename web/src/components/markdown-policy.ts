// Only attachment paths admitted by this conversation may load as images.
export function isTicketImage(src: string | undefined, attachmentIDs?: readonly number[]): boolean {
  if (!src || !/^\/api\/tickets\/attachments\/[1-9]\d*$/.test(src)) return false
  const id = Number(src.slice(src.lastIndexOf('/') + 1))
  return Number.isSafeInteger(id) && (!attachmentIDs || attachmentIDs.includes(id))
}
export function safeMarkdownLink(src: string | undefined): string | undefined {
  if (!src) return undefined
  if (/^\/(?!\/)/.test(src)) return src
  try { const url = new URL(src); return ['https:', 'http:', 'mailto:'].includes(url.protocol) ? src : undefined } catch { return undefined }
}
