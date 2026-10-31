import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, EmptyState, Field, FieldInput, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { dateTime, kindLabel, money } from '@/lib/format'
import type { Order, Page } from '@/lib/types'
import { adminPath, ConfirmAction, ListPagination, orderStatuses, Select, useAdminFilters, useAdminMutation } from './shared'

type OrderAction = { order: Order; type: 'mark-paid' | 'cancel' | 'refulfill' }
const actionLabels = { 'mark-paid': '确认收款', cancel: '取消订单', refulfill: '重新履约' }

export function Orders() {
  const { params, page, update, setPage } = useAdminFilters()
  const status = params.get('status') ?? ''
  const userID = params.get('user_id') ?? ''
  const query = useQuery({ queryKey: ['admin', 'orders', status, userID, page], queryFn: () => api.get<Page<Order>>(adminPath('/admin/orders', { status, user_id: userID, page, page_size: 20 })) })
  const [action, setAction] = useState<OrderAction | null>(null)
  const [tradeNo, setTradeNo] = useState('')
  const mutation = useAdminMutation((input: OrderAction) => api.post(`/admin/orders/${encodeURIComponent(input.order.order_no)}/${input.type}`, input.type === 'mark-paid' && tradeNo.trim() ? { trade_no: tradeNo.trim() } : {}), '订单操作已提交，列表已更新', () => setAction(null))
  function openAction(order: Order, type: OrderAction['type']) { mutation.reset(); setTradeNo(''); setAction({ order, type }) }
  return <div className="stack">
    <PageHeader eyebrow="交易管理" title="订单管理" description="核对收款、处理待支付订单与履约失败，金额单位为人民币。" />
    <Card><form key={userID} className="toolbar" onSubmit={(event) => { event.preventDefault(); const form = new FormData(event.currentTarget); update({ user_id: String(form.get('user_id') ?? '').trim() }) }}><Select label="订单状态" value={status} onChange={(event) => update({ status: event.target.value })}><option value="">全部状态</option>{Object.entries(orderStatuses).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</Select><Field label="用户编号"><FieldInput name="user_id" type="number" min={1} step={1} placeholder="全部用户" defaultValue={userID} /></Field><Button type="submit" variant="secondary">筛选用户</Button><Button type="button" variant="ghost" onClick={() => update({ status: '', user_id: '' })}>重置</Button></form>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>订单 / 用户</th><th>商品</th><th>金额 / 渠道</th><th>状态</th><th>时间</th><th>操作</th></tr></thead><tbody>{query.data.items.map((order) => <tr key={order.order_no}><td><span className="mono">{order.order_no}</span><p className="muted">{order.user_email || '用户信息暂不可用'}</p></td><td><strong>{order.product_name}</strong><p className="muted">{kindLabel[order.kind]} × {order.quantity}{order.renew_subscription_id ? ` · 续费订阅 #${order.renew_subscription_id}` : ''}</p></td><td className="mono">{money(order.amount_cents)}<p className="muted">{({ manual: '人工转账', epay: '在线支付', mock: '模拟支付', grant: '管理员赠送' } as Record<string, string>)[order.provider] ?? order.provider}</p></td><td><Badge status={order.status}>{orderStatuses[order.status] ?? order.status}</Badge></td><td><span>{dateTime(order.created_at)}</span><p className="muted">{order.paid_at ? `支付 ${dateTime(order.paid_at)}` : `到期 ${dateTime(order.expires_at)}`}</p>{order.fulfilled_at && <p className="muted">履约 {dateTime(order.fulfilled_at)}</p>}</td><td><div className="toolbar">{order.status === 'pending' && <><Button variant="secondary" onClick={() => openAction(order, 'mark-paid')}>确认收款</Button><Button variant="danger" onClick={() => openAction(order, 'cancel')}>取消</Button></>}{order.status === 'fulfill_failed' && <Button variant="secondary" onClick={() => openAction(order, 'refulfill')}>重新履约</Button>}{!['pending', 'fulfill_failed'].includes(order.status) && <span className="muted">无需处理</span>}</div></td></tr>)}</tbody></table></div> : <EmptyState title="没有符合条件的订单" description="尝试调整状态或用户编号筛选。" />}</QueryState>
      {!query.isError && <ListPagination data={query.data} onChange={setPage} />}
    </Card>
    {action && <ConfirmAction title={actionLabels[action.type]} description={action.type === 'mark-paid' ? `请先在支付平台或收款账户核实 ${money(action.order.amount_cents)} 已到账。确认后订单 ${action.order.order_no} 将被标记已支付并开始履约。` : action.type === 'cancel' ? `确认取消待支付订单 ${action.order.order_no}？此操作不会退款，也不能撤销。` : `确认重新履约订单 ${action.order.order_no}？系统将重试未完成的权益发放。`} label={actionLabels[action.type]} danger={action.type === 'cancel'} pending={mutation.isPending} error={mutation.error} onClose={() => setAction(null)} onConfirm={() => mutation.mutate(action)}>{action.type === 'mark-paid' && <Field label="支付流水号（可选）" hint="填写实际支付渠道流水号，便于核对。"><FieldInput value={tradeNo} maxLength={200} onChange={(event) => setTradeNo(event.target.value)} /></Field>}</ConfirmAction>}
  </div>
}
