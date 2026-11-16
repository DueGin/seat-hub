import assert from 'node:assert/strict'
import { test } from 'node:test'
import { reservationCSV, reservationEmail, reservationMock, setReservationProducts } from './mock-reservations.ts'
import type { MockKit } from './mock-kit.ts'
import type { Product, User } from './types.ts'

const admin: User = { id: 1, email: 'admin@example.com', role: 'admin', status: 'active', created_at: new Date().toISOString() }
let user: User | null = null
const kit: MockKit = {
  ok: (data = {}, status = 200) => new Response(JSON.stringify({ ok: true, data }), { status }),
  fail: (status, code, message) => { throw Object.assign(new Error(message), { status, code }) },
  currentUser: () => user ?? kit.fail(401, 'unauthorized', '请先登录'), optionalUser: () => user,
  users: () => [admin], iso: () => new Date().toISOString(),
}
const products: Product[] = [{ id: 777, kind: 'rental_account', name: '预约测试', description: '', unit_quantity: 1, duration_days: 30, price_cents: 100, currency: 'CNY', min_qty: 1, max_qty: 5, enabled: true, sort_order: 0, badge: '', stock: 0, reservation_only: true }]
async function request(path: string, method = 'GET', input = {}) {
  const url = new URL(path, 'https://test.example.com')
  const response = await reservationMock(url.pathname, method, url.searchParams, input, kit)
  assert.ok(response)
  return response
}

test('邮箱统一为小写，CSV 使用 BOM 并保护公式、引号和换行', () => {
  assert.equal(reservationEmail(' User@Example.COM '), 'user@example.com')
  const csv = reservationCSV([['备注', '邮箱'], ['=HYPERLINK("bad")', 'u@example.com'], [' \t+SUM(A1)', '多行\n备注'], ['\t危险', '安全']])
  assert.ok(csv.startsWith('\ufeff'))
  assert.ok(csv.includes('"\'=HYPERLINK(""bad"")"'))
  assert.ok(csv.includes('"\' \t+SUM(A1)"'))
  assert.ok(csv.includes('"多行\n备注"'))
  assert.ok(csv.includes('"\'\t危险"'))
})

test('公开预约、去重、权限、通知和导出演示流程', async () => {
  setReservationProducts(() => products)
  user = null
  await request('/public/products/777/reservations', 'POST', { email: ' One@Example.com ', note: '=1+1' })
  await request('/public/products/777/reservations', 'POST', { email: 'one@example.com', note: '不覆盖' })
  await assert.rejects(() => request('/admin/reservations'), { code: 'unauthorized' })
  user = { ...admin, role: 'user' }
  await assert.rejects(() => request('/admin/reservations'), { code: 'forbidden' })
  user = admin
  const list = await (await request('/admin/reservations?product_id=777')).json()
  assert.equal(list.data.total, 1)
  assert.equal(list.data.items[0].note, '=1+1')
  assert.equal(list.data.items[0].user_id, null)
  assert.equal(list.data.counts.find((count: { product_id: number }) => count.product_id === 777).waiting, 1)
  await assert.rejects(() => request('/admin/products/777/reservations/notify', 'POST', { subject: '开售', body: '欢迎' }), { code: 'mail_not_configured' })
  const config = { enabled: true, host: 'smtp.example.com', port: 465, security: 'tls', username: 'sender', password: 'test-secret', from_address: 'sender@example.com', from_name: '测试', admin_emails: [] }
  const saved = await (await request('/admin/settings/mail', 'PUT', config)).json()
  assert.equal(saved.data.password, '***')
  assert.ok(!JSON.stringify(saved).includes('test-secret'))
  await request('/admin/settings/mail', 'PUT', { ...config, password: '***' })
  await request('/admin/settings/mail/test', 'POST', { to: 'receiver@example.com' })
  const sent = await (await request('/admin/products/777/reservations/notify', 'POST', { subject: '开售', body: '欢迎' })).json()
  assert.equal(sent.data.count, 1)
  const again = await (await request('/admin/products/777/reservations/notify', 'POST', { subject: '开售', body: '欢迎' })).json()
  assert.equal(again.data.count, 0)
  const csvResponse = await request('/admin/reservations/export?product_id=777')
  const bytes = new Uint8Array(await csvResponse.arrayBuffer())
  assert.deepEqual([...bytes.slice(0, 3)], [239, 187, 191])
  assert.ok(new TextDecoder().decode(bytes).includes("'=1+1"))
  await request(`/admin/reservations/${list.data.items[0].id}`, 'DELETE')
  await assert.rejects(() => request(`/admin/reservations/${list.data.items[0].id}`, 'DELETE'), { code: 'reservation_not_found' })
  products[0].reservation_only = false
  await assert.rejects(() => request('/public/products/777/reservations', 'POST', { email: 'two@example.com' }), { code: 'product_not_reservable' })
})
