import assert from 'node:assert/strict'
import { test } from 'node:test'

test('演示收款方式切换保留配置、遮罩私钥，并为当面付生成二维码', async () => {
  const values = new Map<string, string>()
  Object.defineProperty(globalThis, 'sessionStorage', { configurable: true, value: {
    get length() { return values.size },
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value) },
    removeItem: (key: string) => { values.delete(key) },
    clear: () => values.clear(),
    key: (index: number) => [...values.keys()][index] ?? null,
  } satisfies Storage })
  const { mockRequest } = await import('./mock.ts')
  const request = async (path: string, method = 'GET', body?: unknown) => {
    const response = await mockRequest(path, method, body)
    return { response, envelope: await response.json() }
  }
  const saved = await request('/admin/settings', 'PUT', { payment: { active: 'alipay_f2f', alipay_f2f: { app_id: '2026111600000001', private_key: 'demo-private-key', alipay_public_key: 'demo-public-key', sandbox: true } } })
  assert.equal(saved.response.status, 200)
  assert.equal(saved.envelope.data.payment.active, 'alipay_f2f')
  assert.equal(saved.envelope.data.payment.alipay_f2f.private_key, '***')
  assert.ok(!JSON.stringify(saved.envelope).includes('demo-private-key'))
  const config = await request('/public/config')
  assert.deepEqual(config.envelope.data.payment_providers.map((provider: { id: string }) => provider.id), ['alipay_f2f', 'mock'])
  const blocked = await request('/orders', 'POST', { product_id: 1, quantity: 1, provider: 'epay' })
  assert.equal(blocked.response.status, 422)
  assert.equal(blocked.envelope.error.code, 'payment_provider_unavailable')
  const order = await request('/orders', 'POST', { product_id: 1, quantity: 1, provider: 'alipay_f2f' })
  assert.equal(order.response.status, 201)
  assert.equal(order.envelope.data.payment.type, 'qrcode')
  assert.match(order.envelope.data.payment.qr, /^https:\/\/qr\.alipay\.com\/demo/)
  const switched = await request('/admin/settings', 'PUT', { payment: { active: 'manual', epay: { gateway_url: '' }, alipay_f2f: { private_key: '***' } } })
  assert.equal(switched.response.status, 200)
  assert.equal(switched.envelope.data.payment.alipay_f2f.private_key, '***')
  assert.equal(switched.envelope.data.payment.alipay_f2f.alipay_public_key, 'demo-public-key')
  const restored = await request('/admin/settings', 'PUT', { payment: { active: 'alipay_f2f', alipay_f2f: { private_key: '' } } })
  assert.equal(restored.response.status, 200)
  assert.equal(restored.envelope.data.payment.alipay_f2f.sandbox, true)
})
