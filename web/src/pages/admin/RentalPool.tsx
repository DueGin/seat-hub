import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, EmptyState, Field, FieldInput, Modal, PageHeader, Pagination, QueryState } from '@/components/ui'
import { api, ApiError } from '@/lib/api'
import { dateTime, number } from '@/lib/format'
import type { PoolCandidate, PoolItem, SelfcheckCheck, UsageWindow } from '@/lib/types'
import { SelfcheckBadge, SelfcheckChecks } from '@/components/selfcheck'
import { ConfirmAction, credentialLabels, MutationError, scheduleLabels, Select, Textarea, useAdminMutation } from './shared'

const poolStatuses: Record<string, string> = { available: '可分配', leased: '已租出', maintenance: '维护中' }
const selfcheckMessages: Record<string, string> = { proxy_bound: '未绑定合格的独享 SOCKS5 出口', proxy_healthy: '出口代理健康检查失败', proxy_exclusive: '出口代理被其他槽位占用', exit_probe: '出口探测失败', exit_ip_unique: '出口 IP 与其他槽位重复', exit_geo_timezone: '出口地理时区与槽位时区不一致', fingerprint_present: '设备指纹信息不完整', fingerprint_unique: '设备指纹与其他槽位重复', account_unique: '该账号已在其它槽位托管' }
function admissionFailures(error: unknown): SelfcheckCheck[] {
  if (!(error instanceof ApiError)) return []
  if (error.checks.length) return error.checks
  if (error.code !== 'selfcheck_failed') return []
  return Object.entries(selfcheckMessages).filter(([code]) => error.message.includes(code)).map(([code, message]) => ({ code, ok: false, message }))
}

// The backend sends leased_to_email only when the lease can be joined to a user, so an
// empty email is normal. Fall back to the user id, and only report "暂无租用" when the pool
// item really has no tenant (or when a leased row is missing both fields).
function leaseLabel(item: PoolItem): string {
  const email = item.leased_to_email?.trim()
  if (email) return email
  if (item.leased_to_user_id) return `用户 #${item.leased_to_user_id}`
  return item.status === 'leased' ? '租用人信息缺失' : '暂无租用'
}

