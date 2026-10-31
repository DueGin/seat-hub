import { useState } from 'react'
import { useQuery } from '@tanstack/react-query'
import { Badge, Button, Card, EmptyState, Field, FieldInput, Modal, PageHeader, Pagination, QueryState } from '@/components/ui'
import { api } from '@/lib/api'
import { kindLabel, money, number } from '@/lib/format'
import type { Product, ProductKind } from '@/lib/types'
import { ConfirmAction, MutationError, Select, Textarea, useAdminMutation } from './shared'

type ProductInput = Omit<Product, 'id' | 'stock'>
const initialProduct: ProductInput = { kind: 'concurrency', name: '', description: '', unit_quantity: 1, duration_days: 30, price_cents: 0, currency: 'CNY', min_qty: 1, max_qty: 20, enabled: false, sort_order: 0, badge: '' }

export function Products() {
  const query = useQuery({ queryKey: ['admin', 'products'], queryFn: () => api.get<{ items: Product[] }>('/admin/products') })
  const [editor, setEditor] = useState<Product | 'new' | null>(null)
  const [action, setAction] = useState<{ product: Product; type: 'toggle' | 'delete' } | null>(null)
  const [search, setSearch] = useState('')
  const [kind, setKind] = useState('')
  const [enabled, setEnabled] = useState('')
  const [page, setPage] = useState(1)
  const mutation = useAdminMutation(async (value: NonNullable<typeof action>) => value.type === 'delete' ? api.delete(`/admin/products/${value.product.id}`) : api.patch(`/admin/products/${value.product.id}`, { enabled: !value.product.enabled }), '商品状态已更新', () => setAction(null))
  const items = (query.data?.items ?? []).filter((item) => (!search || `${item.name} ${item.description}`.toLowerCase().includes(search.toLowerCase())) && (!kind || item.kind === kind) && (!enabled || String(item.enabled) === enabled))
  const currentPage = Math.min(page, Math.max(1, Math.ceil(items.length / 20)))
  return <div className="stack">
    <PageHeader eyebrow="商业配置" title="商品管理" description="配置售卖规格与价格，租赁商品库存由资源池实时计算。" actions={<Button onClick={() => setEditor('new')}>新建商品</Button>} />
    <Card><div className="toolbar"><Field label="搜索商品"><FieldInput type="search" placeholder="名称或描述" value={search} onChange={(event) => { setSearch(event.target.value); setPage(1) }} /></Field><Select label="商品类型" value={kind} onChange={(event) => { setKind(event.target.value); setPage(1) }}><option value="">全部类型</option>{Object.entries(kindLabel).filter(([key]) => ['concurrency', 'hosted_seat', 'rental_account'].includes(key)).map(([key, text]) => <option key={key} value={key}>{text}</option>)}</Select><Select label="上架状态" value={enabled} onChange={(event) => { setEnabled(event.target.value); setPage(1) }}><option value="">全部状态</option><option value="true">已上架</option><option value="false">已下架</option></Select></div>
      <QueryState query={query}>{items.length ? <><div className="table-wrap"><table className="data-table"><thead><tr><th>商品</th><th>规格</th><th>价格</th><th>购买范围</th><th>库存 / 状态</th><th>操作</th></tr></thead><tbody>{items.slice((currentPage - 1) * 20, currentPage * 20).map((product) => <tr key={product.id}><td><strong>{product.name}</strong>{product.badge && <Badge>{product.badge}</Badge>}<p className="muted">{kindLabel[product.kind]} · 排序 {product.sort_order}</p></td><td>{number(product.unit_quantity)} {kindLabel[product.kind]}<p className="muted">{number(product.duration_days)} 天</p></td><td className="mono">{money(product.price_cents)}</td><td>{product.min_qty}–{product.max_qty} 份</td><td><Badge status={product.enabled ? 'enabled' : 'offline'} />{product.kind === 'rental_account' && <p className="muted">{product.stock == null ? '库存暂不可用' : `可售 ${number(product.stock)} 个`}</p>}</td><td><div className="toolbar"><Button variant="secondary" onClick={() => setEditor(product)}>编辑</Button><Button variant="ghost" onClick={() => { mutation.reset(); setAction({ product, type: 'toggle' }) }}>{product.enabled ? '下架' : '上架'}</Button><Button variant="danger" onClick={() => { mutation.reset(); setAction({ product, type: 'delete' }) }}>删除</Button></div></td></tr>)}</tbody></table></div><Pagination page={currentPage} total={items.length} onChange={setPage} /></> : <EmptyState title="没有符合条件的商品" description="调整筛选条件，或创建第一个商品。" />}</QueryState>
    </Card>
    {editor && <ProductEditor product={editor === 'new' ? undefined : editor} onClose={() => setEditor(null)} />}
    {action && <ConfirmAction title={action.type === 'delete' ? '删除商品' : action.product.enabled ? '下架商品' : '上架商品'} description={action.type === 'delete' ? `确认删除「${action.product.name}」？如已有订单引用，系统只会下架并保留历史记录。` : `确认${action.product.enabled ? '下架' : '上架'}「${action.product.name}」？历史订单和已生效订阅不会改变。`} danger={action.type === 'delete' || action.product.enabled} pending={mutation.isPending} error={mutation.error} onClose={() => setAction(null)} onConfirm={() => mutation.mutate(action)} />}
  </div>
}

