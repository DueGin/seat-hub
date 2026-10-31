export const money = (cents: number) => new Intl.NumberFormat('zh-CN', { style: 'currency', currency: 'CNY' }).format(cents / 100)
export const usd = (amount: number) => new Intl.NumberFormat('zh-CN', { style: 'currency', currency: 'USD', minimumFractionDigits: 2, maximumFractionDigits: 4 }).format(amount)
export const number = (value: number) => new Intl.NumberFormat('zh-CN').format(value)
export const compact = (value: number) => new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 }).format(value)
export const dateTime = (value: string | null | undefined) => value ? new Date(value).toLocaleString('zh-CN', { hour12: false }) : '暂无'
export const date = (value: string | null | undefined) => value ? new Date(value).toLocaleDateString('zh-CN') : '长期有效'
export const kindLabel: Record<string, string> = { concurrency: '并发', hosted_seat: '托管席位', rental_account: '租赁账号', hosted: '托管账号', rental: '租赁账号' }
export function countdown(value: string | null | undefined, now = Date.now()) {
  if (!value) return '等待同步'
  const minutes = Math.max(0, Math.ceil((new Date(value).getTime() - now) / 60_000))
  if (!minutes) return '即将重置'
  const hours = Math.floor(minutes / 60)
  return hours >= 24 ? `${Math.floor(hours / 24)} 天 ${hours % 24} 小时` : `${hours ? `${hours} 小时 ` : ''}${minutes % 60} 分钟`
}
