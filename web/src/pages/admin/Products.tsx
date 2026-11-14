import { useId, useState } from 'react'
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

// Labels that depend on the product kind: what one unit buys and how it is counted.
const kindUnits: Record<ProductKind, { label: string; unit: string; description: string }> = {
  concurrency: { label: '每份包含并发', unit: '路', description: '在基础并发之上叠加的并发路数' },
  hosted_seat: { label: '每份包含席位', unit: '个', description: '可托管用户自己的 Claude 账号' },
  rental_account: { label: '每份包含账号', unit: '个', description: '从租赁池分配平台账号' },
}

// Prices are edited in yuan and stored as integer cents. Parse the text exactly
// (no floating point) so "19.9" is 1990 and never 1989.
function yuanToCents(value: string): number | null {
  const match = /^(\d{1,9})(?:\.(\d{0,2}))?$/.exec(value.trim())
  if (!match) return null
  return Number(match[1]) * 100 + Number((match[2] ?? '').padEnd(2, '0'))
}
const centsToYuan = (cents: number) => (cents / 100).toFixed(2)

function validateProduct(form: ProductInput, cents: number | null): string {
  if (!form.name.trim()) return '请填写商品名称。'
  if (cents === null) return '价格须为非负金额，最多两位小数，例如 49 或 19.90。'
  if (!Number.isSafeInteger(form.unit_quantity) || form.unit_quantity < 1) return `${kindUnits[form.kind].label}须为正整数。`
  if (!Number.isSafeInteger(form.duration_days) || form.duration_days < 1) return '有效时长须为正整数天。'
  if (!Number.isSafeInteger(form.min_qty) || form.min_qty < 1) return '每单最少购买份数须为正整数。'
  if (!Number.isSafeInteger(form.max_qty) || form.max_qty < form.min_qty) return '每单最多购买份数须为整数，且不能小于最少份数。'
  if (!Number.isSafeInteger(form.sort_order)) return '排序值须为整数。'
  return ''
}

