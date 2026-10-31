import assert from 'node:assert/strict'
import { test } from 'node:test'
import type { NormalizedSettings } from '@/lib/types'
import { normalizeSettings, validateSettings } from './settings-validation.ts'

function settings(): NormalizedSettings {
  return { site_name: '测试站点', registration_open: true, base_concurrency: 1, concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0, hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0, hosted_vm_template: {}, payment: { epay: { enabled: false, gateway_url: '', pid: '', key: '', types: [] }, manual: { enabled: false, instructions: '' } } }
}

test('保留所有设置字段并解析模板及支付方式', () => {
  const draft = settings()
  draft.payment.manual = { enabled: true, instructions: ' 请联系管理员 ' }
  const result = validateSettings(draft, '{"runtime_type":"docker","extra":{"cpu":2}}', '["alipay", " wxpay ", "alipay"]')
  assert.deepEqual(result.hosted_vm_template, { runtime_type: 'docker', extra: { cpu: 2 } })
  assert.deepEqual(result.payment.epay.types, ['alipay', 'wxpay'])
  assert.equal(result.payment.manual.instructions, '请联系管理员')
  assert.equal(result.hosted_grace_hours, 24)
  assert.equal(draft.payment.manual.instructions, ' 请联系管理员 ')
})

test('模板拒绝无效 JSON、数组、null 和原始值', () => {
  for (const value of ['{', '[]', 'null', 'true', '"docker"']) assert.throws(() => validateSettings(settings(), value, '[]'), /模板/)
})

test('支付方式必须是非空字符串数组', () => {
  for (const value of ['{', '{}', 'null', '[1]', '[""]', '[" "]']) assert.throws(() => validateSettings(settings(), '{}', value), /支付方式/)
})

test('权益与时间设置拒绝负数、小数、NaN 与不安全整数', () => {
  for (const key of ['base_concurrency', 'concurrency_per_hosted_seat', 'concurrency_per_rental_account', 'hosted_grace_hours', 'rental_grace_hours', 'gateway_queue_timeout_ms'] as const) {
    for (const value of [-1, 0.5, Number.NaN, Number.POSITIVE_INFINITY, Number.MAX_SAFE_INTEGER + 1]) {
      const draft = settings(); draft[key] = value
      assert.throws(() => validateSettings(draft, '{}', '[]'), /非负安全整数/)
    }
  }
})

test('站点名称和人工转账说明不能为空', () => {
  const draft = settings(); draft.site_name = ' '
  assert.throws(() => validateSettings(draft, '{}', '[]'), /站点名称/)
  draft.site_name = '测试站点'; draft.payment.manual.enabled = true
  assert.throws(() => validateSettings(draft, '{}', '[]'), /转账说明/)
})

test('在线支付启用前必须完整配置且不能伪造已有密钥', () => {
  const draft = settings(); draft.payment.epay.enabled = true
  assert.throws(() => validateSettings(draft, '{}', '[]'), /启用在线支付前/)
  Object.assign(draft.payment.epay, { gateway_url: 'https://pay.example.com/', pid: '1001', key: '***' })
  assert.throws(() => validateSettings(draft, '{}', '["alipay"]'), /支付密钥/)
})

test('已有密钥的 *** 和留空均保持原值语义，不覆盖其他支付字段', () => {
  for (const key of ['***', '']) {
    const draft = settings()
    draft.payment.epay = { enabled: true, gateway_url: 'https://pay.example.com/', pid: '1001', key, types: ['alipay'] }
    const result = validateSettings(draft, '{}', '["alipay"]', true)
    assert.equal(result.payment.epay.key, key)
    assert.equal(result.payment.epay.pid, '1001')
  }
})

test('新密钥按输入提交，不把掩码当作真实密钥', () => {
  const draft = settings()
  draft.payment.epay = { enabled: true, gateway_url: 'https://pay.example.com/', pid: '1001', key: 'new-secret', types: ['alipay'] }
  assert.equal(validateSettings(draft, '{}', '["alipay"]').payment.epay.key, 'new-secret')
})

test('支付网关拒绝非 HTTP 协议和带凭证 URL', () => {
  for (const gateway_url of ['not-a-url', 'javascript:alert(1)', 'ftp://example.com/', 'https://admin:secret@example.com/']) {
    const draft = settings(); draft.payment.epay.gateway_url = gateway_url
    assert.throws(() => validateSettings(draft, '{}', '[]'), /网关/)
  }
})

test('payment 缺失、为空对象或只有一个支付方时也能渲染与校验', () => {
  const fresh = { site_name: '全新部署', registration_open: true } as unknown as Parameters<typeof normalizeSettings>[0]
  const normalized = normalizeSettings(fresh)
  assert.deepEqual(normalized.payment.epay, { enabled: false, gateway_url: '', pid: '', key: '', types: ['alipay', 'wxpay'] })
  assert.deepEqual(normalized.payment.manual, { enabled: false, instructions: '' })
  assert.equal(normalized.base_concurrency, 1)
  assert.deepEqual(normalizeSettings({ payment: {} }).payment.epay.types, ['alipay', 'wxpay'])
  const partial = normalizeSettings({ payment: { epay: { enabled: true } } })
  assert.equal(partial.payment.epay.enabled, true)
  assert.equal(partial.payment.manual.enabled, false)
  const result = validateSettings({ site_name: '全新部署', hosted_vm_template: {}, base_concurrency: 1, concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0, hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0 } as never, '{}', '[]')
  assert.deepEqual(result.payment.manual, { enabled: false, instructions: '' })
})
