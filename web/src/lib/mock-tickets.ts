import type { MockHandler, MockKit } from './mock-kit'
import type { Ticket, TicketDetail, TicketMessage } from '../pages/tickets'
import type { TicketAttachment } from '../components/markdown-editor'

type StoredAttachment = TicketAttachment & { uploader_id: number; ticket_id?: number; data_url: string }
const demoPNG = 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jZl8AAAAASUVORK5CYII='
const screenshot = `data:image/svg+xml,${encodeURIComponent('<svg xmlns="http://www.w3.org/2000/svg" width="640" height="240" viewBox="0 0 640 240"><rect width="640" height="240" rx="16" fill="#f5f3ef"/><rect x="24" y="24" width="592" height="48" rx="8" fill="#e8e3dc"/><text x="42" y="54" font-size="18" font-family="sans-serif" fill="#34312d">API request log</text><rect x="24" y="92" width="592" height="124" rx="8" fill="#fff"/><circle cx="52" cy="126" r="6" fill="#da7038"/><text x="72" y="132" font-family="monospace" font-size="16" fill="#34312d">429 concurrency_limit_exceeded</text><text x="42" y="180" font-family="sans-serif" font-size="15" fill="#706b63">Try again after the active request completes.</text></svg>')}`
let nextID = 10
const attachments: StoredAttachment[] = [{ id: 1, url: '/api/tickets/attachments/1', name: '调用截图.png', size: 472, content_type: 'image/png', uploader_id: 1, ticket_id: 1, data_url: screenshot }]
const tickets: TicketDetail[] = []
const now = () => new Date().toISOString()
export function demoAttachmentURL(path: string): string { return attachments.find((a) => a.url === path)?.data_url || path }
function seed(kit: MockKit) {
  if (tickets.length) return
  const user = kit.users().find((u) => u.id === 1) || kit.currentUser()
  const base = (id: number, title: string, category: string, status: string, role: string, offset: number): Ticket => ({ id, user_id: user.id, user_email: user.email, title, category, status, created_at: kit.iso(offset), updated_at: kit.iso(offset + 1000), last_message_at: kit.iso(offset + 1000), last_message_role: role, unread: false, user_read_message_id: id === 1 ? 1 : 3, admin_read_message_id: id === 1 ? 2 : 0, user_read_at: kit.iso(offset), admin_read_at: null })
  tickets.push({ ticket: base(1, '并发请求返回 429，如何排查？', 'api', 'replied', 'admin', -86_400_000), messages: [{ id: 1, ticket_id: 1, author_id: user.id, role: 'user', body: '同时发起多个请求时出现了以下提示，请帮忙看一下。\n\n![调用截图](/api/tickets/attachments/1)', created_at: kit.iso(-86_400_000), attachments: [attachments[0]] }, { id: 2, ticket_id: 1, author_id: 1, role: 'admin', body: '已检查你的资源，当前并发额度为 **1 路**。\n\n- 请等待正在进行的请求完成后重试。\n- 如需同时运行多个任务，可以增加并发额度。\n\n```json\n{ "error": "concurrency_limit_exceeded" }\n```', created_at: kit.iso(-82_800_000), attachments: [] }] }, { ticket: base(2, '订单已支付，想确认开通进度', 'billing', 'open', 'user', -3_600_000), messages: [{ id: 3, ticket_id: 2, author_id: user.id, role: 'user', body: '刚刚完成支付，页面还在开通中，请帮忙确认一下，谢谢。', created_at: kit.iso(-3_600_000), attachments: [] }] })
}
function unread(ticket: TicketDetail, admin: boolean) { const marker = admin ? ticket.ticket.admin_read_message_id : ticket.ticket.user_read_message_id; return ticket.messages.some((m) => m.role === (admin ? 'user' : 'admin') && m.id > marker) }
export const ticketMock: MockHandler = (path, method, query, input, kit) => {
  if (!/^\/(admin\/)?tickets(?:\/|$)/.test(path)) return undefined
  const actor = kit.currentUser()
  const admin = path.startsWith('/admin/')
  if (admin && actor.role !== 'admin') kit.fail(403, 'forbidden', '需要管理员权限')
  seed(kit)
  const clean = path.replace(/^\/admin/, '')
  const visible = tickets.filter((d) => admin || d.ticket.user_id === actor.id)
  const invalid = () => kit.fail(422, 'ticket_invalid', '请检查工单分类、标题、正文和状态')
  const checkBody = () => { if (typeof input.body !== 'string' || !input.body.trim() || [...input.body].length > 20_000 || input.body.includes('\0')) invalid() }
  function bind(id: number): TicketAttachment[] {
    const ids = input.attachment_ids ?? []
    if (!Array.isArray(ids) || ids.length > 6 || new Set(ids).size !== ids.length) kit.fail(422, 'ticket_attachment_invalid', '每条消息最多 6 张图片')
    const list = (ids as unknown[]).map((value) => attachments.find((a) => a.id === value))
    if (list.some((a) => !a || a.uploader_id !== actor.id || a.ticket_id)) kit.fail(422, 'ticket_attachment_invalid', '请使用自己上传且未使用的图片')
    const result = list as StoredAttachment[]
    const refs = [...String(input.body).matchAll(/\/api\/tickets\/attachments\/(\d+)/g)]
    if (new Set([...(ids as number[]), ...refs.map((ref) => Number(ref[1]))]).size > 6) kit.fail(422, 'ticket_attachment_invalid', '每条消息最多 6 张图片')
    if (refs.some((ref) => !result.some((a) => a.id === Number(ref[1])) && !attachments.some((a) => a.id === Number(ref[1]) && a.ticket_id === id))) kit.fail(422, 'ticket_attachment_invalid', '图片不属于此工单')
    result.forEach((a) => { a.ticket_id = id }); return result
  }
  if (clean === '/tickets/attachments' && method === 'POST') {
    if (typeof input.size !== 'number' || input.size <= 0 || input.size > 5 * 1024 * 1024) kit.fail(413, 'ticket_image_size', '图片不能为空，且不能超过 5 MB')
    if (!['image/png', 'image/jpeg', 'image/gif', 'image/webp'].includes(String(input.type))) kit.fail(422, 'ticket_image_type', '只支持 PNG、JPEG、GIF 和 WebP 图片')
    const id = nextID++
    const image: StoredAttachment = { id, url: `/api/tickets/attachments/${id}`, name: String(input.name || '图片'), size: Number(input.size), content_type: String(input.type), uploader_id: actor.id, data_url: typeof input.data_url === 'string' && /^data:image\/(png|jpeg|gif|webp);base64,/.test(input.data_url) ? input.data_url : demoPNG }
    attachments.push(image); return kit.ok(image, 201)
  }
  const imageMatch = clean.match(/^\/tickets\/attachments\/(\d+)$/)
  if (imageMatch && method === 'GET') {
    const image = attachments.find((a) => a.id === Number(imageMatch[1]))
    if (!image) return kit.fail(404, 'ticket_not_found', '图片不存在')
    const owner = tickets.find((d) => d.ticket.id === image.ticket_id)?.ticket.user_id ?? image.uploader_id
    if (owner !== actor.id && actor.role !== 'admin') kit.fail(403, 'ticket_forbidden', '无权查看此图片')
    return new Response(Uint8Array.from(atob(demoPNG.split(',')[1]), (c) => c.charCodeAt(0)), { headers: { 'Content-Type': 'image/png', 'X-Content-Type-Options': 'nosniff', 'Content-Security-Policy': 'sandbox', 'Cache-Control': 'private' } })
  }
  if (clean === '/tickets/unread' && method === 'GET') return kit.ok({ count: admin ? tickets.filter((d) => d.ticket.status === 'open').length : visible.filter((d) => unread(d, false)).length })
  if (clean === '/tickets' && method === 'GET') {
    const status = query.get('status') || ''; const category = query.get('category') || ''; const q = (query.get('q') || '').toLowerCase(); const userID = query.get('user_id') || ''
    if ((status && !['open', 'replied', 'closed'].includes(status)) || (category && !['account', 'billing', 'api', 'other'].includes(category)) || (userID && (!Number.isSafeInteger(Number(userID)) || Number(userID) <= 0))) invalid()
    const filtered = visible.filter((d) => (!category || d.ticket.category === category) && (!userID || d.ticket.user_id === Number(userID)) && (!q || `${d.ticket.title} ${d.ticket.user_email} ${d.messages.map((m) => m.body).join(' ')}`.toLowerCase().includes(q)))
    const counts = { open: filtered.filter((d) => d.ticket.status === 'open').length, replied: filtered.filter((d) => d.ticket.status === 'replied').length, closed: filtered.filter((d) => d.ticket.status === 'closed').length }
    const items = filtered.filter((d) => !status || d.ticket.status === status).sort((a, b) => b.ticket.last_message_at.localeCompare(a.ticket.last_message_at)).map((d) => ({ ...d.ticket, unread: unread(d, admin) }))
    const page = Math.max(1, Number(query.get('page')) || 1); const pageSize = Math.min(100, Math.max(1, Number(query.get('page_size')) || 20))
    return kit.ok({ items: items.slice((page - 1) * pageSize, page * pageSize), total: items.length, page, page_size: pageSize, ...(admin ? { counts } : {}) })
  }
  if (clean === '/tickets' && method === 'POST') {
    checkBody()
    if (typeof input.title !== 'string' || !input.title.trim() || [...input.title.trim()].length > 100 || /[\0\r\n]/.test(input.title) || !['account', 'billing', 'api', 'other'].includes(String(input.category))) invalid()
    const id = nextID++; const messageID = nextID++; const images = bind(id); const stamp = now()
    const detail: TicketDetail = { ticket: { id, user_id: actor.id, user_email: actor.email, title: String(input.title).trim(), category: String(input.category), status: 'open', order_no: input.order_no ? String(input.order_no) : undefined, account_id: input.account_id ? Number(input.account_id) : undefined, created_at: stamp, updated_at: stamp, last_message_at: stamp, last_message_role: 'user', unread: false, user_read_message_id: messageID, admin_read_message_id: 0, user_read_at: stamp, admin_read_at: null }, messages: [{ id: messageID, ticket_id: id, author_id: actor.id, role: 'user', body: String(input.body), created_at: stamp, attachments: images }] }
    tickets.push(detail); return kit.ok(detail, 201)
  }
  const match = clean.match(/^\/tickets\/(\d+)(\/messages|\/close)?$/)
  if (!match) return undefined
  const d = tickets.find((t) => t.ticket.id === Number(match[1]))
  if (!d) return kit.fail(404, 'ticket_not_found', '工单不存在')
  if (!admin && d.ticket.user_id !== actor.id) kit.fail(403, 'ticket_forbidden', '无权查看此工单')
  if (method === 'GET' && !match[2]) {
    const last = d.messages.at(-1)?.id || 0
    if (admin) { d.ticket.admin_read_message_id = last; d.ticket.admin_read_at = now() } else { d.ticket.user_read_message_id = last; d.ticket.user_read_at = now() }
    return kit.ok({ ...d, ticket: { ...d.ticket, unread: false } })
  }
  if (method === 'POST' && match[2] === '/messages') {
    checkBody(); const images = bind(d.ticket.id); const mid = nextID++; const stamp = now()
    const message: TicketMessage = { id: mid, ticket_id: d.ticket.id, author_id: actor.id, role: admin ? 'admin' : 'user', body: String(input.body), created_at: stamp, attachments: images }
    d.messages.push(message); Object.assign(d.ticket, { status: admin ? 'replied' : 'open', last_message_role: message.role, last_message_at: stamp, updated_at: stamp })
    if (admin) { d.ticket.admin_read_message_id = mid; d.ticket.admin_read_at = stamp } else { d.ticket.user_read_message_id = mid; d.ticket.user_read_at = stamp }
    return kit.ok(d, 201)
  }
  if ((method === 'PATCH' && admin && !match[2]) || (method === 'POST' && match[2] === '/close')) {
    const status = admin && method === 'PATCH' ? String(input.status) : 'closed'
    if (!['open', 'replied', 'closed'].includes(status)) invalid()
    d.ticket.status = status; d.ticket.updated_at = now(); return kit.ok({ ticket: d.ticket })
  }
  return undefined
}
