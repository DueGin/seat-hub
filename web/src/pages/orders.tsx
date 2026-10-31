import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link, useLocation, useParams, useSearchParams } from 'react-router-dom'
import { ArrowLeft, ArrowUpRight, RefreshCw } from 'lucide-react'
import { QRCodeSVG } from 'qrcode.react'
import { toast } from 'sonner'
import { api, isMock, safeExternalUrl } from '@/lib/api'
import { dateTime, kindLabel, money } from '@/lib/format'
import type { Order, Page, Payment, PublicConfig } from '@/lib/types'
import {
  Badge, Button, Card, CopyButton, EmptyState, Modal, Notice, PageHeader,
  Pagination, QueryState, StatCard,
} from '@/components/ui'

const pageSize = 20
const pendingStatuses = new Set(['pending', 'paid'])
const orderLabels: Record<string, string> = {
  pending: '待支付', paid: '开通中', fulfilled: '已完成', fulfill_failed: '开通失败',
  cancelled: '已取消', expired: '已过期', refunded: '已退款',
}
const orderMessages: Record<string, string> = {
  pending: '请在订单有效期内完成付款，支付后页面会自动更新。',
  paid: '已收到付款，正在为你开通资源，页面会自动更新。请勿重复支付。',
  fulfilled: '资源已开通，你可以在订阅与账号页面查看。',
  fulfill_failed: '付款已成功，但资源开通失败。请联系管理员并提供订单号，请勿重复支付。',
  cancelled: '订单已取消。如需购买，请返回订阅页面重新下单。',
  expired: '订单已过期，请勿继续支付。如已付款但状态未更新，请联系管理员并提供订单号。',
  refunded: '订单已退款。如需了解退款进度，请联系管理员。',
}

function OrderBadge({ status }: { status: string }) {
  return <Badge status={status}>{orderLabels[status] ?? '状态待确认'}</Badge>
}

function readPayment(state: unknown): Payment | undefined {
  if (!state || typeof state !== 'object' || !('payment' in state)) return undefined
  const payment = state.payment
  if (!payment || typeof payment !== 'object' || !('type' in payment)) return undefined
  if (payment.type !== 'redirect' && payment.type !== 'qrcode' && payment.type !== 'manual' && payment.type !== 'mock') return undefined
  return {
    type: payment.type,
    url: 'url' in payment && typeof payment.url === 'string' ? payment.url : undefined,
    qr: 'qr' in payment && typeof payment.qr === 'string' ? payment.qr : undefined,
    instructions: 'instructions' in payment && typeof payment.instructions === 'string' ? payment.instructions : undefined,
  }
}

export function OrdersPage() {
  const [searchParams, setSearchParams] = useSearchParams()
  const requestedPage = Number(searchParams.get('page') ?? 1)
  const page = Number.isSafeInteger(requestedPage) && requestedPage > 0 ? requestedPage : 1
  const ordersQuery = useQuery({
    queryKey: ['orders', page],
    queryFn: () => api.get<Page<Order>>(`/orders?page=${page}&page_size=${pageSize}`),
    refetchInterval: (query) => query.state.data?.items.some((order) => pendingStatuses.has(order.status)) ? 3_000 : false,
  })
  const orders = ordersQuery.data?.items ?? []

  function changePage(next: number) {
    const params = new URLSearchParams(searchParams)
    if (next === 1) params.delete('page')
    else params.set('page', String(next))
    setSearchParams(params)
  }

  return <div className="stack">
    <PageHeader title="我的订单" description="查看购买记录、支付进度与资源开通状态。" actions={<Link className="button button-primary" to="/subscriptions">购买或续费<ArrowUpRight size={16} /></Link>} />
    {isMock && <Notice>当前为演示环境，订单与支付状态仅用于演示。</Notice>}
    <Card>
      <div className="section-heading"><div><h2>订单记录</h2><p className="muted">待支付和开通中的订单将自动更新状态。</p></div><Button variant="ghost" aria-label="刷新订单列表" disabled={ordersQuery.isFetching} onClick={() => { void ordersQuery.refetch() }}><RefreshCw size={16} /></Button></div>
      <QueryState query={ordersQuery}>
        {!orders.length ? <EmptyState title={page > 1 ? '这一页没有订单' : '还没有订单'} description={page > 1 ? '订单数量可能已发生变化，请返回第一页查看。' : '购买资源后，可在这里追踪支付与开通进度。'} action={page > 1 ? <Button onClick={() => changePage(1)}>返回第一页</Button> : <Link className="button button-primary" to="/subscriptions">浏览订阅方案</Link>} /> : <div className="table-wrap"><table className="data-table">
          <thead><tr><th>订单 / 商品</th><th>类型</th><th>数量</th><th>金额</th><th>状态</th><th>创建时间</th><th>操作</th></tr></thead>
          <tbody>{orders.map((order) => <tr key={order.order_no}>
            <td><strong>{order.product_name}</strong><div className="mono muted">{order.order_no}</div>{order.renew_subscription_id !== null && <span className="muted">续费订单</span>}</td>
            <td>{kindLabel[order.kind] ?? '资源'}</td><td className="mono">{order.quantity}</td><td className="mono">{money(order.amount_cents)}</td>
            <td><OrderBadge status={order.status} /></td><td>{dateTime(order.created_at)}</td>
            <td><Link className="button button-secondary" to={`/orders/${encodeURIComponent(order.order_no)}`}>查看详情<ArrowUpRight size={14} /></Link></td>
          </tr>)}</tbody>
        </table></div>}
        {ordersQuery.data && <Pagination page={page} pageSize={pageSize} total={ordersQuery.data.total} onChange={changePage} />}
      </QueryState>
    </Card>
  </div>
}

