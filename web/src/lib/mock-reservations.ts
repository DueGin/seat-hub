import type { MockHandler, MockInput, MockKit } from './mock-kit'
import type { Product } from './types'

type Reservation = { id: number; product_id: number; user_id: number | null; email: string; note: string; status: 'waiting' | 'notified'; ip: string; created_at: string; notified_at: string | null }
type MailConfig = { enabled: boolean; host: string; port: number; security: string; username: string; password: string; from_address: string; from_name: string; admin_emails: string[] }
let productSource: () => Product[] = () => []
export function setReservationProducts(source: () => Product[]) { productSource = source }
let nextId = 20
const reservations: Reservation[] = [{ id: 1, product_id: 6, user_id: 2, email: 'user@example.com', note: '希望有季度方案', status: 'waiting', ip: '127.0.0.1', created_at: new Date().toISOString(), notified_at: null }]
let mail: MailConfig = { enabled: false, host: '', port: 587, security: 'starttls', username: '', password: '', from_address: '', from_name: '', admin_emails: [] }
let passwordStored = false
const attempts: number[] = []
const address = (value: string) => value.length <= 254 && /^[^\s<>@]+@[^\s<>@]+\.[^\s<>@]+$/.test(value)
export function reservationEmail(value: string) { return value.trim().toLowerCase() }
export function reservationCSV(rows: string[][]) {
  return '\ufeff' + rows.map(row => row.map(value => {
    const guarded = /^[\t\r\n]|^\s*[=+\-@]/.test(value) ? `'${value}` : value
    return `"${guarded.replaceAll('"', '""')}"`
  }).join(',')).join('\r\n') + '\r\n'
}
function text(input: MockInput, key: string, max: number, kit: MockKit) {
  const value = input[key] ?? ''
  if (typeof value !== 'string' || Array.from(value.trim()).length > max) return kit.fail(422, 'invalid_reservation', '请检查填写的内容，长度超出限制')
  return value.trim()
}
function ready() { return mail.enabled && mail.host !== '' && address(mail.from_address) && (!mail.username || passwordStored) }
function filtered(query: URLSearchParams, kit: MockKit) {
  const product = query.get('product_id'), status = query.get('status'), email = query.get('email') ?? ''
  if ((product && (!Number.isSafeInteger(Number(product)) || Number(product) <= 0)) || (status && !['waiting', 'notified'].includes(status)) || email.length > 254) kit.fail(422, 'invalid_filter', '预约筛选条件不正确')
  return reservations.filter(item => (!product || item.product_id === Number(product)) && (!status || item.status === status) && item.email.includes(email.toLowerCase())).sort((a, b) => b.id - a.id)
}
export const reservationMock: MockHandler = (path, method, query, input, kit) => {
  const publicMatch = /^\/public\/products\/(\d+)\/reservations$/.exec(path)
  if (publicMatch && method === 'POST') {
    const product = productSource().find(item => item.id === Number(publicMatch[1]) && item.enabled && item.reservation_only)
    if (!product) return kit.fail(422, 'product_not_reservable', '该商品暂不接受预约')
    const email = reservationEmail(text(input, 'email', 254, kit)), note = text(input, 'note', 200, kit)
    if (!address(email) || note.includes('\0')) kit.fail(422, 'invalid_reservation', '请填写正确的邮箱，备注最多 200 字')
    const now = Date.now(); while (attempts.length && attempts[0] <= now - 3600000) attempts.shift()
    if (attempts.length >= 10) kit.fail(429, 'reservation_rate_limited', '预约操作过于频繁，请一小时后再试')
    attempts.push(now)
    if (!reservations.some(item => item.product_id === product.id && item.email === email)) reservations.push({ id: ++nextId, product_id: product.id, user_id: kit.optionalUser()?.id ?? null, email, note, status: 'waiting', ip: '127.0.0.1', created_at: kit.iso(), notified_at: null })
    return kit.ok({})
  }
  if (!path.startsWith('/admin/reservations') && !/^\/admin\/products\/\d+\/reservations\/notify$/.test(path) && !path.startsWith('/admin/settings/mail')) return undefined
  if (kit.currentUser().role !== 'admin') kit.fail(403, 'forbidden', '需要管理员权限')
  if (path === '/admin/settings/mail') {
    if (method === 'GET') return kit.ok({ ...mail, password: passwordStored ? '***' : '' })
    if (method === 'PUT') {
      const enabled = input.enabled
      const host = text(input, 'host', 253, kit), from = text(input, 'from_address', 254, kit), security = text(input, 'security', 20, kit)
      const port = Number(input.port), username = text(input, 'username', 320, kit)
      if (typeof enabled !== 'boolean' || !Number.isInteger(port) || port < 1 || port > 65535 || !['tls', 'starttls', 'none'].includes(security) || (from && !address(from)) || /[\r\n\0]/.test(host + username)) kit.fail(422, 'invalid_mail_settings', '请检查邮件服务器、端口、加密方式和邮箱地址')
      if (!Array.isArray(input.admin_emails) || !input.admin_emails.every(value => typeof value === 'string' && address(value))) return kit.fail(422, 'invalid_mail_settings', '管理员通知邮箱不正确')
      const password = text(input, 'password', 4096, kit)
      const hasPassword = password === '***' ? passwordStored : Boolean(password)
      if (enabled && (!host || !address(from) || (username && !hasPassword))) kit.fail(422, 'invalid_mail_settings', '请填写服务器、发件邮箱和授权码')
      const fromName = text(input, 'from_name', 200, kit)
      if (/[\r\n\0]/.test(fromName)) kit.fail(422, 'invalid_mail_settings', '发件人名称不正确')
      passwordStored = hasPassword
      mail = { enabled: enabled as boolean, host, port, security, username, password: '', from_address: from, from_name: fromName, admin_emails: input.admin_emails as string[] }
      return kit.ok({ ...mail, password: hasPassword ? '***' : '' })
    }
  }
  if (path === '/admin/settings/mail/test' && method === 'POST') {
    if (!address(text(input, 'to', 254, kit))) kit.fail(422, 'invalid_email', '请填写有效的收件邮箱')
    if (!ready()) kit.fail(422, 'mail_not_configured', '请先启用并配置邮件发送')
    return kit.ok({})
  }
  if ((path === '/admin/reservations' || path === '/admin/reservations/export') && method === 'GET') {
    const items = filtered(query, kit)
    const productName = (id: number) => productSource().find(item => item.id === id)?.name ?? '已删除商品'
    if (path.endsWith('/export')) {
      const csv = reservationCSV([['编号', '商品', '邮箱', '备注', '状态', '预约时间', '通知时间'], ...items.map(item => [String(item.id), productName(item.product_id), item.email, item.note, item.status === 'waiting' ? '待通知' : '已通知', item.created_at, item.notified_at ?? ''])])
      return new Response(csv, { headers: { 'Content-Type': 'text/csv; charset=utf-8', 'Content-Disposition': 'attachment; filename="reservations.csv"' } })
    }
    const page = Math.max(1, Number(query.get('page')) || 1), size = Math.min(100, Math.max(1, Number(query.get('page_size')) || 20))
    const counts = productSource().filter(product => product.reservation_only || reservations.some(item => item.product_id === product.id)).map(product => {
      const all = reservations.filter(item => item.product_id === product.id)
      return { product_id: product.id, product_name: product.name, total: all.length, waiting: all.filter(item => item.status === 'waiting').length, notified: all.filter(item => item.status === 'notified').length }
    })
    return kit.ok({ items: items.slice((page - 1) * size, page * size).map(item => ({ ...item, product_name: productName(item.product_id) })), total: items.length, page, page_size: size, counts })
  }
  const deleting = /^\/admin\/reservations\/(\d+)$/.exec(path)
  if (deleting && method === 'DELETE') {
    const index = reservations.findIndex(item => item.id === Number(deleting[1])); if (index < 0) kit.fail(404, 'reservation_not_found', '预约记录不存在')
    reservations.splice(index, 1); return kit.ok({})
  }
  const notify = /^\/admin\/products\/(\d+)\/reservations\/notify$/.exec(path)
  if (notify && method === 'POST') {
    const subject = text(input, 'subject', 200, kit), body = text(input, 'body', 20000, kit)
    if (!subject || !body || /[\r\n\0]/.test(subject)) kit.fail(422, 'invalid_reservation_notice', '请填写邮件标题和正文，标题最多 200 字且不能换行')
    if (!productSource().some(item => item.id === Number(notify[1]))) kit.fail(422, 'product_not_reservable', '商品不存在')
    if (!ready()) kit.fail(422, 'mail_not_configured', '请先在设置中启用并配置邮件发送，再通知预约用户')
    const waiting = reservations.filter(item => item.product_id === Number(notify[1]) && item.status === 'waiting')
    for (const item of waiting) { item.status = 'notified'; item.notified_at = kit.iso() }
    return kit.ok({ count: waiting.length })
  }
  return undefined
}
