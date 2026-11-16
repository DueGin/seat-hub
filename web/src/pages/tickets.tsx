import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { ArrowLeft, LifeBuoy, Plus, Send } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { dateTime } from '@/lib/format'
import type { Account, Order, Page } from '@/lib/types'
import { Badge, Button, Card, EmptyState, Field, FieldInput, Modal, Notice, PageHeader, Pagination, QueryState } from '@/components/ui'
import { Markdown } from '@/components/markdown'
import { MarkdownEditor } from '@/components/markdown-editor'
import type { TicketAttachment } from '@/components/markdown-editor'

export interface Ticket {
  id: number; user_id: number; user_email?: string; category: string; title: string; status: string; order_no?: string; account_id?: number
  created_at: string; updated_at: string; last_message_at: string; last_message_role: string; unread: boolean
  user_read_message_id: number; admin_read_message_id: number; user_read_at: string | null; admin_read_at: string | null
}
export interface TicketMessage { id: number; ticket_id: number; author_id: number; role: string; body: string; created_at: string; attachments: TicketAttachment[] }
export interface TicketDetail { ticket: Ticket; messages: TicketMessage[] }
export interface TicketPage extends Page<Ticket> { counts?: Record<string, number> }
export const ticketCategories: Record<string, string> = { account: '账号问题', billing: '订单与支付', api: 'API 调用', other: '其他' }
export const ticketStatuses: Record<string, string> = { open: '待处理', replied: '已回复', closed: '已关闭' }
export function TicketBadge({ status }: { status: string }) { return <Badge status={status === 'open' ? 'pending' : status === 'replied' ? 'active' : 'closed'}>{ticketStatuses[status] || status}</Badge> }
export function useTicketRefresh() {
  const client = useQueryClient()
  return () => { void client.invalidateQueries({ queryKey: ['tickets'] }); void client.invalidateQueries({ queryKey: ['ticket-unread'] }) }
}
export function TicketsPage() {
  const [status, setStatus] = useState('')
  const [page, setPage] = useState(1)
  const [creating, setCreating] = useState(false)
  const query = useQuery({ queryKey: ['tickets', 'user', status, page], queryFn: () => api.get<TicketPage>(`/tickets?status=${status}&page=${page}`) })
  return <div className="stack ticket-page">
    <PageHeader eyebrow="服务支持" title="工单" description="把问题留在这里，回复和处理进度都会保存在同一段对话中。" actions={<Button onClick={() => setCreating(true)}><Plus size={16} />新建工单</Button>} />
    <Card><div className="ticket-status-tabs" role="tablist" aria-label="工单状态">{Object.entries({ '': '全部工单', ...ticketStatuses }).map(([key, label]) => <button key={key} role="tab" aria-selected={status === key} onClick={() => { setStatus(key); setPage(1) }}>{label}</button>)}</div>
      <QueryState query={query}>{query.data?.items.length ? <div className="ticket-list">{query.data.items.map((ticket) => <Link className="ticket-list-item" to={`/tickets/${ticket.id}`} key={ticket.id}><div className="ticket-list-icon"><LifeBuoy size={19} /></div><div className="ticket-list-main"><div className="ticket-title-row"><strong>{ticket.title}</strong>{ticket.unread && <span className="ticket-unread-dot" aria-label="有未读回复" />}</div><p className="muted">#{ticket.id} · {ticketCategories[ticket.category]} · 更新于 {dateTime(ticket.last_message_at)}</p></div><TicketBadge status={ticket.status} /></Link>)}</div> : <EmptyState title={status ? '没有此状态的工单' : '还没有工单'} description="遇到账号、订单或调用问题时，可以在这里联系管理员。" action={<Button variant="secondary" onClick={() => setCreating(true)}>新建工单</Button>} />}</QueryState>
      {query.data && <Pagination page={page} total={query.data.total} onChange={setPage} />}
    </Card>{creating && <NewTicket onClose={() => setCreating(false)} />}
  </div>
}
function NewTicket({ onClose }: { onClose: () => void }) {
  const [category, setCategory] = useState('account')
  const [title, setTitle] = useState('')
  const [body, setBody] = useState('')
  const [order, setOrder] = useState('')
  const [account, setAccount] = useState('')
  const [attachments, setAttachments] = useState<TicketAttachment[]>([])
  const [uploading, setUploading] = useState(false)
  const navigate = useNavigate()
  const refresh = useTicketRefresh()
  const orders = useQuery({ queryKey: ['tickets', 'order-picker'], queryFn: async () => {
    const first = await api.get<Page<Order>>('/orders?page_size=100'); const items = [...first.items]
    for (let page = 2; items.length < first.total; page++) { const next = await api.get<Page<Order>>(`/orders?page_size=100&page=${page}`); if (!next.items.length) break; items.push(...next.items) }
    return items
  } })
  const accounts = useQuery({ queryKey: ['tickets', 'account-picker'], queryFn: () => api.get<{ items: Account[] }>('/accounts') })
  const create = useMutation({ mutationFn: () => api.post<TicketDetail>('/tickets', { category, title, body, ...(order ? { order_no: order } : {}), ...(account ? { account_id: Number(account) } : {}), attachment_ids: attachments.map((a) => a.id) }), onSuccess: (data) => { refresh(); toast.success('工单已提交'); navigate(`/tickets/${data.ticket.id}`) } })
  const busy = create.isPending || uploading
  return <Modal open onClose={() => { if (!busy) onClose() }} title="新建工单" wide footer={<><Button variant="secondary" disabled={busy} onClick={onClose}>取消</Button><Button form="new-ticket" type="submit" disabled={busy || !title.trim() || !body.trim() || [...title.trim()].length > 100 || [...body].length > 20_000}>{create.isPending ? '正在提交…' : '提交工单'}</Button></>}>
    <form id="new-ticket" className="stack" onSubmit={(event) => { event.preventDefault(); create.mutate() }}>
      <div className="ticket-form-grid"><label className="field"><span>问题分类</span><select className="input" value={category} disabled={busy} onChange={(event) => setCategory(event.target.value)}>{Object.entries(ticketCategories).map(([id, label]) => <option value={id} key={id}>{label}</option>)}</select></label><Field label="标题" hint="最多 100 字"><FieldInput required value={title} disabled={busy} onChange={(event) => setTitle(event.target.value)} placeholder="一句话说明遇到的问题" /></Field></div>
      <div className="ticket-form-grid"><label className="field"><span>关联订单（可选）</span><select className="input" value={order} disabled={busy || orders.isPending || orders.isError} onChange={(event) => setOrder(event.target.value)}><option value="">不关联订单</option>{orders.data?.map((o) => <option value={o.order_no} key={o.order_no}>{o.product_name} · {o.order_no}</option>)}</select>{orders.isError && <small>订单暂时无法加载，可稍后在正文中补充订单号。</small>}</label><label className="field"><span>关联账号（可选）</span><select className="input" value={account} disabled={busy || accounts.isPending || accounts.isError} onChange={(event) => setAccount(event.target.value)}><option value="">不关联账号</option>{accounts.data?.items.map((a) => <option value={a.id} key={a.id}>{a.label || `账号 #${a.id}`}</option>)}</select>{accounts.isError && <small>账号暂时无法加载，可在正文中描述账号。</small>}</label></div>
      <div className="field"><span>问题描述</span><MarkdownEditor value={body} onChange={setBody} attachments={attachments} onAttachments={setAttachments} onUploading={setUploading} disabled={create.isPending} /></div>
      {create.isError && <Notice danger>{create.error.message}</Notice>}
    </form>
  </Modal>
}
export function TicketConversation({ detail }: { detail: TicketDetail }) {
  const ids = detail.messages.flatMap((message) => message.attachments.map((a) => a.id))
  return <div className="ticket-conversation">{detail.messages.map((message) => <Card key={message.id} className={`ticket-message ${message.role === 'admin' ? 'ticket-message-admin' : ''}`}><header><span className="ticket-author-avatar">{message.role === 'admin' ? '管' : '用'}</span><strong>{message.role === 'admin' ? '管理员' : '用户'}</strong><time dateTime={message.created_at}>{dateTime(message.created_at)}</time></header><Markdown body={message.body} attachmentIDs={ids} />{message.attachments.some((a) => !message.body.includes(a.url)) && <div className="ticket-attachment-gallery">{message.attachments.filter((a) => !message.body.includes(a.url)).map((a) => <Markdown key={a.id} body={`![${a.name.replace(/[\[\]\\\r\n]/g, '')}](${a.url})`} attachmentIDs={ids} />)}</div>}</Card>)}</div>
}
export function TicketReply({ detail, admin = false }: { detail: TicketDetail; admin?: boolean }) {
  const [body, setBody] = useState('')
  const [attachments, setAttachments] = useState<TicketAttachment[]>([])
  const [uploading, setUploading] = useState(false)
  const refresh = useTicketRefresh()
  const mutation = useMutation({ mutationFn: () => api.post<TicketDetail>(`${admin ? '/admin' : ''}/tickets/${detail.ticket.id}/messages`, { body, attachment_ids: attachments.map((a) => a.id) }), onSuccess: () => { setBody(''); setAttachments([]); refresh(); toast.success('回复已发送') } })
  return <Card><form className="stack" onSubmit={(event) => { event.preventDefault(); mutation.mutate() }}><div className="section-heading"><div><h2>{admin ? '回复用户' : '继续对话'}</h2><p className="muted">{detail.ticket.status === 'closed' ? '回复会重新打开工单' : admin ? '回复后工单将标记为已回复。' : '补充信息后，管理员会继续处理。'}</p></div></div><MarkdownEditor value={body} onChange={setBody} attachments={attachments} onAttachments={setAttachments} onUploading={setUploading} disabled={mutation.isPending} allowedAttachmentIDs={detail.messages.flatMap((m) => m.attachments.map((a) => a.id))} />{mutation.isError && <Notice danger>{mutation.error.message}</Notice>}<div className="ticket-reply-actions"><Button type="submit" disabled={mutation.isPending || uploading || !body.trim() || [...body].length > 20_000}><Send size={15} />{mutation.isPending ? '正在发送…' : '发送回复'}</Button></div></form></Card>
}
export function TicketRelated({ ticket, admin = false }: { ticket: Ticket; admin?: boolean }) {
  return <div className="ticket-related"><span>分类：{ticketCategories[ticket.category]}</span><span>创建于 {dateTime(ticket.created_at)}</span>{ticket.order_no && <Link to={admin ? `/admin/orders?user_id=${ticket.user_id}` : `/orders/${encodeURIComponent(ticket.order_no)}`}>订单 {ticket.order_no}</Link>}{ticket.account_id && <Link to={admin ? `/admin/accounts?user_id=${ticket.user_id}` : `/accounts/${ticket.account_id}`}>账号 #{ticket.account_id}</Link>}</div>
}
export function TicketDetailPage() {
  const { id } = useParams()
  const refresh = useTicketRefresh()
  const client = useQueryClient()
  const query = useQuery({ queryKey: ['tickets', 'detail', id], queryFn: () => api.get<TicketDetail>(`/tickets/${id}`), refetchInterval: 60_000 })
  useEffect(() => { if (query.dataUpdatedAt) void client.invalidateQueries({ queryKey: ['ticket-unread'] }) }, [query.dataUpdatedAt, client])
  const close = useMutation({ mutationFn: () => api.post(`/tickets/${id}/close`), onSuccess: () => { refresh(); toast.success('工单已关闭') } })
  return <div className="stack ticket-page"><Link className="ticket-back" to="/tickets"><ArrowLeft size={15} />返回工单列表</Link><QueryState query={query}>{query.data && <><PageHeader eyebrow={`工单 #${query.data.ticket.id}`} title={query.data.ticket.title} actions={<><TicketBadge status={query.data.ticket.status} />{query.data.ticket.status !== 'closed' && <Button variant="secondary" disabled={close.isPending} onClick={() => close.mutate()}>{close.isPending ? '正在关闭…' : '关闭工单'}</Button>}</>} /><TicketRelated ticket={query.data.ticket} />{query.data.ticket.status === 'closed' && <Notice>此工单已关闭，回复会重新打开工单。</Notice>}{close.isError && <Notice danger>{close.error.message}</Notice>}<TicketConversation detail={query.data} /><TicketReply key={id} detail={query.data} /></>}</QueryState></div>
}
