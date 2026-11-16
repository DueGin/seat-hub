import { useId, useRef, useState } from 'react'
import { ImagePlus } from 'lucide-react'
import { api } from '@/lib/api'
import { Button } from '@/components/ui'
import { Markdown } from './markdown'

export interface TicketAttachment { id: number; url: string; name: string; size: number; content_type: string }
export function MarkdownEditor({ value, onChange, attachments, onAttachments, onUploading, disabled = false, allowedAttachmentIDs = [] }: {
  value: string; onChange: (value: string) => void; attachments: TicketAttachment[]; onAttachments: (value: TicketAttachment[]) => void; onUploading?: (busy: boolean) => void; disabled?: boolean; allowedAttachmentIDs?: number[]
}) {
  const [preview, setPreview] = useState(false)
  const [progress, setProgress] = useState<number | null>(null)
  const [error, setError] = useState('')
  const fileInput = useRef<HTMLInputElement>(null)
  const textarea = useRef<HTMLTextAreaElement>(null)
  const current = useRef({ value, attachments }); current.current = { value, attachments }
  const busy = useRef(false)
  const editorID = useId()
  async function addImages(files: File[]) {
    if (disabled || busy.current || !files.length) return
    if (current.current.attachments.length + files.length > 6) { setError('每条消息最多添加 6 张图片'); return }
    busy.current = true; onUploading?.(true); setError('')
    let cursor = textarea.current?.selectionStart ?? current.current.value.length
    let end = textarea.current?.selectionEnd ?? cursor
    try {
      for (const file of files) {
        if (!['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(file.type)) throw new Error('只支持 PNG、JPEG、GIF 和 WebP 图片')
        if (!file.size || file.size > 5 * 1024 * 1024) throw new Error('图片不能为空，且不能超过 5 MB')
        setProgress(0)
        const image = await api.upload<TicketAttachment>('/tickets/attachments', file, setProgress)
        const name = image.name.replace(/[\[\]\\\r\n]/g, '') || '图片'
        const insert = `\n![${name}](${image.url})\n`
        const next = current.current.value.slice(0, cursor) + insert + current.current.value.slice(end)
        if ([...next].length > 20_000) throw new Error('正文最多 20000 字，请缩短内容后重新添加图片')
        current.current = { value: next, attachments: [...current.current.attachments, image] }
        onChange(next); onAttachments(current.current.attachments)
        cursor += insert.length; end = cursor
      }
      setPreview(false)
      requestAnimationFrame(() => { textarea.current?.focus(); textarea.current?.setSelectionRange(cursor, cursor) })
    } catch (cause) { setError(cause instanceof Error ? cause.message : '图片上传失败，请重试') }
    finally { setProgress(null); busy.current = false; onUploading?.(false) }
  }
  return <div className="ticket-editor" onDragOver={(event) => { if (event.dataTransfer.types.includes('Files')) event.preventDefault() }} onDrop={(event) => { if (event.dataTransfer.files.length) { event.preventDefault(); void addImages(Array.from(event.dataTransfer.files)) } }}>
    <div className="ticket-editor-toolbar"><div role="tablist" aria-label="正文模式"><button type="button" role="tab" aria-selected={!preview} aria-controls={editorID} onClick={() => setPreview(false)}>编辑</button><button type="button" role="tab" aria-selected={preview} aria-controls={editorID} onClick={() => setPreview(true)}>预览</button></div><Button type="button" variant="ghost" disabled={disabled || progress !== null || attachments.length >= 6} onClick={() => fileInput.current?.click()}><ImagePlus size={15} />添加图片</Button><input ref={fileInput} type="file" hidden multiple accept="image/png,image/jpeg,image/gif,image/webp" onChange={(event) => { void addImages(Array.from(event.target.files ?? [])); event.target.value = '' }} /></div>
    <div id={editorID} role="tabpanel">{preview ? <div className="ticket-preview">{value.trim() ? <Markdown body={value} attachmentIDs={[...allowedAttachmentIDs, ...attachments.map((a) => a.id)]} /> : <p className="muted">输入内容后可在这里预览。</p>}</div> : <textarea ref={textarea} className="ticket-textarea" aria-label="工单正文" placeholder="请描述遇到的问题、发生时间和已尝试的操作。支持 Markdown、粘贴或拖入图片。" value={value} disabled={disabled || progress !== null} onChange={(event) => onChange(event.target.value)} onPaste={(event) => { const files = Array.from(event.clipboardData.items).filter((item) => item.kind === 'file' && item.type.startsWith('image/')).map((item) => item.getAsFile()).filter((file): file is File => file !== null); if (files.length) { event.preventDefault(); void addImages(files) } }} />}</div>
    <div className="ticket-editor-footer"><span>支持 Markdown · 每张 5 MB 以内 · 最多 6 张</span><span>{[...value].length} / 20000 字</span></div>
    {attachments.length > 0 && <div className="ticket-upload-list">{attachments.map((image) => <span key={image.id}>{image.name}<button type="button" aria-label={`移除图片 ${image.name}`} disabled={disabled || progress !== null} onClick={() => { onAttachments(attachments.filter((a) => a.id !== image.id)); onChange(value.replaceAll(`![${image.name.replace(/[\[\]\\\r\n]/g, '') || '图片'}](${image.url})`, '')) }}>×</button></span>)}</div>}
    {progress !== null && <p className="ticket-upload-status" role="status">正在上传图片… {progress}%</p>}{error && <p className="ticket-error" role="alert">{error}</p>}
  </div>
}
