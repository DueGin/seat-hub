import { Component, lazy, Suspense, useEffect, useState } from 'react'
import type { ErrorInfo, ReactNode } from 'react'
import { Navigate, NavLink, Outlet, Route, Routes, useLocation, useNavigate } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { Activity, ArrowUpRight, History, Boxes, CircleHelp, CreditCard, Gauge, KeyRound, LayoutDashboard, ListTodo, LogOut, Menu, Package, Server, Settings, ShieldCheck, ShoppingBag, Users, X, Zap } from 'lucide-react'
import { api, isMock } from '@/lib/api'
import { useSession } from '@/lib/session'
import { Brand, useSiteName } from '@/components/brand'
import { Button, Card, EmptyState, ErrorState, LoadingState, ThemeToggle } from '@/components/ui'

const LandingPage = lazy(() => import('@/pages/public').then((module) => ({ default: module.LandingPage })))
const AuthPage = lazy(() => import('@/pages/public').then((module) => ({ default: module.AuthPage })))
const OverviewPage = lazy(() => import('@/pages/overview').then((module) => ({ default: module.OverviewPage })))
const SubscriptionsPage = lazy(() => import('@/pages/overview').then((module) => ({ default: module.SubscriptionsPage })))
const AccountsPage = lazy(() => import('@/pages/accounts').then((module) => ({ default: module.AccountsPage })))
const AccountDetailPage = lazy(() => import('@/pages/accounts').then((module) => ({ default: module.AccountDetailPage })))
const HostedWizardPage = lazy(() => import('@/pages/accounts').then((module) => ({ default: module.HostedWizardPage })))
const UsagePage = lazy(() => import('@/pages/usage').then((module) => ({ default: module.UsagePage })))
const RecordsPage = lazy(() => import('@/pages/records').then((module) => ({ default: module.RecordsPage })))
const KeysPage = lazy(() => import('@/pages/keys').then((module) => ({ default: module.KeysPage })))
const OrdersPage = lazy(() => import('@/pages/orders').then((module) => ({ default: module.OrdersPage })))
const OrderDetailPage = lazy(() => import('@/pages/orders').then((module) => ({ default: module.OrderDetailPage })))
const AdminPage = lazy(() => import('@/pages/admin').then((module) => ({ default: module.AdminPage })))
const ProfilePage = lazy(() => import('@/pages/profile').then((module) => ({ default: module.ProfilePage })))

