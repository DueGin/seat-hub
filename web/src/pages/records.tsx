import { useMemo } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link, useSearchParams } from 'react-router-dom'
import { ArrowRight, Layers, RefreshCw, Server } from 'lucide-react'
import { api } from '@/lib/api'
import type { Account, Page, UsageLog, UsageSummary } from '@/lib/types'
import { compact, date, dateTime, kindLabel, number, usd } from '@/lib/format'
import { Badge, Button, Card, EmptyState, PageHeader, Pagination, QueryState } from '@/components/ui'

const ranges = [['1', '今天'], ['7', '最近 7 天'], ['30', '最近 30 天']] as const
const PAGE_SIZE = 20

// Ranges are whole local days ending now, matching the usage page.
function rangeFor(days: string) {
  const until = new Date()
  const from = new Date(until)
  from.setHours(0, 0, 0, 0)
  from.setDate(from.getDate() - (Number(days) - 1))
  return { from: from.toISOString(), until: until.toISOString() }
}

function positiveInt(value: string | null) {
  const parsed = Number(value)
  return Number.isSafeInteger(parsed) && parsed > 0 ? parsed : 0
}

const statusText: Record<string, string> = { active: '运行中', needs_reauth: '待重新授权', awaiting_auth: '等待授权', provisioning: '准备中', allocating: '准备中', bootstrapping: '初始化中', suspended: '已暂停', failed: '异常', releasing: '释放中', released: '已结束' }

// accountSubtitle prefers the signed-in account's masked email and falls back
// to what the account is doing, never claiming a running account is unsigned.
function accountSubtitle(account: Account) {
  return account.email_masked || statusText[account.status] || account.status
}

function AccountChip({ account, active, onSelect }: { account: Account; active: boolean; onSelect: () => void }) {
  return <button type="button" className={`record-account ${active ? 'active' : ''} ${account.status === 'released' ? 'ended' : ''}`} aria-pressed={active} onClick={onSelect}>
    <span className={`account-icon ${account.kind}`}><Server size={16} /></span>
    <span className="record-account-text"><strong>{account.label || '未命名账号'}</strong><small className={account.email_masked ? 'mono' : undefined}>{accountSubtitle(account)}</small></span>
    <span className="record-account-kind">{account.status === 'released' ? '已结束' : kindLabel[account.kind]}</span>
  </button>
}

