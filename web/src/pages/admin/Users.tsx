import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Link } from 'react-router-dom'
import { KeyRound, LogOut, Pencil, Plus, ShieldCheck, ShieldOff, Trash2, UserCheck, UserX } from 'lucide-react'
import { Badge, Button, Card, CopyButton, Drawer, EmptyState, Field, FieldInput, Modal, Notice, PageHeader, QueryState } from '@/components/ui'
import { KeyScopeSummary } from '@/components/ui/key-scope'
import { api } from '@/lib/api'
import { useSession } from '@/lib/session'
import { dateTime, kindLabel, money, number } from '@/lib/format'
import type { Account, AdminUser, ApiKey, Order, Page, ProductKind, Subscription, UserRecords, UserSession } from '@/lib/types'
import { adminPath, ConfirmAction, ListPagination, MutationError, Select, Textarea, useAdminFilters, useAdminMutation } from './shared'

interface UserDetail { user: AdminUser; subscriptions: Subscription[]; accounts: Account[]; keys: ApiKey[]; orders: Order[]; sessions: UserSession[]; records: UserRecords }
type Confirm = { type: 'status' | 'revoke' | 'delete'; user: AdminUser } | { type: 'key-status' | 'key-delete'; user: AdminUser; key: ApiKey }

const roleLabel = (role: string) => role === 'admin' ? '管理员' : '普通用户'
const statusLabel = (status: string) => status === 'active' ? '正常' : '已停用'
const NOTE_MAX = 500

// Short device description from a User-Agent, for the session list.
function device(ua: string) {
  if (!ua) return '未知设备'
  const os = /Windows/.test(ua) ? 'Windows' : /Mac OS X|Macintosh/.test(ua) ? 'macOS' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS' : /Linux/.test(ua) ? 'Linux' : ''
  const browser = /Edg\//.test(ua) ? 'Edge' : /Chrome\//.test(ua) ? 'Chrome' : /Firefox\//.test(ua) ? 'Firefox' : /Safari\//.test(ua) ? 'Safari' : /curl\//.test(ua) ? 'curl' : ''
  return [browser, os].filter(Boolean).join(' · ') || ua.slice(0, 40)
}

export function Users() {
  const { params, page, update, setPage } = useAdminFilters()
  const search = params.get('q') ?? ''
  const role = params.get('role') ?? ''
  const status = params.get('status') ?? ''
  const query = useQuery({ queryKey: ['admin', 'users', search, role, status, page], queryFn: () => api.get<Page<AdminUser>>(adminPath('/admin/users', { q: search, role, status, page, page_size: 20 })) })
  const [creating, setCreating] = useState(false)
  const [editing, setEditing] = useState<AdminUser | null>(null)
  const [detail, setDetail] = useState<AdminUser | null>(null)
  return <div className="stack">
    <PageHeader eyebrow="客户管理" title="用户管理" description="新增和编辑用户、重置密码、强制下线，查看订阅、资源与密钥，赠送权益。" actions={<Button onClick={() => setCreating(true)}><Plus size={15} />新增用户</Button>} />
    <Card>
      <form key={`${search}|${role}|${status}`} className="toolbar filter-toolbar" onSubmit={(event) => { event.preventDefault(); update({ q: String(new FormData(event.currentTarget).get('q') ?? '').trim(), role, status }) }}>
        <Field label="搜索"><FieldInput type="search" name="q" defaultValue={search} placeholder="邮箱、备注或 #用户ID" /></Field>
        <Select label="角色" value={role} onChange={(event) => update({ q: search, role: event.target.value, status })}><option value="">全部角色</option><option value="user">普通用户</option><option value="admin">管理员</option></Select>
        <Select label="状态" value={status} onChange={(event) => update({ q: search, role, status: event.target.value })}><option value="">全部状态</option><option value="active">正常</option><option value="disabled">已停用</option></Select>
        <Button type="submit" variant="secondary">搜索</Button>
        {(search || role || status) && <Button type="button" variant="ghost" onClick={() => update({ q: '', role: '', status: '' })}>清除筛选</Button>}
      </form>
      <QueryState query={query}>{query.data?.items.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>用户</th><th>角色 / 状态</th><th>并发</th><th>权益使用</th><th>最近登录</th><th>注册时间</th><th>操作</th></tr></thead><tbody>
        {query.data.items.map((user) => <tr key={user.id}>
          <td><button type="button" className="link-button" onClick={() => setDetail(user)}><strong>{user.email}</strong></button><p className="muted mono">#{user.id}</p>{user.note && <p className="user-note-cell" title={user.note}>{user.note}</p>}</td>
          <td><Badge>{roleLabel(user.role)}</Badge><p><Badge status={user.status}>{statusLabel(user.status)}</Badge></p></td>
          <td className="mono">{number(user.concurrency_total)}</td>
          <td>托管 {user.hosted_used} / {user.hosted_total}<p className="muted">租赁 {user.rental_used} / {user.rental_total}</p></td>
          <td>{user.last_login_at ? dateTime(user.last_login_at) : <span className="muted">从未登录</span>}{user.last_login_ip && <p className="muted mono small">{user.last_login_ip}</p>}</td>
          <td>{dateTime(user.created_at)}</td>
          <td><div className="row"><Button variant="secondary" onClick={() => setDetail(user)}>详情</Button><Button variant="ghost" onClick={() => setEditing(user)}><Pencil size={13} />编辑</Button></div></td>
        </tr>)}
      </tbody></table></div> : <EmptyState title={search || role || status ? '没有符合条件的用户' : '还没有用户'} description={search || role || status ? '换个关键词或清除筛选条件试试。' : '点击右上角「新增用户」创建第一个用户。'} />}</QueryState>
      {!query.isError && <ListPagination data={query.data} onChange={setPage} />}
    </Card>
    {creating && <CreateUser onClose={() => setCreating(false)} onCreated={(user) => setDetail(user)} />}
    {editing && <EditUser user={editing} onClose={() => setEditing(null)} />}
    {detail && <UserDetails user={detail} onClose={() => setDetail(null)} onEdit={(user) => setEditing(user)} />}
  </div>
}