function Protected({ admin = false }: { admin?: boolean }) {
  const session = useSession()
  const location = useLocation()
  if (session.isPending) return <LoadingState />
  if (session.isError) return <ErrorState error={session.error} retry={() => { void session.refetch() }} />
  if (!session.data) return <Navigate to={`/login?returnTo=${encodeURIComponent(location.pathname + location.search)}`} replace />
  if (admin && session.data.role !== 'admin') return <Card><EmptyState title="此页面仅对管理员开放" description="请使用有管理权限的账号访问。" /></Card>
  return <Outlet />
}
const userNav = [
  { to: '/overview', label: '总览', icon: Gauge }, { to: '/accounts', label: '我的账号', icon: Server }, { to: '/keys', label: 'API 密钥', icon: KeyRound }, { to: '/usage', label: '用量分析', icon: Activity }, { to: '/records', label: '使用记录', icon: History }, { to: '/subscriptions', label: '我的订阅', icon: Boxes }, { to: '/orders', label: '订单记录', icon: CreditCard }, { to: '/profile', label: '账户安全', icon: ShieldCheck },
]
const adminNav = [
  { to: '/admin/dashboard', label: '管理总览', icon: LayoutDashboard }, { to: '/admin/products', label: '商品管理', icon: Package }, { to: '/admin/orders', label: '订单管理', icon: ShoppingBag }, { to: '/admin/users', label: '用户管理', icon: Users }, { to: '/admin/rental-pool', label: '租赁池', icon: Boxes }, { to: '/admin/accounts', label: '资源管理', icon: Server }, { to: '/admin/jobs', label: '任务队列', icon: ListTodo }, { to: '/admin/settings', label: '站点设置', icon: Settings },
]
function AppLayout() {
  const [open, setOpen] = useState(false)
  const [loggingOut, setLoggingOut] = useState(false)
  const session = useSession()
  const siteName = useSiteName()
  const location = useLocation()
  const navigate = useNavigate()
  const client = useQueryClient()
  const admin = location.pathname.startsWith('/admin') && session.data?.role === 'admin'
  const navigation = admin ? adminNav : userNav
  const current = navigation.find((item) => location.pathname.startsWith(item.to))
  useEffect(() => { setOpen(false); window.scrollTo({ top: 0 }) }, [location.pathname])
  async function logout() { setLoggingOut(true); try { await api.post('/auth/logout'); client.clear(); navigate('/login') } finally { setLoggingOut(false) } }
  return <div className="app-layout">{open && <button className="sidebar-overlay" aria-label="关闭导航" onClick={() => setOpen(false)} />}<aside className={`sidebar ${open ? 'sidebar-open' : ''}`}><div className="sidebar-brand"><Brand /><Button variant="ghost" className="mobile-only" onClick={() => setOpen(false)} aria-label="关闭导航"><X size={18} /></Button></div><div className="workspace-label"><span className="workspace-avatar">{admin ? <ShieldCheck size={15} /> : '个'}</span><div><strong>{admin ? '管理工作区' : '个人工作区'}</strong><small>{admin ? '站点运营与资源调度' : '你的 AI 资源工作台'}</small></div></div><p className="nav-section-label">{admin ? '平台管理' : '工作空间'}</p><nav aria-label="主导航">{navigation.map(({ to, label, icon: Icon }) => <NavLink key={to} to={to} className={({ isActive }) => `nav-item ${isActive ? 'active' : ''}`}><Icon size={18} /><span>{label}</span>{label === '我的账号' && <span className="nav-item-dot" />}</NavLink>)}</nav><div className="sidebar-bottom">{session.data?.role === 'admin' && <NavLink className="nav-item" to={admin ? '/overview' : '/admin/dashboard'}><ShieldCheck size={17} />{admin ? '返回用户中心' : '管理后台'}<ArrowUpRight size={14} /></NavLink>}<a className="nav-item" href="/#products"><ShoppingBag size={17} />产品与定价<ArrowUpRight size={14} /></a><NavLink className="nav-item" to="/keys"><CircleHelp size={17} />接入指南</NavLink><div className="system-state"><span className="live-dot" />{isMock ? '本地演示环境' : siteName}</div><div className="user-profile"><span className="user-avatar">{session.data?.email.charAt(0).toUpperCase()}</span><div><strong>{session.data?.email.split('@')[0]}</strong><small>{session.data?.role === 'admin' ? '管理员' : '个人账号'}</small></div><Button variant="ghost" aria-label="退出登录" title="退出登录" disabled={loggingOut} onClick={() => { void logout() }}><LogOut size={16} /></Button></div></div></aside><div className="app-main"><header className="topbar"><div className="row"><Button variant="ghost" className="mobile-only" aria-label="打开导航" onClick={() => setOpen(true)}><Menu size={20} /></Button><span className="breadcrumb">{admin ? '管理工作区' : '个人工作区'}<span>/</span><strong>{current?.label || '账号详情'}</strong></span></div><div className="topbar-right">{isMock && <span className="demo-tag">演示模式</span>}<ThemeToggle /><a href="/#products" className="topbar-upgrade"><Zap size={14} />扩展你的资源<ArrowUpRight size={13} /></a></div></header><main className="content"><Suspense fallback={<LoadingState />}><Outlet /></Suspense><footer className="app-footer"><span>{siteName} · 智能连接，有序运行</span><span>额度与用量以实时数据为准</span></footer></main></div></div>
}
class AppErrorBoundary extends Component<{ children: ReactNode }, { failed: boolean }> {
  state = { failed: false }
  static getDerivedStateFromError() { return { failed: true } }
  componentDidCatch(error: Error, info: ErrorInfo) { console.error('页面渲染失败', error, info.componentStack) }
  render() { return this.state.failed ? <div className="fatal-error"><ErrorState error={new Error('页面暂时无法显示，请刷新重试')} retry={() => window.location.reload()} /></div> : this.props.children }
}
// SiteTitle names the tab after the operator's site, not the upstream product.
function SiteTitle() {
  const name = useSiteName()
  useEffect(() => { document.title = `${name} · 智能连接，有序运行` }, [name])
  return null
}
export default function App() {
  return <AppErrorBoundary><SiteTitle /><Suspense fallback={<LoadingState />}><Routes><Route path="/" element={<LandingPage />} /><Route path="/login" element={<AuthPage />} /><Route path="/register" element={<AuthPage register />} /><Route element={<Protected />}><Route element={<AppLayout />}><Route path="/overview" element={<OverviewPage />} /><Route path="/accounts" element={<AccountsPage />} /><Route path="/accounts/new" element={<HostedWizardPage />} /><Route path="/accounts/:id" element={<AccountDetailPage />} /><Route path="/subscriptions" element={<SubscriptionsPage />} /><Route path="/keys" element={<KeysPage />} /><Route path="/usage" element={<UsagePage />} /><Route path="/records" element={<RecordsPage />} /><Route path="/profile" element={<ProfilePage />} /><Route path="/orders" element={<OrdersPage />} /><Route path="/orders/:orderNo" element={<OrderDetailPage />} /><Route element={<Protected admin />}><Route path="/admin" element={<Navigate to="/admin/dashboard" replace />} /><Route path="/admin/:section" element={<AdminPage />} /></Route></Route></Route><Route path="*" element={<div className="not-found"><Brand /><EmptyState title="这个页面走丢了" description="返回工作台，继续你的工作。" action={<NavLink className="button button-primary" to="/overview">返回工作台</NavLink>} /></div>} /></Routes></Suspense></AppErrorBoundary>
}
