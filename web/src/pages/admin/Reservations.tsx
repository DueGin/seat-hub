import { useId, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api, isMock } from '@/lib/api'
import { dateTime } from '@/lib/format'
import { Badge, Button, Card, EmptyState, Field, FieldInput, Modal, Notice, PageHeader, Pagination, QueryState } from '@/components/ui'
import { ConfirmAction, MutationError, Select, Textarea, useAdminMutation } from './shared'

type Reservation = { id: number; product_id: number; product_name: string; email: string; note: string; status: 'waiting' | 'notified'; created_at: string; notified_at: string | null }
type ProductCount = { product_id: number; product_name: string; total: number; waiting: number; notified: number }
type ReservationPage = { items: Reservation[]; total: number; counts: ProductCount[] }

export function Reservations() {
  const [product, setProduct] = useState('')
  const [status, setStatus] = useState('')
  const [email, setEmail] = useState('')
  const [page, setPage] = useState(1)
  const [removing, setRemoving] = useState<Reservation | null>(null)
  const [notifying, setNotifying] = useState<ProductCount | null>(null)
  const params = new URLSearchParams({ page: String(page), page_size: '20', ...(product ? { product_id: product } : {}), ...(status ? { status } : {}), ...(email ? { email } : {}) })
  const query = useQuery({ queryKey: ['admin', 'reservations', params.toString()], queryFn: () => api.get<ReservationPage>(`/admin/reservations?${params}`) })
  const remove = useAdminMutation((id: number) => api.delete(`/admin/reservations/${id}`), '预约记录已删除', () => setRemoving(null))
  const download = useMutation({ mutationFn: async () => {
    const path = `/admin/reservations/export?${params}`
    if (!isMock) { window.location.assign(`/api${path}`); return }
    const response = await (await import('@/lib/mock')).mockRequest(path, 'GET')
    if (!response.ok) throw new Error('导出失败，请刷新后重试。')
    const url = URL.createObjectURL(await response.blob())
    const link = document.createElement('a'); link.href = url; link.download = 'reservations.csv'; link.click(); URL.revokeObjectURL(url)
  } })
  const counts = query.data?.counts ?? []
  return <div className="stack">
    <PageHeader eyebrow="商业配置" title="预约管理" description="查看预约意向，准备开售后向待通知用户发送邮件。" actions={<Button variant="secondary" disabled={!query.data || download.isPending} onClick={() => download.mutate()}>导出 CSV</Button>} />
    <MutationError error={download.error} />
    <QueryState query={query}>
      <div className="split-grid">{counts.map(count => <Card key={count.product_id}><strong>{count.product_name}</strong><p className="mono">共 {count.total} 人 · 待通知 {count.waiting} 人</p><p className="muted">已通知 {count.notified} 人</p><Button variant="secondary" disabled={count.waiting === 0} onClick={() => setNotifying(count)}>通知预约用户</Button></Card>)}</div>
    </QueryState>
    <Card>
      <div className="toolbar">
        <Select label="商品" value={product} onChange={event => { setProduct(event.target.value); setPage(1) }}><option value="">全部商品</option>{counts.map(count => <option key={count.product_id} value={count.product_id}>{count.product_name}</option>)}</Select>
        <Select label="通知状态" value={status} onChange={event => { setStatus(event.target.value); setPage(1) }}><option value="">全部状态</option><option value="waiting">待通知</option><option value="notified">已通知</option></Select>
        <Field label="邮箱筛选"><FieldInput type="search" maxLength={254} placeholder="输入邮箱关键字" value={email} onChange={event => { setEmail(event.target.value); setPage(1) }} /></Field>
      </div>
      <QueryState query={query}>{query.data?.items.length ? <><div className="table-wrap"><table className="data-table"><thead><tr><th>商品 / 邮箱</th><th>备注</th><th>状态</th><th>预约时间</th><th>操作</th></tr></thead><tbody>{query.data.items.map(item => <tr key={item.id}><td><strong>{item.product_name}</strong><p>{item.email}</p></td><td style={{ maxWidth: 280, whiteSpace: 'pre-wrap', overflowWrap: 'anywhere' }}>{item.note || '—'}</td><td><Badge status={item.status === 'waiting' ? 'pending' : 'fulfilled'}>{item.status === 'waiting' ? '待通知' : '已通知'}</Badge>{item.notified_at && <p className="muted">{dateTime(item.notified_at)}</p>}</td><td>{dateTime(item.created_at)}</td><td><Button variant="danger" onClick={() => { remove.reset(); setRemoving(item) }}>删除</Button></td></tr>)}</tbody></table></div><Pagination page={page} total={query.data.total} onChange={setPage} /></> : <EmptyState title="没有符合条件的预约" description="用户在商品页预约后，记录会显示在这里。" />}</QueryState>
    </Card>
    {removing && <ConfirmAction title="删除预约" description={`确认删除 ${removing.email} 对「${removing.product_name}」的预约？`} danger pending={remove.isPending} error={remove.error} onClose={() => setRemoving(null)} onConfirm={() => remove.mutate(removing.id)} />}
    {notifying && <NotifyDialog product={notifying} onClose={() => setNotifying(null)} />}
  </div>
}

function NotifyDialog({ product, onClose }: { product: ProductCount; onClose: () => void }) {
  const id = useId()
  const [subject, setSubject] = useState(`${product.product_name} 已开售`)
  const [body, setBody] = useState('')
  const notify = useAdminMutation(async () => {
    const result = await api.post<{ count: number }>(`/admin/products/${product.product_id}/reservations/notify`, { subject, body })
    toast.success(`已为 ${result.count} 位预约用户加入邮件发送队列`)
  }, '通知已提交', onClose)
  return <Modal open title="通知预约用户" onClose={onClose} footer={<><Button variant="secondary" disabled={notify.isPending} onClick={onClose}>取消</Button><Button type="submit" form={id} disabled={notify.isPending}>{notify.isPending ? '正在提交…' : `通知 ${product.waiting} 位用户`}</Button></>}>
    <form id={id} className="stack" onSubmit={event => { event.preventDefault(); notify.mutate() }}>
      <Notice>将给「{product.product_name}」全部 {product.waiting} 位待通知用户各发送一封邮件。邮件加入队列后标记为已通知；请先确认商品已可购买。</Notice>
      <Field label="邮件标题"><FieldInput required maxLength={200} value={subject} onChange={event => setSubject(event.target.value)} /></Field>
      <Textarea label="邮件正文" hint="以纯文本发送，可附上商品页链接。" required rows={8} maxLength={20000} value={body} onChange={event => setBody(event.target.value)} />
      <MutationError error={notify.error} />
    </form>
  </Modal>
}
