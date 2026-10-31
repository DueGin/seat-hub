import { Bar, BarChart, CartesianGrid, ResponsiveContainer, Tooltip, XAxis, YAxis } from 'recharts'
import type { DailyUsage, ModelUsage } from '@/lib/types'
import { compact, number, usd } from '@/lib/format'
import { EmptyState } from '@/components/ui'

export function UsageChart({ daily, height = 240 }: { daily: DailyUsage[]; height?: number }) {
  if (!daily.length) return <EmptyState title="所选区间暂无用量" />
  return <div className="usage-chart" role="img" aria-label={`每日请求趋势，共 ${daily.length} 天`}><ResponsiveContainer width="100%" height={height}><BarChart data={daily} margin={{ top: 15, right: 10, left: -18, bottom: 0 }}><CartesianGrid stroke="#27302b" vertical={false} strokeDasharray="3 5" /><XAxis dataKey="date" tickFormatter={(value: string) => value.slice(5).replace('-', '/')} axisLine={false} tickLine={false} tick={{ fill: '#85928a', fontSize: 11 }} minTickGap={25} dy={9} /><YAxis axisLine={false} tickLine={false} tickFormatter={compact} tick={{ fill: '#85928a', fontSize: 11 }} /><Tooltip cursor={{ fill: '#ffffff06' }} contentStyle={{ background: '#202923', border: '1px solid #3c4c41', borderRadius: 10, color: '#e5ece7' }} labelFormatter={(label) => `日期 ${String(label)}`} formatter={(value) => [number(Number(value)), '请求数']} /><Bar dataKey="requests" fill="#a2d4a5" radius={[3, 3, 0, 0]} maxBarSize={22} /></BarChart></ResponsiveContainer></div>
}
export function ModelTable({ models }: { models: ModelUsage[] }) { return !models.length ? <EmptyState title="暂无模型用量" /> : <div className="table-wrap"><table className="data-table"><thead><tr><th>模型</th><th>请求数</th><th>令牌数</th><th>费用（美元）</th></tr></thead><tbody>{models.map((model) => <tr key={model.model}><td className="mono">{model.model}</td><td className="mono">{number(model.requests)}</td><td className="mono">{compact(model.tokens)}</td><td className="mono">{usd(model.cost_usd)}</td></tr>)}</tbody></table></div> }
