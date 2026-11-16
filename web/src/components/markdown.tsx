import ReactMarkdown from 'react-markdown'
import remarkGfm from 'remark-gfm'
import { isMock } from '@/lib/api'
import { demoAttachmentURL } from '@/lib/mock-tickets'
import { isTicketImage, safeMarkdownLink } from './markdown-policy'

export function Markdown({ body, attachmentIDs }: { body: string; attachmentIDs?: readonly number[] }) {
  return <div className="ticket-markdown"><ReactMarkdown remarkPlugins={[remarkGfm]} components={{
    a: ({ href, children }) => <a href={safeMarkdownLink(href)} target="_blank" rel="noopener noreferrer nofollow">{children}</a>,
    img: ({ src, alt }) => isTicketImage(src, attachmentIDs)
      ? <img src={isMock ? demoAttachmentURL(src!) : src} alt={alt || '工单图片'} loading="lazy" />
      : <a href={safeMarkdownLink(src)} target="_blank" rel="noopener noreferrer nofollow">{alt || '外部图片'}</a>,
  }}>{body}</ReactMarkdown></div>
}
