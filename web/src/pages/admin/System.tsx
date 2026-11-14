import { useEffect, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { ArrowUpCircle, Check, ExternalLink, LoaderCircle, RefreshCw, RotateCcw } from 'lucide-react'
import { api } from '@/lib/api'
import { dateTime } from '@/lib/format'
import type { PublicConfig, ReleaseInfo, UpdateJob, UpdateStatus } from '@/lib/types'
import { Badge, Button, Card, Modal, Notice, PageHeader, QueryState } from '@/components/ui'
import { MutationError } from './shared'

const STATUS_KEY = ['admin', 'system', 'update'] as const
const RESTART_TIMEOUT_MS = 180_000

const supervisorLabels: Record<string, string> = { docker: 'Docker（容器重启策略）', systemd: 'systemd 服务' }
const stageLabels: Record<UpdateJob['stage'], string> = { downloading: '正在下载新版本', verifying: '正在校验新版本', installing: '正在安装', restarting: '正在重启服务', failed: '更新失败' }

const active = (job: UpdateJob | null | undefined) => !!job && job.stage !== 'failed'
const megabytes = (bytes: number) => `${(bytes / 1_048_576).toFixed(1)} MB`

function useNow(interval: number) {
  const [now, setNow] = useState(Date.now())
  useEffect(() => { const timer = window.setInterval(() => setNow(Date.now()), interval); return () => clearInterval(timer) }, [interval])
  return now
}

function ReleaseNotes({ release }: { release: ReleaseInfo }) {
  return <div className="release">
    <div className="release-head"><strong className="mono">{release.version}</strong><span className="muted small">{dateTime(release.published_at)}</span>{release.url && <a className="text-link" href={release.url} target="_blank" rel="noreferrer">发布页 <ExternalLink size={12} /></a>}</div>
    {/* Plain text on purpose: release notes are rendered as written, never as HTML. */}
    <div className="release-notes">{release.notes.trim() || '本版本没有填写更新说明。'}</div>
  </div>
}

// Waiting tracks the version we expect the restarted server to report.
type Waiting = { version: string; since: number; kind: 'update' | 'rollback' }

export function SystemUpdate() {
  const client = useQueryClient()
  const [waiting, setWaiting] = useState<Waiting | null>(null)
  const [confirm, setConfirm] = useState<'update' | 'rollback' | null>(null)
  const now = useNow(1000)
  const status = useQuery({
    queryKey: STATUS_KEY,
    queryFn: () => api.get<UpdateStatus>('/admin/system/update'),
    // Follow a running job closely; while the server restarts the request fails, keep the last answer.
    refetchInterval: (query) => waiting || active(query.state.data?.job) ? 1000 : 60_000,
    retry: waiting ? false : 1,
  })
  const data = status.data
  const job = data?.job

  // Watch the public version while restarting: the new process answers with the version it runs.
  const watch = useQuery({ queryKey: ['system', 'version-watch', waiting?.version], queryFn: () => api.get<PublicConfig>('/public/config'), enabled: !!waiting, refetchInterval: 1500, retry: false, gcTime: 0 })
  useEffect(() => {
    if (!waiting || watch.data?.version !== waiting.version) return
    toast.success(waiting.kind === 'update' ? `已更新到 ${waiting.version}` : `已回滚到 ${waiting.version}`, { description: '正在刷新页面以加载新版本…' })
    const timer = window.setTimeout(() => window.location.reload(), 900)
    return () => clearTimeout(timer)
  }, [watch.data?.version, waiting])
  // A job that fails before restarting ends the wait.
  useEffect(() => { if (waiting && job?.stage === 'failed') setWaiting(null) }, [job?.stage, waiting])
  // Resume the wait after a page reload while a job is running.
  useEffect(() => { if (!waiting && job && active(job)) setWaiting({ version: job.version, since: Date.parse(job.started_at) || Date.now(), kind: job.kind }) }, [job, waiting])

  const refresh = useMutation({
    mutationFn: () => api.get<UpdateStatus>('/admin/system/update?refresh=1'),
    onSuccess: (next) => { client.setQueryData(STATUS_KEY, next); toast.success(next.update_available ? `发现新版本 ${next.latest?.version}` : next.check_error ? '检查失败，请稍后重试' : '已是最新版本') },
  })
  const apply = useMutation({
    mutationFn: (version: string) => api.post<UpdateJob>('/admin/system/update', { version }),
    onSuccess: (started) => { setConfirm(null); setWaiting({ version: started.version, since: Date.now(), kind: 'update' }); void status.refetch() },
  })
  const rollback = useMutation({
    mutationFn: () => api.post<UpdateJob>('/admin/system/rollback', {}),
    onSuccess: (started) => { setConfirm(null); setWaiting({ version: started.version, since: Date.now(), kind: 'rollback' }); void status.refetch() },
  })

  const restarting = !!waiting && (!job || job.stage === 'restarting')
  const timedOut = !!waiting && now - waiting.since > RESTART_TIMEOUT_MS
  const busy = !!waiting || active(job)

  return <div className="stack">
    <PageHeader eyebrow="版本与发布" title="系统更新" description="检查新版本，在后台一键更新或回滚。更新会下载 GitHub 上发布的程序、校验后替换并平滑重启。" actions={<Button variant="secondary" disabled={refresh.isPending || busy} onClick={() => refresh.mutate()}><RefreshCw size={14} className={refresh.isPending ? 'spin' : undefined} />检查更新</Button>} />
    {!data ? <QueryState query={status}>{null}</QueryState> : <>
      {waiting && <Card className="update-progress">
        <div className="row"><LoaderCircle size={18} className="spin" /><strong>{restarting ? (waiting.kind === 'update' ? `正在重启到 ${waiting.version}` : `正在回滚到 ${waiting.version}`) : job ? stageLabels[job.stage] : '准备中'}</strong></div>
        {job && job.stage === 'downloading' && <div className="update-bar"><span style={{ width: job.total > 0 ? `${Math.min(100, (job.done / job.total) * 100)}%` : '30%' }} className={job.total > 0 ? undefined : 'indeterminate'} /></div>}
        {job && job.stage === 'downloading' && <p className="muted small">{job.total > 0 ? `${megabytes(job.done)} / ${megabytes(job.total)}` : megabytes(job.done)}</p>}
        {restarting && <p className="muted small">服务正在重启：先等待进行中的请求结束（最多 {data.drain_seconds} 秒），再启动新版本。完成后页面会自动刷新。</p>}
        {timedOut && <Notice>重启时间比预期长。新版本若无法正常启动，会在几次尝试后自动回滚到原版本。<Button variant="secondary" onClick={() => window.location.reload()}>刷新页面</Button></Notice>}
      </Card>}
      {job?.stage === 'failed' && !waiting && <Notice danger>{job.kind === 'update' ? `更新到 ${job.version} 失败` : `回滚失败`}：{job.error || '未知错误'}。当前版本未受影响，可以稍后重试。</Notice>}
      {data.last_failure && <Notice danger>上次更新到 {data.last_failure.version} 未能成功：{data.last_failure.error}（{dateTime(data.last_failure.at)}）。</Notice>}
      {data.pending && !waiting && <Notice success>新版本 {data.current} 刚刚启动，正在确认运行正常（约 30 秒），确认前不能再次更新。</Notice>}

      <div className="split-grid">
        <Card>
          <div className="section-heading"><h2>当前版本</h2>{data.update_available ? <Badge status="pending">有新版本</Badge> : data.latest ? <Badge status="active">已是最新</Badge> : null}</div>
          <div className="version-current mono">{data.current}</div>
          <dl className="version-meta">
            <div><dt>提交</dt><dd className="mono">{data.commit ? data.commit.slice(0, 12) : '—'}</dd></div>
            <div><dt>构建时间</dt><dd>{data.build_time ? dateTime(data.build_time) : '—'}</dd></div>
            <div><dt>运行方式</dt><dd>{supervisorLabels[data.supervisor] ?? '手动运行'}</dd></div>
            <div><dt>上次检查</dt><dd>{data.checked_at ? dateTime(data.checked_at) : '尚未检查'}</dd></div>
          </dl>
          {data.rollback && <div className="rollback-row"><span className="muted small">可回滚到 <span className="mono">{data.rollback.version}</span>（{data.rollback.source === 'image' ? '部署时的版本' : '上一次在线更新的版本'}）</span><Button variant="ghost" disabled={busy} onClick={() => { rollback.reset(); setConfirm('rollback') }}><RotateCcw size={14} />回滚</Button></div>}
        </Card>
        <Card>
          <div className="section-heading"><h2>最新版本</h2>{data.latest && <span className="mono muted small">{data.latest.version}</span>}</div>
          {data.check_error && <Notice>{data.check_error}</Notice>}
          {!data.latest && !data.check_error && <p className="muted">还没有找到已发布的版本。</p>}
          {data.latest && !data.update_available && <div className="version-uptodate"><Check size={18} /><span>当前运行的就是最新版本。</span></div>}
          {data.update_available && data.latest && <>
            <p className="muted small">从 {data.current} 更新到 {data.latest.version}，共 {data.changelog.length} 个新版本。</p>
            {!data.can_update && data.reason && <Notice>不能在线更新：{data.reason}</Notice>}
            <Button disabled={!data.can_update || busy || data.pending} onClick={() => { apply.reset(); setConfirm('update') }}><ArrowUpCircle size={15} />更新到 {data.latest.version}</Button>
          </>}
          {!data.update_available && !data.can_update && data.reason && <p className="muted small">{data.reason}</p>}
        </Card>
      </div>

      {data.changelog.length > 0 && <Card>
        <div className="section-heading"><h2>更新内容</h2><span className="muted small">{data.current} 之后发布的版本</span></div>
        <div className="release-list">{data.changelog.map((release) => <ReleaseNotes key={release.version} release={release} />)}</div>
      </Card>}
    </>}

    {confirm === 'update' && data?.latest && <Modal open title={`更新到 ${data.latest.version}`} onClose={() => { if (!apply.isPending) setConfirm(null) }} footer={<><MutationError error={apply.error} /><Button variant="secondary" disabled={apply.isPending} onClick={() => setConfirm(null)}>取消</Button><Button disabled={apply.isPending} onClick={() => apply.mutate(data.latest!.version)}>{apply.isPending ? '正在开始…' : '开始更新'}</Button></>}>
      <div className="stack">
        <p>将从 GitHub 下载 <strong className="mono">{data.latest.version}</strong>，核对 SHA-256 校验值后替换程序并重启服务。</p>
        <ul className="confirm-list">
          <li>重启前会停止接收新请求，等待进行中的请求结束，最多 {data.drain_seconds} 秒；之后再用几秒启动新版本。这段时间内的新请求会失败。</li>
          <li>超过等待时间仍未结束的长连接（如流式输出）会被断开。</li>
          <li>新版本若连续 3 次没能正常启动，会自动回滚到 {data.current}。</li>
          <li>数据库不会被回退，账号、订单等数据保持不变。</li>
        </ul>
        <Notice>建议在访问量低的时候更新。</Notice>
      </div>
    </Modal>}
    {confirm === 'rollback' && data?.rollback && <Modal open title={`回滚到 ${data.rollback.version}`} onClose={() => { if (!rollback.isPending) setConfirm(null) }} footer={<><MutationError error={rollback.error} /><Button variant="secondary" disabled={rollback.isPending} onClick={() => setConfirm(null)}>取消</Button><Button variant="danger" disabled={rollback.isPending} onClick={() => rollback.mutate()}>{rollback.isPending ? '正在开始…' : '确认回滚'}</Button></>}>
      <div className="stack">
        <p>将恢复到 <strong className="mono">{data.rollback.version}</strong>（{data.rollback.source === 'image' ? '部署时的版本' : '上一次在线更新的版本'}）并重启服务，重启期间的新请求会失败。</p>
        <Notice>数据库保持不变。新版本若已经新增了表或字段，旧版本会直接忽略它们。</Notice>
      </div>
    </Modal>}
  </div>
}
