import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useSearchParams } from 'react-router-dom'
import { toast } from 'sonner'
import type { ReactNode, SelectHTMLAttributes, TextareaHTMLAttributes } from 'react'
import { Button, Field, FieldInput, Modal, Notice, Pagination } from '@/components/ui'

export function useAdminMutation<T>(operation: (input: T) => Promise<unknown>, message: string, onSuccess?: () => void) {
  const client = useQueryClient()
  return useMutation({
    mutationFn: operation,
    onSuccess: async () => {
      toast.success(message)
      onSuccess?.()
      await client.invalidateQueries()
    },
  })
}

export function MutationError({ error }: { error: unknown }) {
  return error ? <div role="alert"><Notice>{error instanceof Error ? error.message : '操作未完成，请稍后重试。'}</Notice></div> : null
}

export function Select({ label, children, ...props }: SelectHTMLAttributes<HTMLSelectElement> & { label: string }) {
  return <label className="field"><span>{label}</span><select className="input" {...props}>{children}</select></label>
}

export function Textarea({ label, hint, ...props }: TextareaHTMLAttributes<HTMLTextAreaElement> & { label: string; hint?: string }) {
  return <label className="field"><span>{label}</span><textarea className="input" {...props} />{hint && <small>{hint}</small>}</label>
}

export function useAdminFilters() {
  const [params, setParams] = useSearchParams()
  const rawPage = Number(params.get('page') ?? 1)
  const page = Number.isSafeInteger(rawPage) && rawPage > 0 ? rawPage : 1
  function update(values: Record<string, string>, nextPage = 1) {
    const next = new URLSearchParams(params)
    Object.entries(values).forEach(([key, value]) => { if (value) next.set(key, value); else next.delete(key) })
    if (nextPage > 1) next.set('page', String(nextPage)); else next.delete('page')
    setParams(next)
  }
  return { params, page, update, setPage: (value: number) => update({}, value) }
}

export function adminPath(path: string, values: Record<string, string | number>) {
  const params = new URLSearchParams()
  Object.entries(values).forEach(([key, value]) => { if (value !== '') params.set(key, String(value)) })
  return `${path}?${params.toString()}`
}

export function ListPagination({ data, onChange }: { data?: { page: number; page_size: number; total: number }; onChange: (page: number) => void }) {
  return data ? <Pagination page={data.page} pageSize={data.page_size} total={data.total} onChange={onChange} /> : null
}

export function ConfirmAction({ title, description, children, pending, error, onClose, onConfirm, label = '确认操作', danger = false, phrase, confirmation, onConfirmation }: {
  title: string; description: ReactNode; children?: ReactNode; pending: boolean; error: unknown; onClose: () => void; onConfirm: () => void; label?: string; danger?: boolean; phrase?: string; confirmation?: string; onConfirmation?: (value: string) => void
}) {
  return <Modal open title={title} onClose={() => { if (!pending) onClose() }} footer={<><MutationError error={error} /><Button variant="secondary" disabled={pending} onClick={onClose}>返回</Button><Button variant={danger ? 'danger' : 'primary'} disabled={pending || Boolean(phrase && confirmation !== phrase)} onClick={onConfirm}>{pending ? '正在提交…' : label}</Button></>}>
    <div className="stack">
      <Notice>{description}</Notice>
      {children}
      {phrase && <Field label={`请输入「${phrase}」确认`}><FieldInput autoComplete="off" autoFocus value={confirmation ?? ''} onChange={(event) => onConfirmation?.(event.target.value)} /></Field>}
    </div>
  </Modal>
}

export const orderStatuses: Record<string, string> = { pending: '待支付', paid: '已支付', fulfilled: '已完成', fulfill_failed: '履约失败', cancelled: '已取消', expired: '已过期', refunded: '已退款' }
export const accountStatuses: Record<string, string> = { provisioning: '创建中', awaiting_auth: '等待授权', bootstrapping: '初始化中', active: '运行中', needs_reauth: '待重新授权', suspended: '已暂停', failed: '失败', releasing: '释放中', released: '已释放', allocating: '分配中' }
export const scheduleLabels: Record<string, string> = { on: '参与调度', off: '停止调度', unschedulable: '不可调度', restricted: '受限' }
export const credentialLabels: Record<string, string> = { active: '凭证有效', valid: '凭证有效', ok: '凭证有效', ready: '凭证就绪', healthy: '凭证健康', expired: '凭证已过期', invalid: '凭证无效', missing: '缺少凭证', error: '凭证异常', needs_reauth: '需重新授权' }
export const jobLabels: Record<string, string> = { provision: '资源创建', provision_hosted: '创建托管资源', hosted_provision: '创建托管资源', bootstrap: '初始化', release: '资源释放', release_hosted: '释放托管资源', release_rental: '释放租赁资源', fulfill: '订单履约', reconcile: '状态同步', cleanup: '资源清理' }
