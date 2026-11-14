import { Waypoints } from 'lucide-react'
import { Link } from 'react-router-dom'
import { useConfig } from '@/lib/session'

// The wordmark is the operator's site name, never the upstream product: users
// of the storefront should not see what it runs on.
export const DEFAULT_SITE_NAME = 'AI 工作台'

export function useSiteName() {
  const config = useConfig()
  return config.data?.site_name?.trim() || DEFAULT_SITE_NAME
}

export function Brand() {
  const name = useSiteName()
  return <Link to="/" className="brand" aria-label={`${name} 首页`}><span className="brand-mark"><Waypoints size={21} /></span><span className="brand-name">{name}</span></Link>
}