function ProductEditor({ product, onClose }: { product?: Product; onClose: () => void }) {
  const [form, setForm] = useState<ProductInput>(() => product ? { kind: product.kind, name: product.name, description: product.description, unit_quantity: product.unit_quantity, duration_days: product.duration_days, price_cents: product.price_cents, currency: product.currency, min_qty: product.min_qty, max_qty: product.max_qty, enabled: product.enabled, sort_order: product.sort_order, badge: product.badge } : { ...initialProduct })
  const [validation, setValidation] = useState('')
  const mutation = useAdminMutation((input: ProductInput) => product ? api.patch(`/admin/products/${product.id}`, input) : api.post('/admin/products', input), product ? '商品已保存' : '商品已创建', onClose)
  function numeric(key: 'unit_quantity' | 'duration_days' | 'price_cents' | 'min_qty' | 'max_qty' | 'sort_order', value: string) { setForm((current) => ({ ...current, [key]: value === '' ? Number.NaN : Number(value) })) }
  return <Modal open wide title={product ? `编辑商品 · ${product.name}` : '新建商品'} onClose={() => { if (!mutation.isPending) onClose() }}><form className="stack" onSubmit={(event) => {
    event.preventDefault()
    const integers = [form.unit_quantity, form.duration_days, form.price_cents, form.min_qty, form.max_qty, form.sort_order]
    if (!form.name.trim() || !integers.every(Number.isSafeInteger) || form.unit_quantity < 1 || form.duration_days < 1 || form.price_cents < 0 || form.min_qty < 1 || form.max_qty < form.min_qty) { setValidation('请填写商品名称；规格、时长和数量必须为正整数，价格为非负整数分，最大数量不能小于最小数量。'); return }
    setValidation(''); mutation.mutate({ ...form, name: form.name.trim(), currency: 'CNY' })
  }}>
    <div className="form-grid">
      <Field label="商品名称"><FieldInput required maxLength={200} value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
      <Select label="商品类型" value={form.kind} onChange={(event) => setForm({ ...form, kind: event.target.value as ProductKind })}><option value="concurrency">并发</option><option value="hosted_seat">托管席位</option><option value="rental_account">租赁账号</option></Select>
      <Field label="每份权益数量"><FieldInput type="number" required min={1} step={1} value={Number.isNaN(form.unit_quantity) ? '' : form.unit_quantity} onChange={(event) => numeric('unit_quantity', event.target.value)} /></Field>
      <Field label="有效时长（天）"><FieldInput type="number" required min={1} step={1} value={Number.isNaN(form.duration_days) ? '' : form.duration_days} onChange={(event) => numeric('duration_days', event.target.value)} /></Field>
      <Field label="价格（人民币分）" hint="例如 9900 表示 ¥99.00，金额仅以整数分提交。"><FieldInput type="number" required min={0} step={1} value={Number.isNaN(form.price_cents) ? '' : form.price_cents} onChange={(event) => numeric('price_cents', event.target.value)} /></Field>
      <Field label="展示标签"><FieldInput maxLength={50} placeholder="例如：热销" value={form.badge} onChange={(event) => setForm({ ...form, badge: event.target.value })} /></Field>
      <Field label="最小购买份数"><FieldInput type="number" required min={1} step={1} value={Number.isNaN(form.min_qty) ? '' : form.min_qty} onChange={(event) => numeric('min_qty', event.target.value)} /></Field>
      <Field label="最大购买份数"><FieldInput type="number" required min={form.min_qty || 1} step={1} value={Number.isNaN(form.max_qty) ? '' : form.max_qty} onChange={(event) => numeric('max_qty', event.target.value)} /></Field>
      <Field label="排序值"><FieldInput type="number" required step={1} value={Number.isNaN(form.sort_order) ? '' : form.sort_order} onChange={(event) => numeric('sort_order', event.target.value)} /></Field>
      <label className="row"><input type="checkbox" checked={form.enabled} onChange={(event) => setForm({ ...form, enabled: event.target.checked })} />保存后上架</label>
    </div>
    <Textarea label="商品说明" rows={3} value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} />
    <MutationError error={validation ? new Error(validation) : mutation.error} />
    <div className="toolbar"><Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button><Button type="submit" disabled={mutation.isPending}>{mutation.isPending ? '正在保存…' : '保存商品'}</Button></div>
  </form></Modal>
}
