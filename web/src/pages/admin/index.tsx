import { Link, useParams } from 'react-router-dom'
import { EmptyState, PageHeader } from '@/components/ui'
import { Dashboard } from './Dashboard'
import { Products } from './Products'
import { Orders } from './Orders'
import { Users } from './Users'
import { RentalPool } from './RentalPool'
import { Accounts } from './Accounts'
import { Jobs } from './Jobs'
import { Settings } from './Settings'
import { SystemUpdate } from './System'

export function AdminPage() {
  const { section = 'dashboard' } = useParams<{ section?: string }>()
  switch (section) {
    case 'dashboard': return <Dashboard />
    case 'products': return <Products />
    case 'orders': return <Orders />
    case 'users': return <Users />
    case 'rental-pool': return <RentalPool />
    case 'accounts': return <Accounts />
    case 'jobs': return <Jobs />
    case 'settings': return <Settings />
    case 'system': return <SystemUpdate />
    default: return <div className="stack"><PageHeader title="未找到管理页面" /><EmptyState title="页面不存在" description="请从管理导航选择一个有效页面。" action={<Link className="button button-secondary" to="/admin/dashboard">返回管理总览</Link>} /></div>
  }
}

export default AdminPage