export function RentalPool() {
  const query = useQuery({ queryKey: ['admin', 'rental-pool'], queryFn: () => api.get<{ items: PoolItem[] }>('/admin/rental-pool') })
  const [search, setSearch] = useState('')
  const [status, setStatus] = useState('')
  const [page, setPage] = useState(1)
  const [adding, setAdding] = useState(false)
  const [editing, setEditing] = useState<PoolItem | null>(null)
  const [health, setHealth] = useState<PoolItem | null>(null)
  const [removing, setRemoving] = useState<PoolItem | null>(null)
  const remove = useAdminMutation((item: PoolItem) => api.delete(`/admin/rental-pool/${encodeURIComponent(item.vm_id)}`), 'VM 已移出租赁池', () => setRemoving(null))
  const selfcheck = useAdminMutation((item: PoolItem) => api.post(`/admin/rental-pool/${encodeURIComponent(item.vm_id)}/selfcheck`, {}), '入池自检已完成')
  const items = (query.data?.items ?? []).filter((item) => (!status || status === item.status) && (!search || `${item.vm_id} ${item.vm?.label ?? ''} ${item.vm?.email_masked ?? ''} ${item.leased_to_email ?? ''} ${item.leased_to_user_id ?? ''} ${item.note}`.toLowerCase().includes(search.toLowerCase())))
  const currentPage = Math.min(page, Math.max(1, Math.ceil(items.length / 20)))
  return <div className="stack">
    <PageHeader eyebrow="资源供给" title="租赁资源池" description="从未归属的 Claude VM 中选取资源入池，管理维护状态与健康信息。" actions={<><Button variant="secondary" disabled={query.isFetching} onClick={() => { void query.refetch() }}>刷新健康</Button><Button onClick={() => setAdding(true)}>候选 VM 入池</Button></>} />
    <Card><div className="toolbar"><Field label="搜索资源"><FieldInput type="search" value={search} placeholder="VM 编号、备注、邮箱或租用人" onChange={(event) => { setSearch(event.target.value); setPage(1) }} /></Field><Select label="池状态" value={status} onChange={(event) => { setStatus(event.target.value); setPage(1) }}><option value="">全部状态</option>{Object.entries(poolStatuses).map(([value, text]) => <option key={value} value={value}>{text}</option>)}</Select></div>
      <MutationError error={selfcheck.error} />
      <QueryState query={query}>{items.length ? <><div className="table-wrap"><table className="data-table"><thead><tr><th>VM / 账号</th><th>资源状态</th><th>自检状态</th><th>凭证 / 调度</th><th>租用人</th><th>备注</th><th>操作</th></tr></thead><tbody>{items.slice((currentPage - 1) * 20, currentPage * 20).map((item) => <tr key={item.vm_id}>
        <td><strong>{item.vm?.label || '未命名 VM'}</strong><p className="mono muted">{item.vm_id}</p><p className="muted">{item.vm?.email_masked || '暂无邮箱'} · {item.tier || item.vm?.tier || '未知规格'}</p></td>
        <td>
          {(!item.owned_by || item.status !== 'available') && <Badge status={item.status} />}
          {item.owned_by && <div className="rental-owner"><Badge status="failed">已被 vm2api 用户「{item.owned_by}」占用</Badge><p className="muted">不会分配给新订单。请在 vm2api 收回，或把它移出租赁池</p></div>}
        </td>
        <td><SelfcheckBadge value={item.selfcheck} />{item.selfcheck?.checked_at && <p className="muted">{dateTime(item.selfcheck.checked_at)}</p>}</td>
        <td><span>{credentialLabels[item.vm?.cred_status] ?? '凭证状态待确认'}</span><p className="muted">{scheduleLabels[item.vm?.schedule_state] ?? '调度状态未知'}</p></td>
        <td>{leaseLabel(item)}</td><td>{item.note || <span className="muted">无备注</span>}</td>
        <td><div className="toolbar"><Button variant="secondary" disabled={selfcheck.isPending} onClick={() => selfcheck.mutate(item)}>自检</Button><Button variant="secondary" onClick={() => setHealth(item)}>健康信息</Button><Button variant="secondary" disabled={item.status === 'leased'} onClick={() => setEditing(item)}>维护设置</Button><Button variant="danger" disabled={item.status === 'leased'} onClick={() => { remove.reset(); setRemoving(item) }}>移出</Button></div>{item.status === 'leased' && <p className="muted">租用中不可手动改状态或移出</p>}</td>
      </tr>)}</tbody></table></div><Pagination page={currentPage} total={items.length} onChange={setPage} /></> : <EmptyState title="暂无匹配的池资源" description="调整筛选，或从候选 VM 中选择资源入池。" action={<Button onClick={() => setAdding(true)}>查看候选 VM</Button>} />}</QueryState>
    </Card>
    {adding && <PoolCandidates onClose={() => setAdding(false)} />}
    {editing && <PoolEditor item={editing} onClose={() => setEditing(null)} />}
    {health && <PoolHealth item={query.data?.items.find((item) => item.vm_id === health.vm_id) ?? health} onClose={() => setHealth(null)} />}
    {removing && <ConfirmAction title="移出租赁池" description={`确认将 VM「${removing.vm?.label || removing.vm_id}」移出租赁池？此操作不会删除上游 VM，但该资源将不再参与租赁分配。`} danger pending={remove.isPending} error={remove.error} onClose={() => setRemoving(null)} onConfirm={() => remove.mutate(removing)} />}
  </div>
}

function PoolCandidates({ onClose }: { onClose: () => void }) {
  const query = useQuery({ queryKey: ['admin', 'rental-candidates'], queryFn: () => api.get<{ items: PoolCandidate[] }>('/admin/rental-pool/candidates') })
  const [selected, setSelected] = useState<string[]>([])
  const [search, setSearch] = useState('')
  const [note, setNote] = useState('')
  const [page, setPage] = useState(1)
  const mutation = useAdminMutation((ids: string[]) => api.post('/admin/rental-pool', { vm_ids: ids, note: note.trim() }), '所选 VM 已加入租赁池', onClose)
  const items = (query.data?.items ?? []).filter((item) => !search || `${item.vm_id} ${item.label} ${item.email_masked}`.toLowerCase().includes(search.toLowerCase()))
  const currentPage = Math.min(page, Math.max(1, Math.ceil(items.length / 20)))
  const validSelected = selected.filter((id) => query.data?.items.some((item) => item.vm_id === id))
  const failedChecks = admissionFailures(mutation.error)
  return <Modal open wide title="候选 VM 入池" onClose={() => { if (!mutation.isPending) onClose() }}><div className="stack"><p className="muted">只列出 vm2api 中未归属、未入池的 Claude VM。请先核对凭证与调度状态。</p><Field label="搜索候选 VM"><FieldInput type="search" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1) }} placeholder="名称、VM 编号或邮箱" /></Field><QueryState query={query}>{items.length ? <><div className="table-wrap"><table className="data-table"><thead><tr><th>选择</th><th>候选 VM</th><th>凭证</th><th>调度</th><th>规格</th></tr></thead><tbody>{items.slice((currentPage - 1) * 20, currentPage * 20).map((item) => <tr key={item.vm_id}><td><input type="checkbox" aria-label={`选择 ${item.label || item.vm_id}`} disabled={mutation.isPending} checked={selected.includes(item.vm_id)} onChange={(event) => setSelected((current) => event.target.checked ? [...current, item.vm_id] : current.filter((id) => id !== item.vm_id))} /></td><td><strong>{item.label || '未命名 VM'}</strong><p className="mono muted">{item.vm_id}</p><p className="muted">{item.email_masked || '暂无邮箱'}</p></td><td>{credentialLabels[item.cred_status] ?? '凭证状态待确认'}</td><td>{scheduleLabels[item.schedule_state] ?? '调度状态未知'}</td><td>{item.tier || '未知'}</td></tr>)}</tbody></table></div><Pagination page={currentPage} total={items.length} onChange={setPage} /></> : <EmptyState title="暂无可入池的候选 VM" description="请先在 vm2api 准备未归属的 Claude VM，或调整搜索条件。" />}</QueryState><Textarea label="入池备注（可选）" value={note} rows={2} disabled={mutation.isPending} onChange={(event) => setNote(event.target.value)} /><MutationError error={mutation.error} />{failedChecks.length > 0 && <SelfcheckChecks checks={failedChecks} detail />}<div className="toolbar"><span className="muted">已选择 {number(validSelected.length)} 个 VM（跨页保留）</span><Button variant="secondary" disabled={mutation.isPending || !validSelected.length} onClick={() => setSelected([])}>清空选择</Button><Button disabled={mutation.isPending || query.isError || !validSelected.length} onClick={() => mutation.mutate(validSelected)}>{mutation.isPending ? '正在入池…' : '确认入池'}</Button></div></div></Modal>
}