function ProductEditor({ product, onClose }: { product?: Product; onClose: () => void }) {
  const formId = useId()
  const [form, setForm] = useState<ProductInput>(() => product ? { kind: product.kind, name: product.name, description: product.description, unit_quantity: product.unit_quantity, duration_days: product.duration_days, price_cents: product.price_cents, currency: product.currency, min_qty: product.min_qty, max_qty: product.max_qty, enabled: product.enabled, sort_order: product.sort_order, badge: product.badge } : { ...initialProduct })
  const [price, setPrice] = useState(() => product ? centsToYuan(product.price_cents) : '')
  const [validation, setValidation] = useState('')
  const mutation = useAdminMutation((input: ProductInput) => product ? api.patch(`/admin/products/${product.id}`, input) : api.post('/admin/products', input), product ? '商品已保存' : '商品已创建', onClose)
  const unit = kindUnits[form.kind]
  const cents = yuanToCents(price)
  function numeric(key: 'unit_quantity' | 'duration_days' | 'min_qty' | 'max_qty' | 'sort_order', value: string) { setForm((current) => ({ ...current, [key]: value === '' ? Number.NaN : Number(value) })) }
  const shown = (value: number) => Number.isNaN(value) ? '' : value
  const preview = cents !== null && form.unit_quantity >= 1 && form.duration_days >= 1
    ? `${money(cents)} / 份 · 每份 ${number(form.unit_quantity)} ${unit.unit}${kindLabel[form.kind]} · 有效 ${number(form.duration_days)} 天${form.unit_quantity > 1 ? ` · 折合 ${money(Math.round(cents / form.unit_quantity))} / ${unit.unit}` : ''}`
    : '填写价格、数量和时长后显示前台展示效果'
  const footer = <>
    <MutationError error={validation ? new Error(validation) : mutation.error} />
    <Button type="button" variant="secondary" disabled={mutation.isPending} onClick={onClose}>取消</Button>
    <Button type="submit" form={formId} disabled={mutation.isPending}>{mutation.isPending ? '正在保存…' : product ? '保存修改' : '创建商品'}</Button>
  </>
  return <Modal open wide title={product ? `编辑商品 · ${product.name}` : '新建商品'} footer={footer} onClose={() => { if (!mutation.isPending) onClose() }}><form id={formId} className="form-sections" noValidate onSubmit={(event) => {
    event.preventDefault()
    const message = validateProduct(form, cents)
    if (message) { setValidation(message); return }
    setValidation(''); mutation.mutate({ ...form, name: form.name.trim(), badge: form.badge.trim(), description: form.description.trim(), price_cents: cents ?? 0, currency: 'CNY' })
  }}>
    <section className="form-section">
      <header><h3>基本信息</h3><p className="muted">商品名称、类型和说明会展示在前台商品页。</p></header>
      <div className="field"><span>商品类型</span><div className="segmented" role="radiogroup" aria-label="商品类型">{(Object.keys(kindUnits) as ProductKind[]).map((key) => <label key={key} className={form.kind === key ? 'active' : ''}><input type="radio" name="kind" value={key} checked={form.kind === key} onChange={() => setForm({ ...form, kind: key })} /><strong>{kindLabel[key]}</strong><small>{kindUnits[key].description}</small></label>)}</div></div>
      <div className="form-grid">
        <Field label="商品名称"><FieldInput required maxLength={200} placeholder="例如：托管席位 · 30 天" value={form.name} onChange={(event) => setForm({ ...form, name: event.target.value })} /></Field>
        <Field label="展示标签" hint="显示在商品名旁"><FieldInput maxLength={50} placeholder="例如：推荐（留空则不显示）" value={form.badge} onChange={(event) => setForm({ ...form, badge: event.target.value })} /></Field>
      </div>
      <Textarea label="商品说明" hint="显示在商品卡片上，建议一两句话说明适用场景。" rows={2} value={form.description} onChange={(event) => setForm({ ...form, description: event.target.value })} />
    </section>
    <section className="form-section">
      <header><h3>规格与定价</h3><p className="muted">按「份」售卖，用户下单时选择购买几份。</p></header>
      <div className="form-grid form-grid-3">
        <Field label="售价"><div className="input-group"><span>¥</span><FieldInput inputMode="decimal" required placeholder="0.00" value={price} onChange={(event) => setPrice(event.target.value)} onBlur={() => { const parsed = yuanToCents(price); if (parsed !== null) setPrice(centsToYuan(parsed)) }} /><span>/ 份</span></div></Field>
        <Field label={unit.label}><div className="input-group"><FieldInput type="number" required min={1} step={1} value={shown(form.unit_quantity)} onChange={(event) => numeric('unit_quantity', event.target.value)} /><span>{unit.unit}</span></div></Field>
        <Field label="有效时长"><div className="input-group"><FieldInput type="number" required min={1} step={1} value={shown(form.duration_days)} onChange={(event) => numeric('duration_days', event.target.value)} /><span>天</span></div></Field>
      </div>
      <div className="price-preview"><span className="muted">前台展示</span><strong className={cents === null ? 'muted' : 'mono'}>{preview}</strong></div>
    </section>
    <section className="form-section">
      <header><h3>购买与展示</h3><p className="muted">限制每单购买份数，控制商品排序与上架状态。</p></header>
      <div className="form-grid form-grid-3">
        <Field label="每单最少"><div className="input-group"><FieldInput type="number" required min={1} step={1} value={shown(form.min_qty)} onChange={(event) => numeric('min_qty', event.target.value)} /><span>份</span></div></Field>
        <Field label="每单最多"><div className="input-group"><FieldInput type="number" required min={form.min_qty || 1} step={1} value={shown(form.max_qty)} onChange={(event) => numeric('max_qty', event.target.value)} /><span>份</span></div></Field>
        <Field label="排序值" hint="数字越小越靠前。"><FieldInput type="number" required step={1} value={shown(form.sort_order)} onChange={(event) => numeric('sort_order', event.target.value)} /></Field>
      </div>
      <label className="switch-field"><input type="checkbox" role="switch" checked={form.enabled} onChange={(event) => setForm({ ...form, enabled: event.target.checked })} /><span><strong>{form.enabled ? '上架销售' : '暂不上架'}</strong><small>{form.enabled ? '保存后立即出现在前台商品页。' : '保存后仅后台可见，可随时上架。'}</small></span></label>
    </section>
  </form></Modal>
}