// PasswordField lets the admin generate a password (default) or type one.
function PasswordField({ mode, onMode, value, onValue }: { mode: 'generate' | 'manual'; onMode: (mode: 'generate' | 'manual') => void; value: string; onValue: (value: string) => void }) {
  return <div className="field"><span>登录密码</span>
    <div className="segmented segmented-2" role="radiogroup" aria-label="密码设置方式">
      {(['generate', 'manual'] as const).map((item) => <label key={item} className={mode === item ? 'active' : ''}><input type="radio" name="password-mode" checked={mode === item} onChange={() => onMode(item)} /><strong>{item === 'generate' ? '自动生成' : '手动设置'}</strong><small>{item === 'generate' ? '生成 16 位随机密码，只显示一次' : '由你指定，8–128 位'}</small></label>)}
    </div>
    {mode === 'manual' && <FieldInput type="text" autoComplete="new-password" minLength={8} maxLength={128} placeholder="至少 8 位" value={value} onChange={(event) => onValue(event.target.value)} />}
  </div>
}

// ShownPassword displays a generated password exactly once.
function ShownPassword({ email, password }: { email: string; password: string }) {
  return <div className="stack">
    <Notice success>请立即把密码交给用户。关闭此窗口后无法再次查看，只能重新重置。</Notice>
    <dl className="credential-box"><div><dt>登录邮箱</dt><dd className="mono">{email}</dd></div><div><dt>密码</dt><dd className="mono">{password}</dd></div></dl>
    <div className="row"><CopyButton value={password} label="复制密码" /><CopyButton value={`登录邮箱：${email}\n密码：${password}`} label="复制邮箱和密码" /></div>
  </div>
}

