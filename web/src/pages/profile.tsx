import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { toast } from 'sonner'
import { api } from '@/lib/api'
import { useSession } from '@/lib/session'
import { dateTime } from '@/lib/format'
import { Button, Card, Field, FieldInput, Notice, PageHeader } from '@/components/ui'

export function ProfilePage() {
  const session = useSession()
  const client = useQueryClient()
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const mutation = useMutation({ mutationFn: () => api.post('/auth/password', { old_password: oldPassword, new_password: newPassword }), onSuccess: () => { setOldPassword(''); setNewPassword(''); setConfirmPassword(''); void client.invalidateQueries({ queryKey: ['session'] }); toast.success('密码已修改，其他设备会话已退出') } })
  return <><PageHeader title="账户安全" description="管理你的登录凭证，保持账号安全。" /><div className="split-grid"><Card className="stack"><h2>账号信息</h2><div><span className="muted small">登录邮箱</span><p>{session.data?.email}</p></div><div><span className="muted small">账号角色</span><p>{session.data?.role === 'admin' ? '管理员' : '普通用户'}</p></div><div><span className="muted small">注册时间</span><p>{dateTime(session.data?.created_at)}</p></div></Card><Card className="stack"><h2>修改登录密码</h2><Notice>修改密码后，其他设备的登录会话会被撤销。</Notice><form className="stack" onSubmit={(event) => { event.preventDefault(); if (newPassword === confirmPassword) mutation.mutate() }}><Field label="当前密码"><FieldInput type="password" autoComplete="current-password" required value={oldPassword} onChange={(event) => setOldPassword(event.target.value)} /></Field><Field label="新密码" hint="使用 8–128 位密码"><FieldInput type="password" autoComplete="new-password" required minLength={8} maxLength={128} value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></Field><Field label="再次输入新密码"><FieldInput type="password" autoComplete="new-password" required minLength={8} maxLength={128} value={confirmPassword} onChange={(event) => setConfirmPassword(event.target.value)} /></Field>{confirmPassword && confirmPassword !== newPassword && <p role="alert" className="small text-amber">两次输入的新密码不一致</p>}<Button disabled={mutation.isPending || !newPassword || newPassword !== confirmPassword}>{mutation.isPending ? '正在修改…' : '保存新密码'}</Button></form></Card></div></>
}
