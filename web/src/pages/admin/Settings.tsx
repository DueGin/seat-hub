import { useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Button, Card, Field, FieldInput, Notice, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import type { Settings as SiteSettings } from '@/lib/types'
import { MutationError, Textarea } from './shared'
import { numericSettings, validateSettings } from './settings-validation'

export function Settings() {
  const query = useQuery({ queryKey: ['admin', 'settings'], queryFn: () => api.get<SiteSettings>('/admin/settings'), refetchOnWindowFocus: false })
  return <div className="stack"><PageHeader eyebrow="系统配置" title="站点与支付设置" description="更改基础权益、资源生命周期与支付配置。保存前校验全部字段和 JSON。" /><QueryState query={query}>{query.data && <SettingsEditor initial={query.data} />}</QueryState></div>
}

function SettingsEditor({ initial }: { initial: SiteSettings }) {
  const [draft, setDraft] = useState<SiteSettings>(() => structuredClone(initial))
  const [templateJSON, setTemplateJSON] = useState(() => JSON.stringify(initial.hosted_vm_template, null, 2))
  const [typesJSON, setTypesJSON] = useState(() => JSON.stringify(initial.payment.epay.types))
  const [validation, setValidation] = useState('')
  const [dirty, setDirty] = useState(false)
  const client = useQueryClient()
  const mutation = useMutation({
    mutationFn: (input: SiteSettings) => api.put<SiteSettings>('/admin/settings', input),
    onSuccess: async (saved) => {
      const masked: SiteSettings = { ...saved, payment: { ...saved.payment, epay: { ...saved.payment.epay, key: saved.payment.epay.key ? '***' : '' } } }
      setDraft(structuredClone(masked)); setTemplateJSON(JSON.stringify(masked.hosted_vm_template, null, 2)); setTypesJSON(JSON.stringify(masked.payment.epay.types)); setDirty(false); setValidation('')
      client.setQueryData(['admin', 'settings'], masked)
      toast.success('系统设置已保存')
      await client.invalidateQueries()
    },
  })
  function change<K extends keyof SiteSettings>(key: K, value: SiteSettings[K]) { setDirty(true); setDraft((current) => ({ ...current, [key]: value })) }
  function changeEpay<K extends keyof SiteSettings['payment']['epay']>(key: K, value: SiteSettings['payment']['epay'][K]) { setDirty(true); setDraft((current) => ({ ...current, payment: { ...current.payment, epay: { ...current.payment.epay, [key]: value } } })) }
  function changeManual<K extends keyof SiteSettings['payment']['manual']>(key: K, value: SiteSettings['payment']['manual'][K]) { setDirty(true); setDraft((current) => ({ ...current, payment: { ...current.payment, manual: { ...current.payment.manual, [key]: value } } })) }
  function formatJSON() {
    try {
      const validated = validateSettings(draft, templateJSON, typesJSON, Boolean(initial.payment.epay.key))
      setTemplateJSON(JSON.stringify(validated.hosted_vm_template, null, 2)); setTypesJSON(JSON.stringify(validated.payment.epay.types, null, 2)); setValidation(''); toast.success('全部设置与 JSON 校验通过')
    } catch (error) { setValidation(error instanceof Error ? error.message : 'JSON 校验失败') }
  }
  return <form className="stack" onSubmit={(event) => {
    event.preventDefault()
    try { const input = validateSettings(draft, templateJSON, typesJSON, Boolean(initial.payment.epay.key)); setValidation(''); mutation.mutate(input) } catch (error) { setValidation(error instanceof Error ? error.message : '设置校验失败') }
  }}>
    <fieldset disabled={mutation.isPending} className="stack">
      <Card><h2>站点信息</h2><div className="form-grid"><Field label="站点名称"><FieldInput required maxLength={200} value={draft.site_name} onChange={(event) => change('site_name', event.target.value)} /></Field><label className="row"><input type="checkbox" checked={draft.registration_open} onChange={(event) => change('registration_open', event.target.checked)} />开放新用户注册</label></div></Card>
      <Card><h2>权益与资源回收</h2><div className="form-grid">{numericSettings.map((field) => <Field key={field.key} label={field.label} hint={field.hint}><FieldInput required type="number" min={0} step={1} value={Number.isNaN(draft[field.key]) ? '' : draft[field.key]} onChange={(event) => change(field.key, event.target.value === '' ? Number.NaN : Number(event.target.value))} /></Field>)}</div></Card>
      <Card><div className="section-heading"><h2>托管 VM 模板</h2><Button type="button" variant="secondary" onClick={formatJSON}>校验并格式化 JSON</Button></div><Textarea label="创建模板（JSON 对象）" hint="保留上游所需模板字段；只接受有效 JSON 对象，不接受数组或 null。" rows={9} spellCheck={false} className="input mono" value={templateJSON} onChange={(event) => { setDirty(true); setTemplateJSON(event.target.value) }} /></Card>
      <div className="split-grid">
        <Card><h2>在线支付</h2><div className="stack"><label className="row"><input type="checkbox" checked={draft.payment.epay.enabled} onChange={(event) => changeEpay('enabled', event.target.checked)} />启用在线支付（易支付）</label><Field label="支付网关地址"><FieldInput type="url" placeholder="https://pay.example.com/" value={draft.payment.epay.gateway_url} onChange={(event) => changeEpay('gateway_url', event.target.value)} /></Field><Field label="商户编号"><FieldInput autoComplete="off" value={draft.payment.epay.pid} onChange={(event) => changeEpay('pid', event.target.value)} /></Field><Field label="支付密钥" hint="显示 *** 代表密钥已配置。保留 *** 或留空不会覆盖已有密钥；填写新值才会更新。"><FieldInput type="password" autoComplete="new-password" value={draft.payment.epay.key} onChange={(event) => changeEpay('key', event.target.value)} /></Field><Textarea label="支付方式（JSON 字符串数组）" hint={'例如 ["alipay", "wxpay"]；开启在线支付时至少填写一种。'} spellCheck={false} rows={3} value={typesJSON} onChange={(event) => { setDirty(true); setTypesJSON(event.target.value) }} /></div></Card>
        <Card><h2>人工转账</h2><div className="stack"><label className="row"><input type="checkbox" checked={draft.payment.manual.enabled} onChange={(event) => changeManual('enabled', event.target.checked)} />启用人工转账</label><Textarea label="转账说明" hint="向用户展示的收款方式、对账信息和联系渠道，请勿填写管理员密钥。" rows={8} value={draft.payment.manual.instructions} onChange={(event) => changeManual('instructions', event.target.value)} /><Notice>人工转账不会自动确认到账。请在订单管理中核实资金后，再点击「确认收款」。</Notice></div></Card>
      </div>
    </fieldset>
    <MutationError error={validation ? new Error(validation) : mutation.error} />
    <Card><div className="toolbar"><span className="muted" role="status">{dirty ? '有未保存的更改' : mutation.isSuccess ? '已保存，配置已生效' : '编辑完成后点击保存，密钥不会明文回显。'}</span><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={formatJSON}>校验设置</Button><Button type="submit" disabled={mutation.isPending || !dirty}>{mutation.isPending ? '正在保存…' : '保存全部设置'}</Button></div></Card>
  </form>
}
