import assert from 'node:assert/strict'
import { before, test } from 'node:test'
import { mockRequest } from './mock.ts'
import type { AdminDashboard, PoolCandidate, PoolItem, Product } from './types.ts'

before(() => {
  const storage = new Map<string, string>()
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, value: {
    getItem: (key: string) => storage.get(key) ?? null,
    setItem: (key: string, value: string) => { storage.set(key, value) },
  } })
})

async function request(path: string, method = 'GET', body?: unknown) {
  const response = await mockRequest(path, method, body)
  return { status: response.status, envelope: await response.json() }
}

test('演示总览包含四类对账告警，重查刷新检查时间', async () => {
  const initial = await request('/admin/dashboard')
  const warnings: AdminDashboard['scope_warnings'] = initial.envelope.data.scope_warnings
  assert.deepEqual(warnings.items.map(item => item.kind).sort(), ['key_drifted', 'pool_in_use', 'rental_vm_taken', 'vm_owner_conflict'])
  assert.ok(warnings.items.every(item => item.message.trim()))
  const conflict = warnings.items.find(item => item.kind === 'vm_owner_conflict')!
  assert.equal(conflict.user_email, 'user@example.com')
  for (const item of warnings.items) assert.ok(!('pool_id' in item || 'pool_name' in item))
  assert.equal((await request('/admin/scope/recheck', 'POST', {})).status, 200)
  const rechecked = await request('/admin/dashboard')
  assert.ok(Date.parse(rechecked.envelope.data.scope_warnings.checked_at) > Date.parse(warnings.checked_at))
})

test('租赁池已归属的 VM 不计入库存、不分配且移出后仍不能入池', async () => {
  const rows: PoolItem[] = (await request('/admin/rental-pool')).envelope.data.items
  const held = rows.filter(item => item.owned_by)
  assert.equal(held.length, 1)
  const [taken] = held
  assert.equal(taken.status, 'available')
  const available = rows.filter(item => item.status === 'available' && !item.owned_by).length
  assert.equal((await request('/admin/dashboard')).envelope.data.rental_pool.available, available)
  const products: Product[] = (await request('/public/products')).envelope.data.items
  assert.equal(products.find(product => product.id === 3)?.stock, available)
  const tooMany = await request('/orders', 'POST', { product_id: 3, quantity: available + 1, provider: 'mock' })
  assert.equal(tooMany.envelope.error.code, 'out_of_stock')
  assert.equal((await request('/admin/users/2/grant', 'POST', { kind: 'rental_account', quantity: available, duration_days: 1 })).status, 200)
  const after: PoolItem[] = (await request('/admin/rental-pool')).envelope.data.items
  assert.equal(after.find(item => item.vm_id === taken.vm_id)?.status, 'available')
  assert.equal((await request(`/admin/rental-pool/${taken.vm_id}`, 'DELETE')).status, 200)
  const candidates: PoolCandidate[] = (await request('/admin/rental-pool/candidates')).envelope.data.items
  assert.ok(candidates.length > 0 && candidates.every(item => item.vm_id !== taken.vm_id && !('foreign_pool' in item)))
  assert.equal((await request('/admin/rental-pool', 'POST', { vm_ids: [taken.vm_id] })).status, 409)
  assert.equal((await request('/admin/scope/recheck', 'POST', {})).status, 200)
  const warnings: AdminDashboard['scope_warnings'] = (await request('/admin/dashboard')).envelope.data.scope_warnings
  assert.ok(warnings.items.every(item => item.kind !== 'rental_vm_taken'))
})

test('普通用户创建首把门面密钥后，上游标识只出现在管理员详情', async () => {
  const credentials = { email: 'new-user@example.com', password: 'demo123456' }
  const registered = await request('/auth/register', 'POST', credentials)
  assert.equal(registered.status, 201)
  const userId = registered.envelope.data.user.id
  const login = (email: string) => request('/auth/login', 'POST', { ...credentials, email })
  assert.equal((await login('demo@example.com')).status, 200)
  const before = (await request(`/admin/users/${userId}`)).envelope.data.user
  assert.equal(before.vm2api_username, '')
  assert.equal(before.vm2api_user_id, '')

  assert.equal((await login(credentials.email)).status, 200)
  const created = await request('/keys', 'POST', { name: '首把密钥' })
  assert.equal(created.status, 201)
  assert.match(created.envelope.data.secret, /^sk-pt-/)
  const forbidden = await request(`/admin/users/${userId}`)
  assert.equal(forbidden.status, 403)
  assert.equal((await request(`/keys/${created.envelope.data.key.id}`, 'DELETE')).status, 200)
  assert.equal((await login('demo@example.com')).status, 200)
  const after = (await request(`/admin/users/${userId}`)).envelope.data.user
  assert.equal(after.vm2api_username, `portal_${userId}`)
  assert.ok(after.vm2api_user_id)

  assert.equal((await login('user@example.com')).status, 200)
  const responses = await Promise.all(['/auth/me', '/public/config', '/public/products', '/me/overview', '/me/subscriptions', '/accounts', '/accounts/5', '/keys', '/usage/summary', '/usage/logs', '/orders', '/tickets'].map(path => request(path)))
  for (const result of responses) assert.equal(result.status, 200)
  const visible = JSON.stringify([registered, created, forbidden, ...responses])
  assert.doesNotMatch(visible, /vm2api|sk-vm-|upstream_(owner|key)|portal_\d+/i)
  assert.ok(!visible.includes(after.vm2api_user_id))
})