function PoolEditor({ item, onClose }: { item: PoolItem; onClose: () => void }) {
  const [status, setStatus] = useState(item.status === 'maintenance' ? 'maintenance' : 'available')
  const [note, setNote] = useState(item.note ?? '')
  const mutation = useAdminMutation(() => api.patch(`/admin/rental-pool/${encodeURIComponent(item.vm_id)}`, { status, note: note.trim() }), '资源池设置已保存', onClose)
  return <Modal open title="维护设置" onClose={() => { if (!mutation.isPending) onClose() }}><form className="stack" onSubmit={(event) => { event.preventDefault(); mutation.mutate(undefined) }}><p><strong>{item.vm?.label || '未命名 VM'}</strong> · <span className="mono">{item.vm_id}</span></p><Select label="资源池状态" value={status} onChange={(event) => setStatus(event.target.value)}><option value="available">可分配</option><option value="maintenance">维护中</option></Select><p className="muted">维护中的 VM 不会分配给新订单；恢复为可分配前，请确认健康信息。</p><Textarea label="维护备注" value={note} rows={3} onChange={(event) => setNote(event.target.value)} /><MutationError error={mutation.error} /><div className="toolbar"><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? '正在保存…' : '保存设置'}</Button></div></form></Modal>
}

function PoolHealth({ item, onClose }: { item: PoolItem; onClose: () => void }) {
  return <Modal open title="VM 健康信息" onClose={onClose}><div className="stack"><div className="row"><strong>{item.vm?.label || '未命名 VM'}</strong><Badge status={item.status} /></div><dl className="stack"><div className="row"><dt>VM 编号</dt><dd className="mono">{item.vm_id}</dd></div><div className="row"><dt>账号</dt><dd>{item.vm?.email_masked || '暂无'}</dd></div><div className="row"><dt>凭证</dt><dd>{credentialLabels[item.vm?.cred_status] ?? '状态待确认'} <span className="mono muted">{item.vm?.cred_status}</span></dd></div><div className="row"><dt>调度</dt><dd>{scheduleLabels[item.vm?.schedule_state] ?? '状态未知'}</dd></div><div className="row"><dt>规格</dt><dd>{item.vm?.tier || item.tier || '未知'}</dd></div></dl><WindowHealth label="5 小时窗口" value={item.vm?.five_hour} /><WindowHealth label="7 天窗口" value={item.vm?.seven_day} /><p className="muted">窗口缺失代表上游暂未提供数据，不等于零用量。关闭后可使用页面的「刷新健康」更新。</p></div></Modal>
}

function WindowHealth({ label, value }: { label: string; value?: UsageWindow | null }) {
  return <div className="stack"><h3>{label}</h3>{value ? <><div className="row"><span>已使用</span><strong className="mono">{number(value.utilization)}%</strong></div><progress max={100} value={Math.min(100, Math.max(0, value.utilization))} aria-label={`${label}利用率`} /><p className="muted">重置时间：{dateTime(value.resets_at)} · {value.status === 'ok' ? '状态正常' : '状态待确认'}</p></> : <p className="muted">暂无窗口数据</p>}</div>
}
