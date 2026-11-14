import { createContext, useContext, useEffect, useId, useRef } from 'react'
import type { ButtonHTMLAttributes, InputHTMLAttributes, ReactNode } from 'react'
import { Check, Copy, Inbox, LoaderCircle, Monitor, Moon, RefreshCw, Sun, TriangleAlert, X } from 'lucide-react'
import { toast } from 'sonner'
import { setThemePreference, useTheme } from '@/lib/theme'

export function Button({ variant = 'primary', className = '', ...props }: ButtonHTMLAttributes<HTMLButtonElement> & { variant?: 'primary' | 'secondary' | 'ghost' | 'danger' }) { return <button className={`button button-${variant} ${className}`} {...props} /> }
export function Input({ className = '', ...props }: InputHTMLAttributes<HTMLInputElement>) { return <input className={`input ${className}`} {...props} /> }
const FieldContext = createContext<string | undefined>(undefined)
export function Field({ label, hint, children }: { label: string; hint?: string; children: ReactNode }) { const id = useId(); return <FieldContext value={id}><div className="field"><label htmlFor={id}>{label}</label>{children}{hint && <small>{hint}</small>}</div></FieldContext> }
export function FieldInput(props: InputHTMLAttributes<HTMLInputElement>) { const id = useContext(FieldContext); return <Input id={id} {...props} /> }
export function Card({ children, className = '' }: { children: ReactNode; className?: string }) { return <section className={`card ${className}`}>{children}</section> }
export function PageHeader({ title, description, actions, eyebrow }: { title: string; description?: string; actions?: ReactNode; eyebrow?: string }) { return <header className="page-header"><div>{eyebrow && <p className="eyebrow">{eyebrow}</p>}<h1>{title}</h1>{description && <p className="muted">{description}</p>}</div>{actions && <div className="header-actions">{actions}</div>}</header> }
const statusLabels: Record<string, string> = { active: '运行中', needs_reauth: '待重新授权', suspended: '已暂停', failed: '失败', awaiting_auth: '等待授权', provisioning: '创建中', bootstrapping: '初始化中', releasing: '释放中', released: '已释放', allocating: '分配中', pending: '待处理', paid: '已支付', fulfilled: '已完成', fulfill_failed: '履约失败', cancelled: '已取消', expired: '已过期', refunded: '已退款', disabled: '已停用', available: '可分配', leased: '已租出', maintenance: '维护中', running: '执行中', succeeded: '已完成', enabled: '已上架', offline: '已下架', pass: '自检通过', fail: '自检未通过', exhausted: '转人工处理' }
export function Badge({ status, children }: { status?: string; children?: ReactNode }) {
  const tone = ['active', 'fulfilled', 'succeeded', 'available', 'enabled', 'pass'].includes(status ?? '') ? 'green' : ['failed', 'fulfill_failed', 'exhausted'].includes(status ?? '') ? 'red' : ['needs_reauth', 'awaiting_auth', 'pending', 'paid', 'maintenance', 'provisioning', 'bootstrapping', 'running', 'allocating', 'fail'].includes(status ?? '') ? 'amber' : 'gray'
  return <span className={`badge badge-${tone}`}><span />{children ?? statusLabels[status ?? ''] ?? status}</span>
}
export function EmptyState({ title = '这里还没有数据', description = '完成操作后，内容将显示在这里。', action }: { title?: string; description?: string; action?: ReactNode }) { return <div className="state"><Inbox size={30} /><h3>{title}</h3><p>{description}</p>{action}</div> }
export function LoadingState() { return <div role="status" className="state"><LoaderCircle className="spin" size={26} /><p>正在加载工作台数据…</p></div> }
export function ErrorState({ retry, error }: { retry?: () => void; error?: unknown }) { return <div role="alert" className="state"><TriangleAlert size={28} /><h3>暂时无法加载</h3><p>{error instanceof Error ? error.message : '请稍后重试'}</p>{retry && <Button variant="secondary" onClick={retry}><RefreshCw size={14} />重新加载</Button>}</div> }
export function QueryState({ query, children }: { query: { isPending: boolean; isError: boolean; error: unknown; refetch: () => unknown }; children: ReactNode }) { return query.isPending ? <LoadingState /> : query.isError ? <ErrorState error={query.error} retry={() => { void query.refetch() }} /> : <>{children}</> }
// Modal keeps the header and the optional footer pinned while the body scrolls,
// so primary actions stay reachable however tall the content gets. Buttons in
// the footer submit a form in the body through the form="…" attribute.
export function Modal({ open, onClose, title, children, footer, wide = false }: { open: boolean; onClose: () => void; title: string; children: ReactNode; footer?: ReactNode; wide?: boolean }) {
  const ref = useRef<HTMLDialogElement>(null)
  const closeRef = useRef(onClose)
  const titleId = useId()
  useEffect(() => { closeRef.current = onClose }, [onClose])
  useEffect(() => { const dialog = ref.current; if (open && !dialog?.open) dialog?.showModal(); if (!open && dialog?.open) dialog.close(); return () => { if (dialog?.open) dialog.close() } }, [open])
  return <dialog ref={ref} className={`modal ${wide ? 'modal-wide' : ''}`} aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); closeRef.current() }} onClick={(event) => { if (event.target === event.currentTarget) closeRef.current() }}><div className="modal-header"><h2 id={titleId}>{title}</h2><Button variant="ghost" aria-label="关闭弹窗" onClick={onClose}><X size={18} /></Button></div>{open && <div className="modal-body">{children}</div>}{open && footer && <div className="modal-footer">{footer}</div>}</dialog>
}
export function Drawer({ open, onClose, title, children }: { open: boolean; onClose: () => void; title: string; children: ReactNode }) {
  const ref = useRef<HTMLDialogElement>(null)
  const closeRef = useRef(onClose)
  const titleId = useId()
  useEffect(() => { closeRef.current = onClose }, [onClose])
  useEffect(() => { const dialog = ref.current; if (open && !dialog?.open) dialog?.showModal(); if (!open && dialog?.open) dialog.close(); return () => { if (dialog?.open) dialog.close() } }, [open])
  return <dialog ref={ref} className="drawer" aria-labelledby={titleId} onCancel={(event) => { event.preventDefault(); closeRef.current() }} onClick={(event) => { if (event.target === event.currentTarget) closeRef.current() }}><div className="modal-header"><h2 id={titleId}>{title}</h2><Button variant="ghost" aria-label="关闭抽屉" onClick={onClose}><X size={18} /></Button></div><div className="drawer-body">{open && children}</div></dialog>
}
export function CopyButton({ value, label = '复制' }: { value: string; label?: string }) { return <Button variant="secondary" onClick={() => { void navigator.clipboard.writeText(value).then(() => toast.success('已复制到剪贴板')).catch(() => toast.error('复制失败，请手动选择文本复制')) }}><Copy size={14} />{label}</Button> }
export function StatCard({ label, value, detail, icon }: { label: string; value: ReactNode; detail?: ReactNode; icon?: ReactNode }) { return <Card className="stat-card"><div className="stat-label">{label}{icon}</div><div className="stat-value mono">{value}</div>{detail && <div className="stat-detail">{detail}</div>}</Card> }
export function Pagination({ page, total, pageSize = 20, onChange }: { page: number; total: number; pageSize?: number; onChange: (page: number) => void }) { return <div className="pagination"><span>共 <span className="mono">{total}</span> 条 · 第 {page} / {Math.max(1, Math.ceil(total / pageSize))} 页</span><div><Button variant="secondary" disabled={page <= 1} onClick={() => onChange(page - 1)}>上一页</Button><Button variant="secondary" disabled={page * pageSize >= total} onClick={() => onChange(page + 1)}>下一页</Button></div></div> }
export function Notice({ children, success = false, danger = false }: { children: ReactNode; success?: boolean; danger?: boolean }) { return <div className={`notice ${success ? 'notice-success' : danger ? 'notice-danger' : ''}`}>{success ? <Check size={18} /> : <TriangleAlert size={18} />}<div>{children}</div></div> }
const themeOptions = [
  { value: 'system', label: '跟随系统', icon: Monitor },
  { value: 'light', label: '浅色', icon: Sun },
  { value: 'dark', label: '深色', icon: Moon },
] as const
// ThemeToggle switches between following the OS and a pinned light/dark theme.
export function ThemeToggle({ className = '' }: { className?: string }) {
  const { preference } = useTheme()
  return <div className={`theme-toggle ${className}`} role="radiogroup" aria-label="界面主题">{themeOptions.map(({ value, label, icon: Icon }) => <button key={value} type="button" role="radio" aria-checked={preference === value} aria-label={label} title={label} className={preference === value ? 'active' : ''} onClick={() => setThemePreference(value)}><Icon size={14} /></button>)}</div>
}
