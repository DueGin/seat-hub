import { useState } from 'react'
import { useMutation, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from 'react-router-dom'
import { api } from '@/lib/api'
import { useConfig, useSession } from '@/lib/session'
import type { Order, Payment, Product } from '@/lib/types'
import { money } from '@/lib/format'
import { Button, Field, FieldInput, Modal, Notice } from '@/components/ui'

export function PurchaseDialog({ product, onClose, subscriptionId, fixedQuantity }: { product: Product | null; onClose: () => void; subscriptionId?: number; fixedQuantity?: number }) {
  const [quantity, setQuantity] = useState(product?.min_qty ?? 1)
  const [provider, setProvider] = useState('')
  const session = useSession()
  const config = useConfig()
  const navigate = useNavigate()
  const client = useQueryClient()
  const providers = config.data?.payment_providers.filter((item) => item.id !== 'mock' || config.data.dev) ?? []
  const selectedProvider = providers.some((item) => item.id === provider) ? provider : providers[0]?.id ?? ''
  const count = fixedQuantity ?? quantity
  const create = useMutation({ mutationFn: () => api.post<{ order: Order; payment: Payment }>('/orders', { product_id: product?.id, quantity: count, provider: selectedProvider, ...(subscriptionId ? { renew_subscription_id: subscriptionId } : {}) }), onSuccess: (data) => { void client.invalidateQueries({ queryKey: ['orders'] }); onClose(); navigate(`/orders/${data.order.order_no}`, { state: { payment: data.payment } }) } })
  return <Modal open={!!product} onClose={onClose} title={subscriptionId ? '续费订阅' : '确认购买'}>{product && <form className="stack" onSubmit={(event) => { event.preventDefault(); if (!session.data) { navigate(`/login?returnTo=${encodeURIComponent('/#products')}`); return } create.mutate() }}><div><p className="eyebrow">{product.duration_days} 天有效</p><h3>{product.name}</h3><p className="muted">{product.description}</p></div><Field label="购买数量" hint={`每份包含 ${product.unit_quantity} 个权益单位，单次最多 ${product.max_qty} 份`}><FieldInput type="number" min={product.min_qty} max={product.max_qty} step={1} required disabled={!!fixedQuantity} value={count} onChange={(event) => setQuantity(Number(event.target.value))} /></Field><Field label="支付方式">{providers.length === 1 ? <p>{providers[0].name}</p> : <select className="input" aria-label="支付方式" value={selectedProvider} onChange={(event) => setProvider(event.target.value)}>{providers.map((item) => <option key={item.id} value={item.id}>{item.name}</option>)}</select>}</Field><div className="order-total"><span>应付金额</span><strong className="mono">{money(product.price_cents * count)}</strong></div>{product.stock !== null && <Notice>当前库存 {product.stock} 个，以提交订单时的库存为准。</Notice>}<Button type="submit" disabled={create.isPending || !selectedProvider || (!subscriptionId && (count < product.min_qty || count > product.max_qty)) || !Number.isInteger(count)}>{create.isPending ? '正在创建订单…' : session.data ? '创建订单并支付' : '登录后购买'}</Button></form>}</Modal>
}