export function OrderDetailPage() {
  const { orderNo } = useParams<{ orderNo: string }>()
  const location = useLocation()
  const queryClient = useQueryClient()
  const [cancelOpen, setCancelOpen] = useState(false)
  const payment = readPayment(location.state)
  const orderQuery = useQuery({
    queryKey: ['order', orderNo],
    queryFn: () => api.get<{ order: Order }>(`/orders/${encodeURIComponent(orderNo ?? '')}`),
    enabled: Boolean(orderNo),
    refetchInterval: (query) => {
      const status = query.state.data?.order.status
      return status && pendingStatuses.has(status) ? 2_000 : false
    },
  })
  const configQuery = useQuery({ queryKey: ['public-config'], queryFn: () => api.get<PublicConfig>('/public/config') })
  const order = orderQuery.data?.order

  useEffect(() => {
    if (!order || pendingStatuses.has(order.status)) return
    void queryClient.invalidateQueries({ queryKey: ['orders'] })
    if (order.status === 'fulfilled') {
      void queryClient.invalidateQueries({ queryKey: ['overview'] })
      void queryClient.invalidateQueries({ queryKey: ['subscriptions'] })
      void queryClient.invalidateQueries({ queryKey: ['accounts'] })
    }
  }, [order?.order_no, order?.status, queryClient])

  function refreshOrder() {
    void queryClient.invalidateQueries({ queryKey: ['order', orderNo] })
    void queryClient.invalidateQueries({ queryKey: ['orders'] })
  }

  const cancelOrder = useMutation({
    mutationFn: () => api.post(`/orders/${encodeURIComponent(orderNo ?? '')}/cancel`),
    onSuccess: () => {
      setCancelOpen(false)
      refreshOrder()
      toast.success('订单已取消')
    },
    onError: refreshOrder,
  })
  const mockPay = useMutation({
    mutationFn: () => api.post(`/orders/${encodeURIComponent(orderNo ?? '')}/mock-pay`),
    onSuccess: () => {
      refreshOrder()
      toast.success('模拟支付已提交，正在更新订单状态')
    },
    onError: refreshOrder,
  })
  const canMockPay = configQuery.data?.dev === true && order?.provider === 'mock' && order?.status === 'pending'
  const redirectUrl = safeExternalUrl(payment?.url)
  const providerName = configQuery.data?.payment_providers.find((provider) => provider.id === order?.provider)?.name
    ?? (order?.provider === 'mock' ? '模拟支付' : order?.provider === 'manual' ? '人工转账' : order?.provider === 'grant' ? '管理员赠送' : '在线支付')

  if (!orderNo) return <Card><EmptyState title="缺少订单号" description="请从订单列表重新进入订单详情。" action={<Link className="button button-primary" to="/orders">返回订单列表</Link>} /></Card>

  return <div className="stack">
    <PageHeader title="订单详情" description="支付与开通进度会在这里持续更新。" actions={<Link className="button button-secondary" to="/orders"><ArrowLeft size={16} />返回订单</Link>} />
    {isMock && <Notice>当前为演示环境，本页不会发起真实扣款。</Notice>}
    <QueryState query={orderQuery}>
      {order && <>
        <Card className="stack">
          <div className="section-heading"><div><h2>{order.product_name}</h2><p className="mono muted">{order.order_no}</p></div><OrderBadge status={order.status} /></div>
          <Notice success={order.status === 'fulfilled'}>{orderMessages[order.status] ?? '暂时无法识别订单状态，请刷新页面或联系管理员确认。'}</Notice>
          <div className="toolbar"><CopyButton value={order.order_no} label="复制订单号" /><Button variant="secondary" disabled={orderQuery.isFetching} onClick={() => { void orderQuery.refetch() }}><RefreshCw size={14} />{orderQuery.isFetching ? '正在更新…' : '刷新状态'}</Button></div>
          {pendingStatuses.has(order.status) && <p role="status" className="muted">正在自动查询订单状态，完成后会停止更新。</p>}
          {order.status === 'fulfilled' && <div className="row"><Link className="button button-primary" to="/subscriptions">查看订阅</Link><Link className="button button-secondary" to="/accounts">查看账号</Link></div>}
        </Card>

        <div className="metric-grid"><StatCard label="订单金额" value={money(order.amount_cents)} detail="人民币结算" /><StatCard label="购买数量" value={order.quantity} detail={kindLabel[order.kind] ?? '资源'} /><StatCard label="支付方式" value={providerName} detail={order.renew_subscription_id !== null ? '续费订单' : '新购订单'} /></div>

        <div className="split-grid">
          <Card className="stack">
            <div className="section-heading"><h2>订单信息</h2></div>
            <dl className="form-grid">
              <div><dt className="muted">订单号</dt><dd className="mono">{order.order_no}</dd></div>
              <div><dt className="muted">商品名称</dt><dd>{order.product_name}</dd></div>
              <div><dt className="muted">资源类型</dt><dd>{kindLabel[order.kind] ?? '资源'}</dd></div>
              <div><dt className="muted">购买数量</dt><dd>{order.quantity}</dd></div>
              <div><dt className="muted">创建时间</dt><dd>{dateTime(order.created_at)}</dd></div>
              <div><dt className="muted">支付截止</dt><dd>{dateTime(order.expires_at)}</dd></div>
              <div><dt className="muted">支付时间</dt><dd>{order.paid_at ? dateTime(order.paid_at) : '尚未支付'}</dd></div>
              <div><dt className="muted">开通时间</dt><dd>{order.fulfilled_at ? dateTime(order.fulfilled_at) : '尚未开通'}</dd></div>
              {order.renew_subscription_id !== null && <div><dt className="muted">续费订阅编号</dt><dd className="mono">{order.renew_subscription_id}</dd></div>}
            </dl>
            {order.status === 'pending' && <Button variant="danger" disabled={cancelOrder.isPending || mockPay.isPending} onClick={() => { cancelOrder.reset(); setCancelOpen(true) }}>取消订单</Button>}
          </Card>

          {order.status === 'pending' ? <Card className="stack">
            <div className="section-heading"><h2>完成支付</h2></div>
            <p>应付金额 <strong className="mono">{money(order.amount_cents)}</strong></p>
            <p className="muted">请于 {dateTime(order.expires_at)} 前完成支付，并核对收款信息与订单金额。</p>

            {!payment && <Notice>当前页面没有原始支付参数，刷新或直接打开订单链接后可能出现此情况。请查看下单时打开的原支付窗口，或联系管理员并提供订单号获取支付指引；请勿重复下单或重复付款。</Notice>}

            {payment?.type === 'redirect' && (redirectUrl ? <div className="stack"><p className="muted">在支付平台完成付款后，返回本页即可查看开通进度。</p><a className="button button-primary" href={redirectUrl} target="_blank" rel="noopener noreferrer">打开支付页面<ArrowUpRight size={16} /></a>{payment.instructions && <p style={{ whiteSpace: 'pre-wrap' }}>{payment.instructions}</p>}</div> : <Notice>支付链接缺失或无效，请联系管理员并提供订单号，或查看原支付窗口。</Notice>)}

            {payment?.type === 'qrcode' && (payment.qr?.trim() ? <div className="stack"><p>请使用对应支付应用扫描二维码。</p><div className="row"><QRCodeSVG value={payment.qr} size={208} marginSize={4} level="M" title="订单支付二维码" style={{ maxWidth: '100%', height: 'auto' }} /></div>{payment.instructions && <p style={{ whiteSpace: 'pre-wrap' }}>{payment.instructions}</p>}<p className="muted">付款后请等待状态同步，二维码仍显示不代表需要再次支付。</p></div> : <Notice>支付二维码暂不可用，请查看原支付窗口或联系管理员。</Notice>)}

            {payment?.type === 'manual' && <div className="stack">{payment.instructions?.trim() ? <><h3>转账说明</h3><p style={{ whiteSpace: 'pre-wrap' }}>{payment.instructions}</p><CopyButton value={payment.instructions} label="复制转账说明" /></> : <Notice>此订单使用人工转账，当前没有转账说明。请联系管理员确认收款信息后付款。</Notice>}<p className="muted">转账后请向管理员提供订单号和付款凭证。管理员确认收款后，页面会自动更新。</p></div>}

            {payment?.type === 'mock' && <Notice>此订单使用模拟支付，仅用于开发环境验证支付与开通流程。</Notice>}

            {order.provider === 'mock' && configQuery.isPending && <p role="status" className="muted">正在确认模拟支付是否可用…</p>}
            {order.provider === 'mock' && configQuery.isError && <Notice><p>暂时无法确认开发环境配置，请重试读取配置后再操作。</p><Button variant="secondary" onClick={() => { void configQuery.refetch() }}>重试读取配置</Button></Notice>}
            {order.provider === 'mock' && configQuery.isSuccess && !configQuery.data.dev && <Notice>此站点未开启开发模式，无法进行模拟支付。请联系管理员处理此订单。</Notice>}
            {payment?.type === 'mock' && order.provider !== 'mock' && <Notice>支付方式与订单不一致，请联系管理员核实。</Notice>}
            {canMockPay && <Button disabled={mockPay.isPending || cancelOrder.isPending} onClick={() => mockPay.mutate()}>{mockPay.isPending ? '正在提交模拟支付…' : '模拟支付成功'}</Button>}
            {mockPay.isError && <p role="alert">{mockPay.error.message}</p>}
          </Card> : <Card className="stack"><div className="section-heading"><h2>支付与开通</h2></div><OrderBadge status={order.status} /><p>{orderMessages[order.status] ?? '请联系管理员确认订单状态。'}</p><p className="muted">如有疑问，请复制订单号并联系管理员查询。</p><CopyButton value={order.order_no} label="复制订单号" /></Card>}
        </div>
      </>}
    </QueryState>

    <Modal open={cancelOpen} title="确认取消订单" onClose={() => { if (!cancelOrder.isPending) setCancelOpen(false) }}>
      <div className="stack">
        <p>确认取消订单 <span className="mono">{orderNo}</span>？</p>
        <Notice>取消后无法继续支付此订单。如已完成付款，请关闭此窗口并刷新订单状态；仍未更新时请联系管理员。</Notice>
        {cancelOrder.isError && <p role="alert">{cancelOrder.error.message}</p>}
        {order && order.status !== 'pending' && <Notice>订单状态已更新，当前无法取消。请关闭窗口查看最新状态。</Notice>}
        <div className="toolbar"><Button variant="secondary" disabled={cancelOrder.isPending} onClick={() => setCancelOpen(false)}>保留订单</Button><Button variant="danger" disabled={cancelOrder.isPending || order?.status !== 'pending'} onClick={() => cancelOrder.mutate()}>{cancelOrder.isPending ? '正在取消…' : '确认取消'}</Button></div>
      </div>
    </Modal>
  </div>
}