function CreateUser({ onClose, onCreated }: { onClose: () => void; onCreated: (user: AdminUser) => void }) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState<'user' | 'admin'>('user')
  const [mode, setMode] = useState<'generate' | 'manual'>('generate')
  const [password, setPassword] = useState('')
  const [note, setNote] = useState('')
  const [validation, setValidation] = useState('')
  const [created, setCreated] = useState<{ user: AdminUser; password?: string } | null>(null)
  const mutation = useAdminMutation((body: Record<string, string>) => api.post<{ user: AdminUser; password?: string }>('/admin/users', body), '用户已创建')
  if (created) {
    return <Modal open title="用户已创建" onClose={onClose} footer={<><Button variant="secondary" onClick={() => { onClose(); onCreated(created.user) }}>查看用户详情</Button><Button onClick={onClose}>完成</Button></>}>
      {created.password ? <ShownPassword email={created.user.email} password={created.password} /> : <Notice success>{created.user.email} 已创建，可以用你设置的密码登录。</Notice>}
    </Modal>
  }
  return <Modal open title="新增用户" onClose={() => { if (!mutation.isPending) onClose() }} footer={<><MutationError error={validation ? new Error(validation) : mutation.error} /><Button variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" form="create-user" disabled={mutation.isPending}>{mutation.isPending ? '正在创建…' : '创建用户'}</Button></>}>
    <form id="create-user" className="stack" noValidate onSubmit={(event) => {
      event.preventDefault()
      const trimmed = email.trim()
      if (!/^[^\s@]+@[^\s@]+$/.test(trimmed)) { setValidation('请输入有效邮箱。'); return }
      if (mode === 'manual' && (password.length < 8 || password.length > 128)) { setValidation('密码长度须为 8–128 位。'); return }
      setValidation('')
      mutation.mutate({ email: trimmed, role, note: note.trim(), ...(mode === 'manual' ? { password } : {}) }, { onSuccess: (data) => setCreated(data) })
    }}>
      <Field label="登录邮箱"><FieldInput type="email" required autoFocus autoComplete="off" placeholder="user@example.com" value={email} onChange={(event) => setEmail(event.target.value)} /></Field>
      <PasswordField mode={mode} onMode={setMode} value={password} onValue={setPassword} />
      <Select label="角色" value={role} onChange={(event) => setRole(event.target.value as 'user' | 'admin')}><option value="user">普通用户</option><option value="admin">管理员（可进入管理后台）</option></Select>
      <Textarea label="备注（仅管理员可见）" hint={`${note.length} / ${NOTE_MAX}`} rows={2} maxLength={NOTE_MAX} placeholder="例如：来源渠道、联系方式" value={note} onChange={(event) => setNote(event.target.value)} />
    </form>
  </Modal>
}