export function RecordsPage() {
  const [params, setParams] = useSearchParams()
  const accountId = positiveInt(params.get('account'))
  const days = ranges.some(([value]) => value === params.get('range')) ? params.get('range')! : '7'
  const status = params.get('status') === 'success' || params.get('status') === 'error' ? params.get('status')! : ''
  const page = positiveInt(params.get('page')) || 1
  // Recompute the range only when the selection changes, so paging keeps a
  // stable window instead of sliding "until" forward on every render.
  const range = useMemo(() => rangeFor(days), [days])

  function update(values: Record<string, string>, keepPage = false) {
    const next = new URLSearchParams(params)
    for (const [key, value] of Object.entries(values)) { if (value) next.set(key, value); else next.delete(key) }
    if (!keepPage) next.delete('page')
    setParams(next, { replace: true })
  }

  const accounts = useQuery({ queryKey: ['accounts', 'with-released'], queryFn: () => api.get<{ items: Account[] }>('/accounts?include_released=1') })
  const items = accounts.data?.items ?? []
  const live = items.filter((account) => account.status !== 'released')
  const ended = items.filter((account) => account.status === 'released')
  const selected = items.find((account) => account.id === accountId)

  const rangeParams = new URLSearchParams(range)
  const summary = useQuery({ queryKey: ['usage', range], queryFn: () => api.get<UsageSummary>(`/usage/summary?${rangeParams}`) })
  const logParams = new URLSearchParams({ ...range, page: String(page), page_size: String(PAGE_SIZE), ...(accountId ? { account_id: String(accountId) } : {}), ...(status ? { status } : {}) })
  const logs = useQuery({ queryKey: ['usage-logs', 'records', range, accountId, status, page], queryFn: () => api.get<Page<UsageLog>>(`/usage/logs?${logParams}`) })

  // Totals for the header strip: the selected account's row, or every account.
  const totals = useMemo(() => {
    const rows = summary.data?.by_account ?? []
    const scoped = accountId ? rows.filter((row) => row.account_id === accountId) : rows
    return scoped.reduce((sum, row) => ({ requests: sum.requests + row.requests, tokens: sum.tokens + row.tokens, cost: sum.cost + row.cost_usd }), { requests: 0, tokens: 0, cost: 0 })
  }, [summary.data, accountId])

  return <>
    <PageHeader eyebrow="每个账号，每一次调用" title="使用记录" description="查看你托管或租赁的账号上发生的每一次调用，包括模型、令牌与费用。" actions={<Button variant="ghost" onClick={() => { void accounts.refetch(); void summary.refetch(); void logs.refetch() }}><RefreshCw size={14} />刷新</Button>} />
    <Card>
      <div className="section-heading"><h2>选择账号</h2><span className="muted small">{live.length} 个使用中{ended.length ? ` · ${ended.length} 个已结束` : ''}</span></div>
      <QueryState query={accounts}>{items.length ? <div className="record-accounts" role="group" aria-label="选择账号">
        <button type="button" className={`record-account ${accountId ? '' : 'active'}`} aria-pressed={!accountId} onClick={() => update({ account: '' })}><span className="account-icon"><Layers size={16} /></span><span className="record-account-text"><strong>全部账号</strong><small>汇总所有账号的调用</small></span></button>
        {[...live, ...ended].map((account) => <AccountChip key={account.id} account={account} active={account.id === accountId} onSelect={() => update({ account: String(account.id) })} />)}
      </div> : <EmptyState title="还没有账号" description="托管你的 Claude 账号，或购买租赁账号后，这里会显示它们的使用记录。" action={<Link className="button button-secondary" to="/#products">查看产品方案 <ArrowRight size={14} /></Link>} />}</QueryState>
    </Card>
    {accountId > 0 && accounts.isSuccess && !selected && <Card><EmptyState title="找不到这个账号" description="它可能不属于当前登录的用户，或已被删除。" action={<Button variant="secondary" onClick={() => update({ account: '' })}>查看全部账号</Button>} /></Card>}
    {(!accountId || selected) && <Card>
      <div className="records-toolbar">
        <div className="records-scope">
          <h2>{selected ? selected.label || '未命名账号' : '全部账号'}</h2>
          <p className="muted small">{selected ? `${kindLabel[selected.kind]} · ${accountSubtitle(selected)} · 开通于 ${date(selected.created_at)}${selected.status === 'released' ? ' · 已结束' : selected.expires_at ? ` · ${date(selected.expires_at)} 到期` : ''}` : '所有托管与租赁账号上的调用'}</p>
        </div>
        <div className="row">
          <div className="tabs" role="tablist" aria-label="时间范围">{ranges.map(([value, label]) => <button key={value} role="tab" aria-selected={days === value} className={days === value ? 'active' : ''} onClick={() => update({ range: value === '7' ? '' : value })}>{label}</button>)}</div>
          <select className="input" aria-label="调用状态" value={status} onChange={(event) => update({ status: event.target.value })}><option value="">全部状态</option><option value="success">成功</option><option value="error">失败</option></select>
        </div>
      </div>
      <div className="records-totals">
        <div><span>调用次数</span><strong className="mono">{summary.isSuccess ? number(totals.requests) : '—'}</strong></div>
        <div><span>令牌总量</span><strong className="mono">{summary.isSuccess ? compact(totals.tokens) : '—'}</strong></div>
        <div><span>费用（美元）</span><strong className="mono">{summary.isSuccess ? usd(totals.cost) : '—'}</strong></div>
        {selected && selected.status !== 'released' && <Link className="records-detail-link" to={`/accounts/${selected.id}`}>查看账号额度 <ArrowRight size={14} /></Link>}
      </div>
      <QueryState query={logs}>{logs.data?.items.length ? <>
        <div className="table-wrap"><table className="data-table"><thead><tr><th>调用时间</th>{!accountId && <th>账号</th>}<th>模型</th><th>API 密钥</th><th>状态</th><th>输入 / 输出</th><th>缓存读 / 写</th><th>费用</th><th>首字 / 总耗时</th></tr></thead><tbody>
          {logs.data.items.map((log) => <tr key={log.request_id}>
            <td className="mono small">{dateTime(log.created_at)}</td>
            {!accountId && <td>{log.account_id ? <button type="button" className="link-button" onClick={() => update({ account: String(log.account_id) })}>{log.account_label}</button> : <span className="muted">—</span>}</td>}
            <td><span className="mono">{log.model}</span><small className="block muted">{log.stream ? '流式' : '非流式'}</small></td>
            <td>{log.key_name || <span className="muted">—</span>}</td>
            <td><Badge status={log.status_code < 400 ? 'active' : 'failed'}>{log.status_code < 400 ? '成功' : log.error_label || '请求失败'}</Badge></td>
            <td className="mono">{compact(log.input_tokens)} / {compact(log.output_tokens)}</td>
            <td className="mono">{compact(log.cache_read_tokens)} / {compact(log.cache_creation_tokens)}</td>
            <td className="mono">{usd(log.cost_usd)}</td>
            <td className="mono">{log.first_token_ms == null ? '—' : number(log.first_token_ms)} / {number(log.duration_ms)} ms</td>
          </tr>)}
        </tbody></table></div>
        <Pagination page={page} total={logs.data.total} pageSize={logs.data.page_size} onChange={(next) => update({ page: next > 1 ? String(next) : '' }, true)} />
      </> : <EmptyState title={status ? '没有符合条件的调用' : '这段时间还没有调用'} description={selected?.status === 'released' ? '账号已结束，只能查看持有期内的调用。' : '使用 API 密钥发起请求后，调用会在这里按时间倒序出现。'} action={!status ? <Link className="button button-secondary" to="/keys">查看接入方式 <ArrowRight size={14} /></Link> : undefined} />}</QueryState>
    </Card>}
  </>
}
