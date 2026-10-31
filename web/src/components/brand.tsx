import { Waypoints } from 'lucide-react'
import { Link } from 'react-router-dom'

export function Brand() { return <Link to="/" className="brand" aria-label="vm2api 首页"><span className="brand-mark"><Waypoints size={21} /></span><span>vm<span className="brand-soft">2</span>api<span className="brand-dot">.</span></span></Link> }
