import { useMemo, useState } from 'react'
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { Button, Card, Field, FieldInput, Notice, PageHeader, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { useConfig } from '@/lib/session'
import type { NormalizedSettings, Settings as SiteSettings } from '@/lib/types'
import { MutationError, Textarea } from './shared'
import { normalizeSettings, numericSettings, selfcheckNumericSettings, validateSettings } from './settings-validation'
import { MailSettings } from './MailSettings'

export function Settings() {
  const query = useQuery({ queryKey: ['admin', 'settings'], queryFn: () => api.get<SiteSettings>('/admin/settings'), refetchOnWindowFocus: false })
  return <div className="stack"><PageHeader eyebrow="系统配置" title="站点与支付设置" description="更改基础权益、资源生命周期与支付配置。保存前校验全部字段和 JSON。" /><QueryState query={query}>{query.data && <SettingsEditor initial={query.data} />}</QueryState><MailSettings /></div>
}

// The editor works on the normalized document: initial.payment may be missing entirely
// (fresh install returning {}, or an older stored row), and nothing below may crash on it.
function SettingsEditor({ initial }: { initial: SiteSettings }) {
  const normalized = useMemo(() => normalizeSettings(initial), [initial])
  const [draft, setDraft] = useState<NormalizedSettings>(() => structuredClone(normalized))
  const [templateJSON, setTemplateJSON] = useState(() => JSON.stringify(normalized.hosted_vm_template, null, 2))
  const [typesJSON, setTypesJSON] = useState(() => JSON.stringify(normalized.payment.epay.types))
  const [validation, setValidation] = useState('')
  const [dirty, setDirty] = useState(false)
  const hasExistingKey = Boolean(normalized.payment.epay.key)
  const hasExistingPrivateKey = Boolean(normalized.payment.alipay_f2f.private_key)
  const config = useConfig()
  const notifyURL = `${(config.data?.base_url || window.location.origin).replace(/\/$/, '')}/api/payments/alipay_f2f/notify`
  const client = useQueryClient()
  const mutation = useMutation({
    mutationFn: (input: SiteSettings) => api.put<SiteSettings>('/admin/settings', input),
    onSuccess: async (saved) => {
      const normalized = normalizeSettings(saved)
      const masked: NormalizedSettings = { ...normalized, payment: { ...normalized.payment, epay: { ...normalized.payment.epay, key: normalized.payment.epay.key ? '***' : '' }, alipay_f2f: { ...normalized.payment.alipay_f2f, private_key: normalized.payment.alipay_f2f.private_key ? '***' : '' } } }
      setDraft(structuredClone(masked)); setTemplateJSON(JSON.stringify(masked.hosted_vm_template, null, 2)); setTypesJSON(JSON.stringify(masked.payment.epay.types)); setDirty(false); setValidation('')
      client.setQueryData(['admin', 'settings'], masked)
      toast.success('系统设置已保存')
      await client.invalidateQueries()
    },
  })
  function change<K extends keyof NormalizedSettings>(key: K, value: NormalizedSettings[K]) { setDirty(true); setDraft((current) => ({ ...current, [key]: value })) }
  function changeEpay<K extends keyof NormalizedSettings['payment']['epay']>(key: K, value: NormalizedSettings['payment']['epay'][K]) { setDirty(true); setDraft((current) => ({ ...current, payment: { ...current.payment, epay: { ...current.payment.epay, [key]: value } } })) }
  function changeAlipay<K extends keyof NormalizedSettings['payment']['alipay_f2f']>(key: K, value: NormalizedSettings['payment']['alipay_f2f'][K]) { setDirty(true); setDraft((current) => ({ ...current, payment: { ...current.payment, alipay_f2f: { ...current.payment.alipay_f2f, [key]: value } } })) }
  function changeManual<K extends keyof NormalizedSettings['payment']['manual']>(key: K, value: NormalizedSettings['payment']['manual'][K]) { setDirty(true); setDraft((current) => ({ ...current, payment: { ...current.payment, manual: { ...current.payment.manual, [key]: value } } })) }
  function formatJSON() {
    try {
      const validated = validateSettings(draft, templateJSON, typesJSON, hasExistingKey, hasExistingPrivateKey)
      setTemplateJSON(JSON.stringify(validated.hosted_vm_template, null, 2)); setTypesJSON(JSON.stringify(validated.payment.epay.types, null, 2)); setValidation(''); toast.success('全部设置与 JSON 校验通过')
    } catch (error) { setValidation(error instanceof Error ? error.message : 'JSON 校验失败') }
  }
  return <form className="stack" onSubmit={(event) => {
    event.preventDefault()
    try { const input = validateSettings(draft, templateJSON, typesJSON, hasExistingKey, hasExistingPrivateKey); setValidation(''); mutation.mutate(input) } catch (error) { setValidation(error instanceof Error ? error.message : '设置校验失败') }
  }}>
    <fieldset disabled={mutation.isPending} className="stack">
      <Card><h2>站点信息</h2><div className="form-grid"><Field label="站点名称"><FieldInput required maxLength={200} value={draft.site_name} onChange={(event) => change('site_name', event.target.value)} /></Field><label className="row"><input type="checkbox" checked={draft.registration_open} onChange={(event) => change('registration_open', event.target.checked)} />开放新用户注册</label></div></Card>
      <Card><h2>权益与资源回收</h2><div className="form-grid">{numericSettings.map((field) => <Field key={field.key} label={field.label} hint={field.hint}><FieldInput required type="number" min={0} step={1} value={Number.isNaN(draft[field.key]) ? '' : draft[field.key]} onChange={(event) => change(field.key, event.target.value === '' ? Number.NaN : Number(event.target.value))} /></Field>)}</div></Card>
      <Card><h2>槽位环境自检</h2><div className="stack"><div className="form-grid"><label className="row"><input type="checkbox" checked={draft.selfcheck_require_dedicated_proxy} onChange={(event) => change('selfcheck_require_dedicated_proxy', event.target.checked)} />要求独享 SOCKS5 出口</label><label className="row"><input type="checkbox" checked={draft.selfcheck_require_timezone_match} onChange={(event) => change('selfcheck_require_timezone_match', event.target.checked)} />要求时区与出口一致</label></div><div className="form-grid">{selfcheckNumericSettings.map((field) => <Field key={field.key} label={field.label} hint={field.hint}><FieldInput required type="number" min={field.min} max={field.max} step={1} value={Number.isNaN(draft[field.key]) ? '' : draft[field.key]} onChange={(event) => change(field.key, event.target.value === '' ? Number.NaN : Number(event.target.value))} /></Field>)}</div><Notice>请同时在 vm2api 将代理池 bind_limit 设为 1；门面会通过 proxy_exclusive 检查兜底，不会自动修改上游配置。</Notice></div></Card>
      <Card><div className="section-heading"><h2>托管 VM 模板</h2><Button type="button" variant="secondary" onClick={formatJSON}>校验并格式化 JSON</Button></div><Textarea label="创建模板（JSON 对象）" hint="保留上游所需模板字段；只接受有效 JSON 对象，不接受数组或 null。" rows={9} spellCheck={false} className="input mono" value={templateJSON} onChange={(event) => { setDirty(true); setTemplateJSON(event.target.value) }} /></Card>
      <Card><h2>收款方式</h2><fieldset className="row"><legend className="muted">同一时间只使用一种收款方式，切换后保留其他配置。</legend>{([{ id: 'epay', label: '易支付' }, { id: 'alipay_f2f', label: '支付宝当面付' }, { id: 'manual', label: '人工转账' }] as const).map((method) => <label className="row" key={method.id}><input type="radio" name="active-payment" value={method.id} checked={draft.payment.active === method.id} onChange={() => change('payment', { ...draft.payment, active: method.id })} />{method.label}</label>)}</fieldset></Card>
      <div className="split-grid">
        <Card><h2>易支付</h2><div className="stack"><Field label="支付网关地址"><FieldInput type="url" placeholder="https://pay.example.com/" value={draft.payment.epay.gateway_url} onChange={(event) => changeEpay('gateway_url', event.target.value)} /></Field><Field label="商户编号"><FieldInput autoComplete="off" value={draft.payment.epay.pid} onChange={(event) => changeEpay('pid', event.target.value)} /></Field><Field label="支付密钥" hint="显示 *** 代表密钥已配置。保留 *** 或留空不会覆盖已有密钥；填写新值才会更新。"><FieldInput type="password" autoComplete="new-password" value={draft.payment.epay.key} onChange={(event) => changeEpay('key', event.target.value)} /></Field><Textarea label="支付方式（JSON 字符串数组）" hint={'例如 ["alipay", "wxpay"]；开启在线支付时至少填写一种。'} spellCheck={false} rows={3} value={typesJSON} onChange={(event) => { setDirty(true); setTypesJSON(event.target.value) }} /></div></Card>
        <Card><h2>人工转账</h2><div className="stack"><Textarea label="转账说明" hint="向用户展示的收款方式、对账信息和联系渠道，请勿填写管理员密钥。" rows={8} value={draft.payment.manual.instructions} onChange={(event) => changeManual('instructions', event.target.value)} /><Notice>人工转账不会自动确认到账。请在订单管理中核实资金后，再点击「确认收款」。</Notice></div></Card>
        <Card><h2>支付宝当面付</h2><div className="stack">
          <Field label="AppID"><FieldInput autoComplete="off" inputMode="numeric" value={draft.payment.alipay_f2f.app_id} onChange={(event) => changeAlipay('app_id', event.target.value)} /></Field>
          <Textarea label="应用私钥" hint="显示 *** 表示已保存；保留 *** 或留空可保留原值。支持 PKCS1 / PKCS8 PEM 或不带头尾的 base64。" autoComplete="off" spellCheck={false} rows={5} value={draft.payment.alipay_f2f.private_key} onChange={(event) => changeAlipay('private_key', event.target.value)} />
          <Textarea label="支付宝公钥" hint="填写支付宝提供的公钥，支持 PEM 或 base64。" spellCheck={false} rows={5} value={draft.payment.alipay_f2f.alipay_public_key} onChange={(event) => changeAlipay('alipay_public_key', event.target.value)} />
          <label className="row"><input type="checkbox" role="switch" checked={draft.payment.alipay_f2f.sandbox} onChange={(event) => changeAlipay('sandbox', event.target.checked)} />沙箱环境</label>
          <p className="muted">在支付宝开放平台创建应用并开通「当面付」，接口加签方式选「公钥」，把应用公钥上传到支付宝、把支付宝公钥填到这里。异步通知地址会自动使用下方地址。</p>
          <Field label="异步通知地址"><FieldInput readOnly value={notifyURL} /></Field>
        </div></Card>
      </div>
    </fieldset>
    <MutationError error={validation ? new Error(validation) : mutation.error} />
    <Card><div className="toolbar"><span className="muted" role="status">{dirty ? '有未保存的更改' : mutation.isSuccess ? '已保存，配置已生效' : '编辑完成后点击保存，密钥不会明文回显。'}</span><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={formatJSON}>校验设置</Button><Button type="submit" disabled={mutation.isPending || !dirty}>{mutation.isPending ? '正在保存…' : '保存全部设置'}</Button></div></Card>
  </form>
}
