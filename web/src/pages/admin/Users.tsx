import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Badge, Button, Card, EmptyState, Field, FieldInput, Modal, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { dateTime, kindLabel, money, number } from '@/lib/format'
import type { Account, AdminUser, ApiKey, Order, Page, ProductKind, Subscription } from '@/lib/types'
import { adminPath, ConfirmAction, ListPagination, MutationError, Select, Textarea, useAdminFilters, useAdminMutation } from './shared'

type UserAction = { user: AdminUser; type: 'status' | 'role' }
interface UserDetail { user: AdminUser; subscriptions: Subscription[]; accounts: Account[]; keys: ApiKey[]; orders: Order[] }

export function Users() {
  const { params, page, update, setPage } = useAdminFilters()
  const search = params.get('q') ?? ''
  const query = useQuery({ queryKey: ['admin', 'users', search, page], queryFn: () => api.get<Page<AdminUser>>(adminPath('/admin/users', { q: search, page, page_size: 20 })) })
  const [action, setAction] = useState<UserAction | null>(null)
  const [grant, setGrant] = useState<AdminUser | null>(null)
  const [detail, setDetail] = useState<AdminUser | null>(null)
  const mutation = useAdminMutation((input: UserAction) => api.patch(`/admin/users/${input.user.id}`, input.type === 'status' ? { status: input.user.status === 'active' ? 'disabled' : 'active' } : { role: input.user.role === 'admin' ? 'user' : 'admin' }), '用户信息已更新', () => setAction(null))
  return <div className="stack">
    <PageHeader eyebrow="客户管理" title="用户管理" description="搜索客户、查看订阅与资源，管理访问权限和赠送权益。" />
    <Card><form key={search} className="toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: String(new FormData(event.currentTarget).get('q') ?? '').trim() }) }}><Field label="搜索用户"><FieldInput type="search" name="q" defaultValue={search} placeholder="邮箱或关键词" /></Field><Button type="submit" variant="secondary">搜索</Button><Button type="button" variant="ghost" onClick={() => update({ q: '' })}>重置</Button></form>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>用户</th><th>角色 / 状态</th><th>并发</th><th>权益使用</th><th>注册时间</th><th>操作</th></tr></thead><tbody>{query.data.items.map((user) => <tr key={user.id}><td><strong>{user.email}</strong><p className="muted mono">用户 #{user.id}</p></td><td><Badge>{user.role === 'admin' ? '管理员' : '普通用户'}</Badge><p><Badge status={user.status}>{user.status === 'active' ? '正常' : '已停用'}</Badge></p></td><td className="mono">{number(user.concurrency_total)}</td><td>托管 {user.hosted_used} / {user.hosted_total}<p className="muted">租赁 {user.rental_used} / {user.rental_total}</p></td><td>{dateTime(user.created_at)}</td><td><div className="toolbar"><Button variant="secondary" onClick={() => setDetail(user)}>详情</Button><Button variant="secondary" onClick={() => setGrant(user)}>赠送订阅</Button><Button variant={user.status === 'active' ? 'danger' : 'secondary'} onClick={() => { mutation.reset(); setAction({ user, type: 'status' }) }}>{user.status === 'active' ? '停用' : '启用'}</Button><Button variant="ghost" onClick={() => { mutation.reset(); setAction({ user, type: 'role' }) }}>{user.role === 'admin' ? '取消管理员' : '设为管理员'}</Button></div></td></tr>)}</tbody></table></div> : <EmptyState title="没有找到用户" description="请更换邮箱或搜索关键词。" />}</QueryState>
      {!query.isError && <ListPagination data={query.data} onChange={setPage} />}
    </Card>
    {action && <ConfirmAction title={action.type === 'status' ? action.user.status === 'active' ? '停用用户' : '启用用户' : '修改用户角色'} description={action.type === 'status' ? action.user.status === 'active' ? `确认停用 ${action.user.email}？将同步停用 vm2api 用户与密钥，影响其 API 访问。` : `确认恢复 ${action.user.email} 的访问权限？` : `确认将 ${action.user.email} ${action.user.role === 'admin' ? '改为普通用户，移除管理权限' : '设为管理员，授予全部后台管理权限'}？`} danger={action.type === 'role' || action.user.status === 'active'} pending={mutation.isPending} error={mutation.error} onClose={() => setAction(null)} onConfirm={() => mutation.mutate(action)} />}
    {grant && <GrantEditor user={grant} onClose={() => setGrant(null)} />}
    {detail && <UserDetails user={detail} onClose={() => setDetail(null)} />}
  </div>
}

