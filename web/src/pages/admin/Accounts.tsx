import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, EmptyState, Field, FieldInput, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { dateTime, kindLabel } from '@/lib/format'
import type { Account, Page } from '@/lib/types'
import { accountStatuses, adminPath, ConfirmAction, ListPagination, Select, useAdminFilters, useAdminMutation } from './shared'

type AccountAction = { account: Account; type: 'retry' | 'release' }

export function Accounts() {
  const { params, page, update, setPage } = useAdminFilters()
  const kind = params.get('kind') ?? ''
  const status = params.get('status') ?? ''
  const userID = params.get('user_id') ?? ''
  const query = useQuery({ queryKey: ['admin', 'accounts', kind, status, userID, page], queryFn: () => api.get<Page<Account>>(adminPath('/admin/accounts', { kind, status, user_id: userID, page, page_size: 20 })) })
  const [action, setAction] = useState<AccountAction | null>(null)
  const [confirmation, setConfirmation] = useState('')
  const mutation = useAdminMutation((input: AccountAction) => api.post(`/admin/accounts/${input.account.id}/${input.type}`, {}), '资源操作已提交，请刷新查看最新状态', () => setAction(null))
  function openAction(account: Account, type: AccountAction['type']) { mutation.reset(); setConfirmation(''); setAction({ account, type }) }
  return <div className="stack">
    <PageHeader eyebrow="资源运维" title="账号资源" description="查看全部用户的托管和租赁资源，追踪错误、重试失败步骤或强制释放。" actions={<Button variant="secondary" disabled={query.isFetching} onClick={() => { void query.refetch() }}>刷新状态</Button>} />
    <Card><form key={userID} className="toolbar" onSubmit={(event) => { event.preventDefault(); update({ user_id: String(new FormData(event.currentTarget).get('user_id') ?? '').trim() }) }}><Select label="资源类型" value={kind} onChange={(event) => update({ kind: event.target.value })}><option value="">全部类型</option><option value="hosted">托管账号</option><option value="rental">租赁账号</option></Select><Select label="资源状态" value={status} onChange={(event) => update({ status: event.target.value })}><option value="">全部状态</option>{Object.entries(accountStatuses).map(([value, text]) => <option key={value} value={value}>{text}</option>)}</Select><Field label="用户编号"><FieldInput name="user_id" type="number" min={1} step={1} placeholder="全部用户" defaultValue={userID} /></Field><Button type="submit" variant="secondary">筛选用户</Button><Button type="button" variant="ghost" onClick={() => update({ kind: '', status: '', user_id: '' })}>重置</Button></form>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>资源 / 用户</th><th>类型 / VM</th><th>状态</th><th>最近错误</th><th>时间</th><th>操作</th></tr></thead><tbody>{query.data.items.map((account) => <tr key={account.id}><td><strong>{account.label || `资源 #${account.id}`}</strong><p className="muted">{account.email_masked || '尚无账号信息'}</p><p className="muted">{account.user_email || '用户信息暂不可用'}</p></td><td>{kindLabel[account.kind]}<p className="mono muted">{account.vm_id || 'VM 尚未分配'}</p><p className="muted">资源 #{account.id}</p></td><td><Badge status={account.status} />{account.status_reason && <p className="muted">{account.status_reason}</p>}</td><td>{account.last_error ? <details><summary>查看最近错误</summary><p className="mono">{account.last_error}</p></details> : <span className="muted">暂无错误记录</span>}</td><td>{dateTime(account.created_at)}<p className="muted">{account.expires_at ? `到期 ${dateTime(account.expires_at)}` : '未设置到期时间'}</p></td><td><div className="toolbar">{account.status === 'failed' && <Button variant="secondary" onClick={() => openAction(account, 'retry')}>重试失败步骤</Button>}<Button variant="danger" disabled={['released', 'releasing'].includes(account.status)} onClick={() => openAction(account, 'release')}>强制释放</Button></div></td></tr>)}</tbody></table></div> : <EmptyState title="没有符合条件的资源" description="尝试调整资源类型、状态或用户编号。" />}</QueryState>
      {!query.isError && <ListPagination data={query.data} onChange={setPage} />}
    </Card>
    {action && <ConfirmAction title={action.type === 'release' ? '强制释放资源' : '重试失败步骤'} description={action.type === 'release' ? `即将强制释放「${action.account.label || `资源 #${action.account.id}`}」（${action.account.user_email || '用户信息未知'}）。可能终止在途请求、销毁托管凭证或归还租赁 VM；此操作不可撤销，不会自动退款。` : `确认重试资源 #${action.account.id} 的失败步骤？系统将继续执行资源创建或初始化流程。`} danger={action.type === 'release'} phrase={action.type === 'release' ? String(action.account.id) : undefined} confirmation={confirmation} onConfirmation={setConfirmation} label={action.type === 'release' ? '确认强制释放' : '确认重试'} pending={mutation.isPending} error={mutation.error} onClose={() => setAction(null)} onConfirm={() => mutation.mutate(action)} />}
  </div>
}