function EditUser({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const session = useSession()
  const self = session.data?.id === user.id
  const [email, setEmail] = useState(user.email)
  const [role, setRole] = useState(user.role)
  const [status, setStatus] = useState(user.status)
  const [note, setNote] = useState(user.note ?? '')
  const [validation, setValidation] = useState('')
  const mutation = useAdminMutation((body: Record<string, string>) => api.patch(`/admin/users/${user.id}`, body), '用户信息已保存', onClose)
  const changes: Record<string, string> = {}
  if (email.trim().toLowerCase() !== user.email) changes.email = email.trim()
  if (role !== user.role) changes.role = role
  if (status !== user.status) changes.status = status
  if (note.trim() !== (user.note ?? '')) changes.note = note.trim()
  const changed = Object.keys(changes).length > 0
  return <Modal open title={`编辑用户 · ${user.email}`} onClose={() => { if (!mutation.isPending) onClose() }} footer={<><MutationError error={validation ? new Error(validation) : mutation.error} /><Button variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" form="edit-user" disabled={mutation.isPending || !changed}>{mutation.isPending ? '正在保存…' : '保存修改'}</Button></>}>
    <form id="edit-user" className="stack" noValidate onSubmit={(event) => {
      event.preventDefault()
      if (!/^[^\s@]+@[^\s@]+$/.test(email.trim())) { setValidation('请输入有效邮箱。'); return }
      setValidation(''); mutation.mutate(changes)
    }}>
      <Field label="登录邮箱" hint="修改后用户需用新邮箱登录。"><FieldInput type="email" required value={email} onChange={(event) => setEmail(event.target.value)} /></Field>
      <div className="form-grid">
        <Select label="角色" value={role} disabled={self} onChange={(event) => setRole(event.target.value as AdminUser['role'])}><option value="user">普通用户</option><option value="admin">管理员</option></Select>
        <Select label="状态" value={status} disabled={self} onChange={(event) => setStatus(event.target.value as AdminUser['status'])}><option value="active">正常</option><option value="disabled">已停用</option></Select>
      </div>
      {self && <p className="muted small">不能修改自己的角色和状态，请让其他管理员操作。</p>}
      {status === 'disabled' && user.status === 'active' && <Notice>停用后用户立即无法登录和调用 API，其所有 API 密钥会在上游同步停用；重新启用时自动恢复。</Notice>}
      <Textarea label="备注（仅管理员可见）" hint={`${note.length} / ${NOTE_MAX}`} rows={3} maxLength={NOTE_MAX} value={note} onChange={(event) => setNote(event.target.value)} />
    </form>
  </Modal>
}

function ResetPassword({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const [mode, setMode] = useState<'generate' | 'manual'>('generate')
  const [password, setPassword] = useState('')
  const [validation, setValidation] = useState('')
  const [shown, setShown] = useState<string | null>(null)
  const mutation = useAdminMutation((body: Record<string, string>) => api.post<{ password?: string }>(`/admin/users/${user.id}/password`, body), '密码已重置')
  if (shown !== null) {
    return <Modal open title="密码已重置" onClose={onClose} footer={<Button onClick={onClose}>完成</Button>}>
      {shown ? <ShownPassword email={user.email} password={shown} /> : <Notice success>已改为你设置的密码，用户所有登录状态已失效。</Notice>}
    </Modal>
  }
  return <Modal open title={`重置密码 · ${user.email}`} onClose={() => { if (!mutation.isPending) onClose() }} footer={<><MutationError error={validation ? new Error(validation) : mutation.error} /><Button variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" form="reset-password" disabled={mutation.isPending}>{mutation.isPending ? '正在重置…' : '重置密码'}</Button></>}>
    <form id="reset-password" className="stack" noValidate onSubmit={(event) => {
      event.preventDefault()
      if (mode === 'manual' && (password.length < 8 || password.length > 128)) { setValidation('密码长度须为 8–128 位。'); return }
      setValidation('')
      mutation.mutate(mode === 'manual' ? { password } : {}, { onSuccess: (data) => setShown(data.password ?? '') })
    }}>
      <PasswordField mode={mode} onMode={setMode} value={password} onValue={setPassword} />
      <Notice>重置后旧密码立即失效，用户在所有设备上的登录也会被强制退出。API 密钥不受影响。</Notice>
    </form>
  </Modal>
}

function confirmTitle(action: Confirm) {
  switch (action.type) {
    case 'status': return action.user.status === 'active' ? '停用用户' : '启用用户'
    case 'revoke': return '强制下线'
    case 'delete': return '删除用户'
    case 'key-status': return action.key.status === 'active' ? '停用 API 密钥' : '启用 API 密钥'
    case 'key-delete': return '删除 API 密钥'
  }
}

function confirmDescription(action: Confirm) {
  switch (action.type) {
    case 'status': return action.user.status === 'active' ? `停用 ${action.user.email} 后，用户立即无法登录和调用 API，所有 API 密钥会同步停用。可随时重新启用。` : `恢复 ${action.user.email} 的登录和 API 访问，之前停用的密钥会自动恢复。`
    case 'revoke': return `${action.user.email} 在所有设备上的登录都会失效，需要重新登录。API 密钥不受影响。`
    case 'delete': return `将永久删除 ${action.user.email} 及其 API 密钥，邮箱可被重新注册。此操作不可撤销。`
    case 'key-status': return `${action.key.status === 'active' ? '停用后该密钥的请求会立即被拒绝。' : !action.key.scope.usable ? '此密钥当前无可用账号，启用后仍需用户重新选择账号。' : '启用后该密钥可以重新调用。'}（${action.key.name} · ${action.key.prefix}…）`
    case 'key-delete': return `删除后该密钥立即失效且无法恢复。（${action.key.name} · ${action.key.prefix}…）`
  }
}

function UserDetails({ user, onClose, onEdit }: { user: AdminUser; onClose: () => void; onEdit: (user: AdminUser) => void }) {
  const session = useSession()
  const query = useQuery({ queryKey: ['admin', 'user', user.id], queryFn: () => api.get<UserDetail>(`/admin/users/${user.id}`) })
  const [grant, setGrant] = useState(false)
  const [reset, setReset] = useState(false)
  const [subscription, setSubscription] = useState<Subscription | null>(null)
  const [confirm, setConfirm] = useState<Confirm | null>(null)
  const [phrase, setPhrase] = useState('')
  const data = query.data
  const current = data?.user ?? user
  const self = session.data?.id === user.id
  const blocked = data ? data.records.orders + data.records.subscriptions + data.records.resources + (data.records.tickets ?? 0) > 0 : true
  const mutation = useAdminMutation(async (action: Confirm) => {
    switch (action.type) {
      case 'status': return api.patch(`/admin/users/${action.user.id}`, { status: action.user.status === 'active' ? 'disabled' : 'active' })
      case 'revoke': return api.post(`/admin/users/${action.user.id}/sessions/revoke`, {})
      case 'delete': return api.delete(`/admin/users/${action.user.id}`)
      case 'key-status': return api.patch(`/admin/users/${action.user.id}/keys/${action.key.id}`, { status: action.key.status === 'active' ? 'disabled' : 'active' })
      case 'key-delete': return api.delete(`/admin/users/${action.user.id}/keys/${action.key.id}`)
    }
  }, '操作已完成', () => { if (confirm?.type === 'delete') onClose(); setConfirm(null); setPhrase('') })
  const open = (action: Confirm) => { mutation.reset(); setPhrase(''); setConfirm(action) }

  return <>
    <Drawer open title="用户详情" onClose={onClose}>
      <QueryState query={query}>{data && <div className="stack">
        <div className="user-head">
          <span className="user-avatar">{current.email.charAt(0).toUpperCase()}</span>
          <div><h2>{current.email}</h2><p className="muted small mono">#{current.id} · 注册于 {dateTime(current.created_at)}</p></div>
          <div className="row"><Badge>{roleLabel(current.role)}</Badge><Badge status={current.status}>{statusLabel(current.status)}</Badge></div>
        </div>
        {current.note && <div className="user-note">{current.note}</div>}
        <dl className="version-meta">
          <div><dt>最近登录</dt><dd>{current.last_login_at ? `${dateTime(current.last_login_at)}` : '从未登录'}{current.last_login_ip && <span className="mono muted"> · {current.last_login_ip}</span>}</dd></div>
          <div><dt>在线设备</dt><dd>{current.active_sessions} 台</dd></div>
          <div><dt>并发</dt><dd className="mono">{number(current.concurrency_total)}</dd></div>
          <div><dt>上游用户</dt><dd>{current.vm2api_username || current.vm2api_user_id ? <>
            {current.vm2api_username && <span className="mono block">{current.vm2api_username}</span>}
            {current.vm2api_user_id && <span className="mono muted block">{current.vm2api_user_id}</span>}
          </> : '尚未创建'}</dd></div>
        </dl>
        <div className="user-actions">
          <Button variant="secondary" onClick={() => onEdit(current)}><Pencil size={14} />编辑资料</Button>
          <Button variant="secondary" onClick={() => setGrant(true)}><Plus size={14} />赠送订阅</Button>
          <Button variant="secondary" disabled={self} title={self ? '不能操作自己' : undefined} onClick={() => setReset(true)}><KeyRound size={14} />重置密码</Button>
          <Button variant="secondary" disabled={self || current.active_sessions === 0} title={self ? '不能操作自己' : undefined} onClick={() => open({ type: 'revoke', user: current })}><LogOut size={14} />强制下线</Button>
          <Button variant={current.status === 'active' ? 'danger' : 'secondary'} disabled={self} title={self ? '不能操作自己' : undefined} onClick={() => open({ type: 'status', user: current })}>{current.status === 'active' ? <><UserX size={14} />停用</> : <><UserCheck size={14} />启用</>}</Button>
          <Button variant="danger" disabled={self || blocked} title={self ? '不能删除自己' : blocked ? '有订单、订阅、资源或工单记录的用户不能删除，请改为停用' : undefined} onClick={() => open({ type: 'delete', user: current })}><Trash2 size={14} />删除</Button>
        </div>
        {blocked && !self && <p className="muted small">该用户有 {data.records.orders} 笔订单、{data.records.subscriptions} 个订阅、{data.records.resources} 个账号资源{data.records.tickets ? `、${data.records.tickets} 条工单记录` : ''}，为保留账务和客服记录不能删除；不再使用请停用。</p>}

        <section><h3 className="detail-heading">订阅权益</h3>{data.subscriptions?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>订阅</th><th>状态</th><th>生效 / 到期</th><th /></tr></thead><tbody>{data.subscriptions.map((item) => <tr key={item.id}><td>#{item.id} · {kindLabel[item.kind]} × {item.quantity}<p className="muted mono">{item.source_order_no || '—'}</p></td><td><Badge status={item.status}>{({ active: '生效中', expired: '已过期', cancelled: '已取消' } as Record<string, string>)[item.status] ?? item.status}</Badge></td><td>{dateTime(item.starts_at)}<p className="muted">{dateTime(item.expires_at)}</p></td><td><Button variant="ghost" disabled={item.status === 'cancelled'} onClick={() => setSubscription(item)}>调整</Button></td></tr>)}</tbody></table></div> : <p className="muted">暂无订阅。</p>}</section>

        <section><h3 className="detail-heading">账号资源</h3>{data.accounts?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>账号</th><th>类型</th><th>状态</th><th>到期</th></tr></thead><tbody>{data.accounts.map((item) => <tr key={item.id}><td>{item.label || `资源 #${item.id}`}<p className="muted">{item.email_masked || '尚无账号信息'}</p></td><td>{kindLabel[item.kind]}</td><td><Badge status={item.status} /></td><td>{dateTime(item.expires_at)}</td></tr>)}</tbody></table></div> : <p className="muted">暂无资源。</p>}</section>

        <section><h3 className="detail-heading">API 密钥</h3>{data.keys?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>名称 / 前缀</th><th>状态</th><th>可调用的账号</th><th>最后使用</th><th /></tr></thead><tbody>{data.keys.map((key) => <tr key={key.id}><td>{key.name}<p className="mono muted">{key.prefix}…</p></td><td><Badge status={key.status === 'active' ? 'active' : 'disabled'}>{key.status === 'active' ? '已启用' : '已停用'}</Badge></td><td><KeyScopeSummary scope={key.scope} accounts={data.accounts} /></td><td>{dateTime(key.last_used_at)}</td><td><div className="row"><Button variant="ghost" onClick={() => open({ type: 'key-status', user: current, key })}>{key.status === 'active' ? <><ShieldOff size={13} />停用</> : <><ShieldCheck size={13} />启用</>}</Button><Button variant="ghost" onClick={() => open({ type: 'key-delete', user: current, key })}><Trash2 size={13} />删除</Button></div></td></tr>)}</tbody></table></div> : <p className="muted">暂无 API 密钥。</p>}</section>

        <section><h3 className="detail-heading">登录设备</h3>{data.sessions?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>设备</th><th>IP</th><th>登录时间</th><th>到期</th></tr></thead><tbody>{data.sessions.map((item, index) => <tr key={`${item.created_at}-${index}`}><td title={item.user_agent}>{device(item.user_agent)}</td><td className="mono">{item.ip || '—'}</td><td>{dateTime(item.created_at)}</td><td>{dateTime(item.expires_at)}</td></tr>)}</tbody></table></div> : <p className="muted">当前没有登录中的设备。</p>}</section>

        <section><div className="section-heading"><h3 className="detail-heading">订单记录</h3><Link className="text-link" to={`/admin/orders?user_id=${user.id}`} onClick={onClose}>查看全部订单 →</Link></div>{data.orders?.length ? <div className="table-wrap"><table className="data-table"><thead><tr><th>订单</th><th>金额</th><th>状态</th></tr></thead><tbody>{data.orders.slice(0, 10).map((item) => <tr key={item.order_no}><td><span className="mono">{item.order_no}</span><p className="muted">{item.product_name}</p></td><td>{money(item.amount_cents)}</td><td><Badge status={item.status} /></td></tr>)}</tbody></table></div> : <p className="muted">暂无订单。</p>}</section>
      </div>}</QueryState>
    </Drawer>
    {grant && <GrantEditor user={current} onClose={() => setGrant(false)} />}
    {reset && <ResetPassword user={current} onClose={() => setReset(false)} />}
    {subscription && <SubscriptionEditor subscription={subscription} onClose={() => setSubscription(null)} />}
    {confirm && <ConfirmAction
      title={confirmTitle(confirm)}
      description={confirmDescription(confirm)}
      danger={confirm.type !== 'revoke' && !(confirm.type === 'status' && confirm.user.status !== 'active') && !(confirm.type === 'key-status' && confirm.key.status !== 'active')}
      phrase={confirm.type === 'delete' ? confirm.user.email : undefined} confirmation={phrase} onConfirmation={setPhrase}
      label={confirm.type === 'delete' ? '永久删除' : '确认'}
      pending={mutation.isPending} error={mutation.error} onClose={() => setConfirm(null)} onConfirm={() => mutation.mutate(confirm)} />}
  </>
}

function GrantEditor({ user, onClose }: { user: AdminUser; onClose: () => void }) {
  const [kind, setKind] = useState<ProductKind>('concurrency')
  const [quantity, setQuantity] = useState('1')
  const [days, setDays] = useState('30')
  const [note, setNote] = useState('')
  const [error, setError] = useState('')
  const [confirmed, setConfirmed] = useState(false)
  const mutation = useAdminMutation((body: { kind: ProductKind; quantity: number; duration_days: number; note: string }) => api.post(`/admin/users/${user.id}/grant`, body), '赠送订阅已生效', onClose)
  return <Modal open title={`赠送订阅 · ${user.email}`} onClose={() => { if (!mutation.isPending) onClose() }} footer={<><MutationError error={error ? new Error(error) : mutation.error} /><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" form="grant-form" disabled={mutation.isPending || !confirmed}>{mutation.isPending ? '正在赠送…' : '确认赠送'}</Button></>}>
    <form id="grant-form" className="stack" onSubmit={(event) => {
      event.preventDefault()
      if (![Number(quantity), Number(days)].every((value) => Number.isSafeInteger(value) && value > 0) || !note.trim() || !confirmed) { setError('数量和有效天数必须为正整数，请填写赠送原因并确认。'); return }
      setError(''); mutation.mutate({ kind, quantity: Number(quantity), duration_days: Number(days), note: note.trim() })
    }}>
      <p className="muted">将为该用户生成一笔已履约的赠送订单。</p>
      <Select label="权益类型" value={kind} onChange={(event) => setKind(event.target.value as ProductKind)}><option value="concurrency">并发</option><option value="hosted_seat">托管席位</option><option value="rental_account">租赁账号</option></Select>
      <div className="form-grid"><Field label="权益数量"><FieldInput type="number" min={1} step={1} required value={quantity} onChange={(event) => setQuantity(event.target.value)} /></Field><Field label="有效天数"><FieldInput type="number" min={1} step={1} required value={days} onChange={(event) => setDays(event.target.value)} /></Field></div>
      <Textarea label="赠送原因" required rows={3} maxLength={1000} value={note} onChange={(event) => setNote(event.target.value)} />
      <label className="row"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />我已核对接收用户、权益数量和有效期</label>
    </form>
  </Modal>
}

function SubscriptionEditor({ subscription, onClose }: { subscription: Subscription; onClose: () => void }) {
  const [expires, setExpires] = useState(subscription.expires_at)
  const [cancel, setCancel] = useState(false)
  const [confirmed, setConfirmed] = useState(false)
  const [error, setError] = useState('')
  const mutation = useAdminMutation((body: { expires_at?: string; status?: 'cancelled' }) => api.patch(`/admin/subscriptions/${subscription.id}`, body), '订阅已更新', onClose)
  return <Modal open title={`调整订阅 #${subscription.id}`} onClose={() => { if (!mutation.isPending) onClose() }} footer={<><MutationError error={error ? new Error(error) : mutation.error} /><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>返回</Button><Button variant={cancel ? 'danger' : 'primary'} type="submit" form="subscription-form" disabled={mutation.isPending || !confirmed}>{mutation.isPending ? '正在提交…' : cancel ? '确认取消订阅' : '保存到期时间'}</Button></>}>
    <form id="subscription-form" className="stack" onSubmit={(event) => {
      event.preventDefault()
      if (!confirmed) return
      if (cancel) { mutation.mutate({ status: 'cancelled' }); return }
      const parsed = new Date(expires)
      if (!/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/.test(expires) || Number.isNaN(parsed.getTime())) { setError('请填写带时区的 RFC3339 时间，例如 2026-12-31T23:59:59Z。'); return }
      setError(''); mutation.mutate({ expires_at: parsed.toISOString() })
    }}>
      <Field label="到期时间（RFC3339）" hint={`当前本地时间：${dateTime(subscription.expires_at)}；提交时统一转为 UTC。`}><FieldInput required={!cancel} disabled={cancel} value={expires} onChange={(event) => setExpires(event.target.value)} /></Field>
      <label className="row"><input type="checkbox" checked={cancel} onChange={(event) => setCancel(event.target.checked)} />取消此订阅，而非调整到期时间</label>
      <label className="row"><input type="checkbox" checked={confirmed} onChange={(event) => setConfirmed(event.target.checked)} />确认立即更改用户权益，可能影响在用资源</label>
    </form>
  </Modal>
}
