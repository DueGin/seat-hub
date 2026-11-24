import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { Activity, Users, Wallet, Boxes } from 'lucide-react'
import { Badge, Button, Card, Notice, PageHeader, QueryState, StatCard } from '@/components/ui'
import { api } from '@/lib/api'
import { dateTime, money, number } from '@/lib/format'
import type { AdminDashboard, UpdateStatus } from '@/lib/types'
import { adminPath, useAdminMutation } from './shared'

export function Dashboard() {
  const query = useQuery({ queryKey: ['admin', 'dashboard'], queryFn: () => api.get<AdminDashboard>('/admin/dashboard'), refetchInterval: 30_000 })
  const data = query.data
  const recheck = useAdminMutation(() => api.post('/admin/scope/recheck', {}), '已开始重新检查，稍后刷新查看结果')
  // Cached on the server (checked against GitHub at most every 10 minutes).
  const updates = useQuery({ queryKey: ['admin', 'system', 'update'], queryFn: () => api.get<UpdateStatus>('/admin/system/update'), refetchInterval: 10 * 60_000, retry: false })
  return <div className="stack">
    <PageHeader eyebrow="运营中心" title="管理总览" description="关注收入、资源供给与服务健康，数据每 30 秒更新。" actions={<Button variant="secondary" disabled={query.isFetching} onClick={() => { void query.refetch() }}>刷新数据</Button>} />
    {updates.data?.last_failure && <Notice danger>上次更新到 {updates.data.last_failure.version} 未能成功，已自动回滚。<Link to="/admin/system">查看详情 →</Link></Notice>}
    {updates.data?.update_available && updates.data.latest && <Notice success>有新版本 {updates.data.latest.version} 可以更新。<Link to="/admin/system">前往系统更新 →</Link></Notice>}
    <QueryState query={query}>{data && <>
      <div className="metric-grid">
        <StatCard label="近 30 天收入" value={money(data.revenue_cents_30d)} detail="已收款订单 · 人民币" icon={<Wallet size={18} />} />
        <StatCard label="注册用户" value={number(data.users)} detail={`${number(data.paying_users)} 位付费用户`} icon={<Users size={18} />} />
        <StatCard label="待支付订单" value={number(data.orders_pending)} detail={<Link to="/admin/orders?status=pending">查看待支付订单 →</Link>} icon={<Activity size={18} />} />
        <StatCard label="运行中资源" value={number(data.accounts.hosted_active + data.accounts.rental_active)} detail={`托管 ${number(data.accounts.hosted_active)} · 租赁 ${number(data.accounts.rental_active)}`} icon={<Boxes size={18} />} />
      </div>
      <div className="scope-warnings">
        {data.scope_warnings.items.length > 0 && <Notice danger>
          <strong>有 {number(data.scope_warnings.items.length)} 个问题需要处理</strong>
          <ul className="scope-warning-items">{data.scope_warnings.items.map((item, index) => <li key={`${item.kind}/${item.user_id}/${item.vm_id}/${item.key_id}/${index}`}>
            {item.user_email && <><Link to={adminPath('/admin/users', { q: item.user_email })}>{item.user_email}</Link>： </>}{item.message}
          </li>)}</ul>
        </Notice>}
        <p className="row small muted">上次检查 {dateTime(data.scope_warnings.checked_at)} · <Button variant="secondary" disabled={recheck.isPending} onClick={() => recheck.mutate(undefined)}>{recheck.isPending ? '正在检查…' : '立即重新检查'}</Button></p>
      </div>
      {data.accounts.failed > 0 && <Notice>有 {number(data.accounts.failed)} 个资源处于失败状态。<Link to="/admin/accounts?status=failed"> 前往处理 →</Link></Notice>}
      <div className="split-grid">
        <Card><div className="section-heading"><h2>租赁池容量</h2><Link to="/admin/rental-pool">管理租赁池 →</Link></div><dl className="stack"><div className="row"><dt>总容量</dt><dd className="mono">{number(data.rental_pool.total)}</dd></div><div className="row"><dt>可分配</dt><dd><Badge status="available">{number(data.rental_pool.available)} 个可分配</Badge></dd></div><div className="row"><dt>已租出</dt><dd className="mono">{number(data.rental_pool.leased)}</dd></div><div className="row"><dt>维护中</dt><dd className="mono">{number(data.rental_pool.maintenance)}</dd></div></dl>{data.rental_pool.available === 0 && <p className="muted">暂无可分配资源，请补充候选 VM 或检查维护中的资源。</p>}</Card>
        <Card><div className="section-heading"><h2>vm2api 连接</h2><Badge status={data.vm2api.ok ? 'active' : 'failed'}>{data.vm2api.ok ? '连接正常' : '连接异常'}</Badge></div><dl className="stack"><div className="row"><dt>服务版本</dt><dd className="mono">{data.vm2api.version || '暂未获取'}</dd></div><div className="row"><dt>响应延迟</dt><dd className="mono">{data.vm2api.ok ? `${number(data.vm2api.latency_ms)} 毫秒` : '暂不可用'}</dd></div></dl>{data.vm2api.error && <Notice>{data.vm2api.error}</Notice>}<p className="muted">上游连接异常时，请检查服务地址、管理密钥及网络。真实用量不可用时不会以零值代替。</p></Card>
      </div>
    </>}</QueryState>
  </div>
}
