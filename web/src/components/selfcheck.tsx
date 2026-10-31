import { Check, Minus, X } from 'lucide-react'
import { Badge, Notice } from '@/components/ui'
import type { Selfcheck, SelfcheckCheck } from '@/lib/types'

export function SelfcheckBadge({ value, attempts = false }: { value: Selfcheck | null | undefined; attempts?: boolean }) {
  if (!value) return <Badge status="pending">未自检</Badge>
  return <span className="row"><Badge status={value.state}>{value.state === 'pending' ? '自检中' : undefined}</Badge>{attempts && <span className="muted small mono">{value.attempts} / {value.max_attempts} 次重建</span>}</span>
}

export function SelfcheckChecks({ checks, detail = false }: { checks: SelfcheckCheck[]; detail?: boolean }) {
  if (!checks.length) return <p className="muted small">检查项尚未生成，请稍候。</p>
  return <ul className="selfcheck-list">{checks.map((check) => <li key={check.code} className={check.ok ? 'selfcheck-ok' : check.skipped ? 'selfcheck-skipped' : 'selfcheck-failed'}>
    {check.ok ? <Check size={16} aria-label="通过" /> : check.skipped ? <Minus size={16} aria-label="已跳过" /> : <X size={16} aria-label="未通过" />}
    <div><div className="row"><strong>{check.message}</strong><code className="muted small">{check.code}</code></div>{check.remedy && <p className="small muted">{check.remedy}</p>}{detail && check.detail && <p className="small mono selfcheck-detail">{check.detail}</p>}</div>
  </li>)}</ul>
}

export function SelfcheckIssue({ value }: { value: Selfcheck | null | undefined }) {
  if (!value || !['fail', 'exhausted'].includes(value.state)) return null
  const duplicate = value.checks.some((check) => check.code === 'account_unique' && !check.ok)
  return <Notice danger={value.state === 'exhausted'}><strong>{value.state === 'exhausted' ? '自检多次未通过，已转人工处理' : duplicate ? '检测到重复托管账号' : '运行环境自检未通过'}</strong><p>{value.state === 'exhausted' ? '多次自检未通过，已转人工处理，请联系管理员。' : duplicate ? '该账号已在其它槽位托管，请勿重复添加。' : '系统正在重建运行环境，自检通过后才能继续授权。'}</p></Notice>
}
