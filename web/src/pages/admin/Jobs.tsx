import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, EmptyState, Modal, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { dateTime, number } from '@/lib/format'
import type { Job, Page } from '@/lib/types'
import { adminPath, ConfirmAction, jobLabels, ListPagination, Select, useAdminFilters, useAdminMutation } from './shared'

const emptyPayloadText = '暂无载荷'

// The backend serializes payload as a JSON *string* (JobDTO.Payload is a Go string), so
// stringify-ing it again would show the escaped text. Parse a string first, re-format the
// result, and fall back to the raw text when it is not valid JSON. Objects stay supported
// in case the contract ever starts returning structured payloads.
export function payloadText(payload: unknown): string {
  if (payload === null || payload === undefined) return emptyPayloadText
  if (typeof payload === 'string') {
    const trimmed = payload.trim()
    if (!trimmed) return emptyPayloadText
    try { return JSON.stringify(JSON.parse(trimmed), null, 2) } catch { return payload }
  }
  try { return JSON.stringify(payload, null, 2) ?? String(payload) } catch { return String(payload) }
}

export function Jobs() {
  const { params, page, update, setPage } = useAdminFilters()
  const status = params.get('status') ?? 'failed'
  const query = useQuery({ queryKey: ['admin', 'jobs', status, page], queryFn: () => api.get<Page<Job>>(adminPath('/admin/jobs', { status: status === 'all' ? '' : status, page, page_size: 20 })), refetchInterval: status === 'running' || status === 'pending' ? 10_000 : false })
  const [retrying, setRetrying] = useState<Job | null>(null)
  const [detail, setDetail] = useState<Job | null>(null)
  const mutation = useAdminMutation((job: Job) => api.post(`/admin/jobs/${job.id}/retry`, {}), '任务已重新加入队列', () => setRetrying(null))
  return <div className="stack">
    <PageHeader eyebrow="执行队列" title="后台任务" description="查看失败原因和任务载荷；待处理与执行中队列每 10 秒更新。" actions={<Button variant="secondary" disabled={query.isFetching} onClick={() => { void query.refetch() }}>刷新队列</Button>} />
    <Card><div className="toolbar"><Select label="任务状态" value={status} onChange={(event) => update({ status: event.target.value })}><option value="failed">失败任务</option><option value="pending">待处理</option><option value="running">执行中</option><option value="all">全部任务</option></Select></div>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>任务</th><th>状态</th><th>尝试次数</th><th>执行时间</th><th>最近错误</th><th>操作</th></tr></thead><tbody>{query.data.items.map((job) => <tr key={job.id}><td><strong>{jobLabels[job.type] ?? '后台任务'} #{job.id}</strong><p className="mono muted">{job.type}</p></td><td><Badge status={job.status} /></td><td className="mono">{number(job.attempts)}</td><td>{dateTime(job.run_after)}<p className="muted">更新 {dateTime(job.updated_at)}</p></td><td>{job.last_error ? <details><summary>查看失败原因</summary><p className="mono">{job.last_error}</p></details> : <span className="muted">暂无错误</span>}</td><td><div className="toolbar"><Button variant="secondary" onClick={() => setDetail(job)}>详情</Button>{job.status === 'failed' && <Button onClick={() => { mutation.reset(); setRetrying(job) }}>重试任务</Button>}</div></td></tr>)}</tbody></table></div> : <EmptyState title="当前队列没有任务" description="切换状态查看其他任务，或稍后刷新。" />}</QueryState>
      {!query.isError && <ListPagination data={query.data} onChange={setPage} />}
    </Card>
    {retrying && <ConfirmAction title="重试失败任务" description={`确认将任务 #${retrying.id} 重新加入执行队列？请先核对失败原因，避免重复触发同类故障。`} pending={mutation.isPending} error={mutation.error} onClose={() => setRetrying(null)} onConfirm={() => mutation.mutate(retrying)} label="确认重试" />}
    {detail && <Modal open wide title={`任务详情 #${detail.id}`} onClose={() => setDetail(null)}><div className="stack"><dl className="stack"><div className="row"><dt>任务类型</dt><dd className="mono">{detail.type}</dd></div><div className="row"><dt>状态</dt><dd><Badge status={detail.status} /></dd></div><div className="row"><dt>尝试次数</dt><dd>{detail.attempts}</dd></div><div className="row"><dt>创建时间</dt><dd>{dateTime(detail.created_at)}</dd></div><div className="row"><dt>最早执行时间</dt><dd>{dateTime(detail.run_after)}</dd></div><div className="row"><dt>更新时间</dt><dd>{dateTime(detail.updated_at)}</dd></div></dl><h3>最近错误</h3><p className="mono">{detail.last_error || '暂无错误记录'}</p><h3>任务载荷</h3><div className="table-wrap"><pre className="mono">{payloadText(detail.payload)}</pre></div></div></Modal>}
  </div>
}
