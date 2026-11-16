import { useId, useState } from 'react'
import { useMutation } from '@tanstack/react-query'
import { api } from '@/lib/api'
import { useSession } from '@/lib/session'
import type { Product } from '@/lib/types'
import { Button, Field, FieldInput, Modal, Notice } from '@/components/ui'

export function ReserveDialog({ product, onClose }: { product: Product; onClose: () => void }) {
  const session = useSession()
  const id = useId()
  const [email, setEmail] = useState<string | null>(null)
  const [note, setNote] = useState('')
  const address = email ?? session.data?.email ?? ''
  const mutation = useMutation({ mutationFn: () => api.post(`/public/products/${product.id}/reservations`, { email: address.trim(), note: note.trim() }) })
  return <Modal open title={`预约 · ${product.name}`} onClose={onClose} footer={mutation.isSuccess ? <Button onClick={onClose}>完成</Button> : <><Button variant="secondary" onClick={onClose}>取消</Button><Button type="submit" form={id} disabled={mutation.isPending}>{mutation.isPending ? '正在预约…' : '确认预约'}</Button></>}>
    {mutation.isSuccess ? <Notice success>预约成功，开售时我们会发邮件通知你</Notice> : <form id={id} className="stack" onSubmit={event => { event.preventDefault(); mutation.mutate() }}>
      <p className="muted">该商品暂未开售。留下邮箱，第一时间收到开售通知。</p>
      <Field label="通知邮箱" hint="可使用与你的登录邮箱不同的地址。"><FieldInput type="email" autoComplete="email" maxLength={254} autoFocus required value={address} onChange={event => setEmail(event.target.value)} /></Field>
      <label className="field"><span>备注（选填）</span><textarea className="input" rows={3} maxLength={200} value={note} onChange={event => setNote(event.target.value)} /><small>最多 200 字</small></label>
      {mutation.error && <p role="alert" className="text-red">{mutation.error.message}</p>}
    </form>}
  </Modal>
}
