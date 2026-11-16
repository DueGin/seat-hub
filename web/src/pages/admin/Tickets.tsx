import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { ArrowLeft, Search } from 'lucide-react'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { dateTime } from '@/lib/format'
import { Button, Card, EmptyState, Field, FieldInput, Notice, PageHeader, Pagination, QueryState } from '@/components/ui'
import { TicketBadge, TicketConversation, TicketRelated, TicketReply, ticketCategories, ticketStatuses, useTicketRefresh } from '@/pages/tickets'
import type { TicketDetail, TicketPage } from '@/pages/tickets'

export function Tickets() {
  const [params, setParams] = useSearchParams()
  const id = params.get('id')
  const status = params.get('status') || ''
  const category = params.get('category') || ''
  const q = params.get('q') || ''
  const user = params.get('user_id') || ''
  const rawPage = Number(params.get('page'))
  const page = Number.isSafeInteger(rawPage) && rawPage > 0 ? rawPage : 1
  const [search, setSearch] = useState(q)
  const [userFilter, setUserFilter] = useState(user)
  useEffect(() => { setSearch(q); setUserFilter(user) }, [q, user])
  function update(values: Record<string, string>) { const next = new URLSearchParams(params); next.delete('page'); for (const [key, value] of Object.entries(values)) { if (value) next.set(key, value); else next.delete(key) } setParams(next) }
  const queryString = new URLSearchParams({ status, category, q, user_id: user, page: String(page) })
  const query = useQuery({ queryKey: ['tickets', 'admin-list', queryString.toString()], queryFn: () => api.get<TicketPage>(`/admin/tickets?${queryString}`), enabled: !id, refetchInterval: 60_000 })
  if (id) return <AdminTicket id={id} onBack={() => update({ id: '' })} />
  return <div className="stack ticket-page"><PageHeader eyebrow="服务支持" title="工单管理" description="查看待处理问题、回复用户，并跟进每段对话。" />
    <Card><div className="ticket-status-tabs" role="tablist" aria-label="工单状态">{Object.entries({ '': '全部', ...ticketStatuses }).map(([key, label]) => <button key={key} role="tab" aria-selected={status === key} onClick={() => update({ status: key })}>{label}<span>{key ? query.data?.counts?.[key] ?? 0 : Object.values(query.data?.counts ?? {}).reduce((a, b) => a + b, 0)}</span></button>)}</div>
      <form className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: search.trim(), user_id: userFilter }) }}><label className="field"><span>分类</span><select className="input" value={category} onChange={(event) => update({ category: event.target.value })}><option value="">全部分类</option>{Object.entries(ticketCategories).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select></label><Field label="搜索"><FieldInput type="search" placeholder="标题、正文或邮箱" value={search} onChange={(event) => setSearch(event.target.value)} /></Field><Field label="用户 ID"><FieldInput type="number" min="1" step="1" placeholder="全部用户" value={userFilter} onChange={(event) => setUserFilter(event.target.value)} /></Field><Button type="submit" variant="secondary"><Search size={15} />搜索</Button>{(category || q || user) && <Button type="button" variant="ghost" onClick={() => update({ category: '', q: '', user_id: '' })}>清除筛选</Button>}</form>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>工单</th><th>用户</th><th>分类</th><th>状态</th><th>最近消息</th><th>操作</th></tr></thead><tbody>{query.data.items.map((ticket) => <tr key={ticket.id}><td><button className="link-button ticket-admin-title" onClick={() => update({ id: String(ticket.id) })}><strong>{ticket.title}</strong>{ticket.unread && <span className="ticket-unread-dot" aria-label="有未读消息" />}</button><p className="muted mono">#{ticket.id}</p></td><td><Link to={`/admin/users?q=${encodeURIComponent(ticket.user_email || `#${ticket.user_id}`)}`}>{ticket.user_email || `用户 #${ticket.user_id}`}</Link></td><td>{ticketCategories[ticket.category]}</td><td><TicketBadge status={ticket.status} /></td><td>{dateTime(ticket.last_message_at)}<p className="muted">{ticket.last_message_role === 'admin' ? '管理员回复' : '用户消息'}</p></td><td><Button variant="secondary" onClick={() => update({ id: String(ticket.id) })}>查看</Button></td></tr>)}</tbody></table></div> : <EmptyState title="没有符合条件的工单" description="调整筛选条件，或稍后再来查看。" />}</QueryState>
      {query.data && <Pagination page={page} total={query.data.total} onChange={(next) => { const updated = new URLSearchParams(params); updated.set('page', String(next)); setParams(updated) }} />}
    </Card>
  </div>
}
function AdminTicket({ id, onBack }: { id: string; onBack: () => void }) {
  const refresh = useTicketRefresh()
  const client = useQueryClient()
  const query = useQuery({ queryKey: ['tickets', 'admin-detail', id], queryFn: () => api.get<TicketDetail>(`/admin/tickets/${id}`), refetchInterval: 60_000 })
  useEffect(() => { if (query.dataUpdatedAt) void client.invalidateQueries({ queryKey: ['ticket-unread'] }) }, [query.dataUpdatedAt, client])
  const status = useMutation({ mutationFn: (value: string) => api.patch(`/admin/tickets/${id}`, { status: value }), onSuccess: () => { refresh(); toast.success('工单状态已更新') } })
  return <div className="stack ticket-page"><div><Button variant="ghost" onClick={onBack}><ArrowLeft size={15} />返回工单列表</Button></div><QueryState query={query}>{query.data && <><PageHeader eyebrow={`工单 #${id}`} title={query.data.ticket.title} actions={<><TicketBadge status={query.data.ticket.status} /><label className="ticket-status-select">更改状态<select className="input" value={query.data.ticket.status} disabled={status.isPending} onChange={(event) => status.mutate(event.target.value)}>{Object.entries(ticketStatuses).map(([key, label]) => <option key={key} value={key}>{label}</option>)}</select></label></>} /><Card><div className="ticket-user-summary"><strong>{query.data.ticket.user_email || `用户 #${query.data.ticket.user_id}`}</strong><Link to={`/admin/users?q=${encodeURIComponent(`#${query.data.ticket.user_id}`)}`}>查看用户资料 →</Link></div><TicketRelated ticket={query.data.ticket} admin /></Card>{status.isError && <Notice danger>{status.error.message}</Notice>}<TicketConversation detail={query.data} /><TicketReply key={id} detail={query.data} admin /></>}</QueryState></div>
}
