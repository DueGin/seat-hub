import { useId, useState } from 'react'
import { useMutation, useQuery } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { Button, Card, Field, FieldInput, Notice, QueryState } from '@/components/ui'
import { MutationError, Select, Textarea } from './shared'

export interface MailConfig { enabled: boolean; host: string; port: number; security: 'starttls' | 'tls' | 'none'; username: string; password: string; from_address: string; from_name: string; admin_emails: string[] }

export function MailSettings() {
  const query = useQuery({ queryKey: ['admin', 'mail'], queryFn: () => api.get<MailConfig>('/admin/settings/mail') })
  return <Card><h2>邮件发送（SMTP）</h2><p className="muted">发送预约确认、开售通知和工单消息。</p><QueryState query={query}>{query.data && <MailForm key={query.dataUpdatedAt} initial={query.data} />}</QueryState></Card>
}

function MailForm({ initial }: { initial: MailConfig }) {
  const id = useId()
  const [form, setForm] = useState(initial)
  const [admins, setAdmins] = useState(initial.admin_emails.join('\n'))
  const [to, setTo] = useState('')
  const [dirty, setDirty] = useState(false)
  function update<K extends keyof MailConfig>(key: K, value: MailConfig[K]) { setDirty(true); setForm(current => ({ ...current, [key]: value })) }
  const save = useMutation({ mutationFn: () => api.put<MailConfig>('/admin/settings/mail', { ...form, admin_emails: admins.split(/[\s,，;；]+/).filter(Boolean) }), onSuccess: data => { setForm(data); setDirty(false); toast.success('邮件设置已保存') } })
  const test = useMutation({ mutationFn: () => api.post('/admin/settings/mail/test', { to }), onSuccess: () => toast.success('测试邮件已发送') })
  return <div className="stack">
    <form id={id} className="stack" onSubmit={event => { event.preventDefault(); save.mutate() }}>
      <label className="switch-field"><input type="checkbox" role="switch" checked={form.enabled} onChange={event => update('enabled', event.target.checked)} /><span><strong>启用邮件发送</strong><small>关闭后不会收集待发邮件。</small></span></label>
      <Notice>常用配置：QQ 邮箱 smtp.qq.com · 465 · TLS；Gmail smtp.gmail.com · 587 · STARTTLS。密码通常需填写邮箱服务提供的授权码。</Notice>
      <div className="form-grid">
        <Field label="SMTP 服务器"><FieldInput value={form.host} maxLength={253} placeholder="smtp.qq.com" required={form.enabled} onChange={event => update('host', event.target.value)} /></Field>
        <Field label="端口"><FieldInput type="number" min={1} max={65535} required value={form.port} onChange={event => update('port', Number(event.target.value))} /></Field>
        <Select label="加密方式" value={form.security} onChange={event => update('security', event.target.value as MailConfig['security'])}><option value="tls">TLS（连接时加密）</option><option value="starttls">STARTTLS</option><option value="none">不加密</option></Select>
        <Field label="登录账号"><FieldInput autoComplete="off" value={form.username} onChange={event => update('username', event.target.value)} /></Field>
        <Field label="密码 / 授权码" hint="显示 *** 表示已保存，保留它即可继续使用原密码。"><FieldInput type="password" autoComplete="new-password" value={form.password} onChange={event => update('password', event.target.value)} /></Field>
        <Field label="发件邮箱"><FieldInput type="email" required={form.enabled} value={form.from_address} onChange={event => update('from_address', event.target.value)} /></Field>
        <Field label="发件人名称"><FieldInput maxLength={200} value={form.from_name} onChange={event => update('from_name', event.target.value)} /></Field>
      </div>
      <Textarea label="管理员通知邮箱" rows={3} hint="每行一个邮箱，或用逗号分隔。留空时通知所有启用的管理员。" value={admins} onChange={event => { setAdmins(event.target.value); setDirty(true) }} />
      <MutationError error={save.error} /><div><Button type="submit" disabled={save.isPending}>{save.isPending ? '正在保存…' : '保存邮件设置'}</Button></div>
    </form>
    <form className="stack" onSubmit={event => { event.preventDefault(); test.mutate() }}>
      <Field label="测试收件邮箱" hint={dirty ? '配置有修改，请先保存再发送测试邮件。' : '测试使用已保存的配置。'}><FieldInput type="email" required value={to} onChange={event => setTo(event.target.value)} /></Field>
      <MutationError error={test.error} />{test.isSuccess && <Notice success>测试邮件已发送，请检查收件箱。</Notice>}
      <div><Button type="submit" variant="secondary" disabled={dirty || test.isPending || !form.enabled}>{test.isPending ? '正在发送…' : '发送测试邮件'}</Button></div>
    </form>
  </div>
}