function GrantEditor({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const [kind, setKind] = useState<ProductKind>('concurrency')
  const [quantity, setQuantity] = useState('1')
  const [days, setDays] = useState('30')
  const [note, setNote] = useState('')
  const [error, setError] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const mutation = useAdminMutation((body: { kind: ProductKind; quantity: number; duration_days: number; note: string }) => api.post(`/admin/users/${user.id}/grant`, body), '赠送订阅已生效', onClose)
  return <Modal open title="赠送订阅" onClose={() => { if (!mutation.isPending) onClose() }}><form className="stack" onSubmit={(event) => {
    event.preventDefault()
    if (![Number(quantity), Number(days)].every((value) => Number.isSafeInteger(value) && value > 0) || !note.trim() || !confirmed) { setError('数量和有效天数必须为正整数，请填写赠送原因并确认。'); return }
    setError(''); mutation.mutate({ kind, quantity: Number(quantity), duration_days: Number(days), note: note.trim() })
  }}><p>为 <strong>{user.email}</strong> 赠送权益，将生成一笔已履约的赠送订单。</p><Select label="权益类型" value={kind} onChange={(event) => setKind(event.target.value as ProductKind)}><option value="concurrency">并发</option><option value="hosted_seat">托管席位</option><option value="rental_account">租赁账号</option></Select><div className="form-grid"><Field label="权益数量"><FieldInput type="number" min={1} step={1} required value={quantity} onChange={(event) => setQuantity(event.target.value)} /></Field><Field label="有效天数"><FieldInput type="number" min={1} step={1} required value={days} onChange={(event) => setDays(event.target.value)} /></Field></div><Textarea label="赠送原因" required rows={3} maxLength={1000} value={note} onChange={(event) => setNote(event.target.value)} /><label className="row"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我已核对接收用户、权益数量和有效期</label><MutationError error={error ? new Error(error) : mutation.error} /><div className="toolbar"><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button disabled={mutation.isPending || !confirmed} type="submit">{mutation.isPending ? '正在赠送…' : '确认赠送'}</Button></div></form></Modal>
}

function UserDetails({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const query = useQuery({ queryKey: ['admin', 'user', user.id], queryFn: () => api.get<UserDetail>(`/admin/users/${user.id}`) })
  const [subscription, setSubscription] = useState<Subscription | null>(null)
  const data = query.data
  return <><Modal open wide title={`用户详情 · ${user.email}`} onClose={onClose}><QueryState query={query}>{data && <div className="stack">
    <div className="row"><span>用户 #{data.user.id} · {data.user.role === 'admin' ? '管理员' : '普通用户'}</span><Badge status={data.user.status}>{data.user.status === 'active' ? '正常' : '已停用'}</Badge></div><p className="muted">上游用户：<span className="mono">{data.user.vm2api_user_id ?? '尚未创建'}</span> · 注册于 {dateTime(data.user.created_at)}</p>
    <h3>订阅权益</h3>{data.subscriptions?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>订阅</th><th>状态</th><th>生效 / 到期</th><th>操作</th></tr></thead><tbody>{data.subscriptions.map((item) => <tr key={item.id}><td>#{item.id} · {kindLabel[item.kind]} × {item.quantity}<p className="muted mono">{item.source_order_no ? item.source_order_no : '—'}</p></td><td><Badge status={item.status}>{({ active: '生效中', expired: '已过期', cancelled: '已取消' } as Record<string, string>)[item.status] ?? item.status}</Badge></td><td>{dateTime(item.starts_at)}<p className="muted">{dateTime(item.expires_at)}</p></td><td><Button variant="secondary" disabled={item.status === 'cancelled'} onClick={() => setSubscription(item)}>调整订阅</Button></td></tr>)}</tbody></table></div> : <p className="muted">暂无订阅。</p>}
    <h3>账号资源</h3>{data.accounts?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>账号</th><th>类型</th><th>状态</th><th>到期</th></tr></thead><tbody>{data.accounts.map((item) => <tr key={item.id}><td>{item.label || `资源 #${item.id}`}<p className="muted">{item.email_masked || '尚无账号信息'}</p></td><td>{kindLabel[item.kind]}</td><td><Badge status={item.status} /></td><td>{dateTime(item.expires_at)}</td></tr>)}</tbody></table></div> : <p className="muted">暂无资源。</p>}
    <h3>访问密钥</h3>{data.keys?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>名称 / 前缀</th><th>状态</th><th>最后使用</th></tr></thead><tbody>{data.keys.map((item) => <tr key={item.id}><td>{item.name}<p className="mono muted">{item.prefix}…</p></td><td><Badge status={item.status}>{item.status === 'active' ? '已启用' : '已停用'}</Badge></td><td>{dateTime(item.last_used_at)}</td></tr>)}</tbody></table></div> : <p className="muted">暂无访问密钥。</p>}
    <div className="section-heading"><h3>订单记录</h3><Link to={`/admin/orders?user_id=${user.id}`} onClick={onClose}>筛选此用户订单 →</Link></div>{data.orders?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>订单</th><th>金额</th><th>状态</th></tr></thead><tbody>{data.orders.slice(0, 10).map((item) => <tr key={item.order_no}><td><span className="mono">{item.order_no}</span><p className="muted">{item.product_name}</p></td><td>{money(item.amount_cents)}</td><td><Badge status={item.status} /></td></tr>)}</tbody></table></div> : <p className="muted">暂无订单。</p>}{data.orders?.length > 10 && <p className="muted">这里只显示前 10 笔，请进入订单页查看全部记录。</p>}
  </div>}</QueryState></Modal>{subscription && <SubscriptionEditor subscription={subscription} onClose={() => setSubscription(null)} />}</>
}

function SubscriptionEditor({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const [expires, setExpires] = useState(subscription.expires_at)
  const [cancel, setCancel] = useState(false)
  const [confirmed, setConfirmed] = useState(false)
  const [error, setError] = useState('')
  const mutation = useAdminMutation((body: { expires_at?: string; status?: 'cancelled' }) => api.patch(`/admin/subscriptions/${subscription.id}`, body), '订阅已更新', onClose)
  return <Modal open title={`调整订阅 #${subscription.id}`} onClose={() => { if (!mutation.isPending) onClose() }}><form className="stack" onSubmit={(event) => {
    event.preventDefault()
    if (!confirmed) return
    if (cancel) { mutation.mutate({ status: 'cancelled' }); return }
    const parsed = new Date(expires)
    if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(expires) || Number.isNaN(parsed.getTime())) { setError('请填写带时区的 RFC3339 时间，例如 2026-12-31T23:59:59Z。'); return }
    setError(''); mutation.mutate({ expires_at: parsed.toISOString() })
  }}><Field label="到期时间（RFC3339）" hint={`当前本地时间：${dateTime(subscription.expires_at)}；提交时统一转为 UTC。`}><FieldInput required={!cancel} disabled={cancel} value={expires} onChange={(event) => setExpires(event.target.value)} /></Field><label className="row"><input type="checkbox" checked={cancel} onChange={(event) => setCancel(event.target.checked)} />取消此订阅，而非调整到期时间</label><label className="row"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />确认立即更改用户权益，可能影响在用资源</label><MutationError error={error ? new Error(error) : mutation.error} /><div className="toolbar"><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>返回</Button><Button variant={cancel ? 'danger' : 'primary'} type="submit" disabled={mutation.isPending || !confirmed}>{mutation.isPending ? '正在提交…' : cancel ? '确认取消订阅' : '保存到期时间'}</Button></div></form></Modal>
}
