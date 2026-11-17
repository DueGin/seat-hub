import assert from 'node:assert/strict'
import { test } from 'node:test'
import type { NormalizedSettings } from '@/lib/types'
import { normalizeSettings, validateSettings } from './settings-validation.ts'

function settings(): NormalizedSettings {
  return { site_name: '测试站点', registration_open: true, register_email_verification: false, register_email_whitelist: [], base_concurrency: 1, concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0, hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0, selfcheck_require_dedicated_proxy: true, selfcheck_require_timezone_match: true, selfcheck_max_rebuilds: 3, selfcheck_recheck_minutes: 30, hosted_vm_template: {}, payment: { active: 'manual', alipay_f2f: { app_id: '', private_key: '', alipay_public_key: '', sandbox: false }, epay: { enabled: false, gateway_url: '', pid: '', key: '', types: [] }, manual: { enabled: true, instructions: '转账后请联系管理员确认收款。' } } }
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
  draft.site_name = '测试站点'; draft.payment.manual.instructions = ''
  assert.throws(() => validateSettings(draft, '{}', '[]'), /转账说明/)
})

test('在线支付启用前必须完整配置且不能伪造已有密钥', () => {
  const draft = settings(); draft.payment.active = 'epay'; draft.payment.epay.enabled = true
  assert.throws(() => validateSettings(draft, '{}', '[]'), /启用在线支付前/)
  Object.assign(draft.payment.epay, { gateway_url: 'https://pay.example.com/', pid: '1001', key: '***' })
  assert.throws(() => validateSettings(draft, '{}', '["alipay"]'), /支付密钥/)
})

test('已有密钥的 *** 和留空均保持原值语义，不覆盖其他支付字段', () => {
  for (const key of ['***', '']) {
    const draft = settings(); draft.payment.active = 'epay'
    draft.payment.epay = { enabled: true, gateway_url: 'https://pay.example.com/', pid: '1001', key, types: ['alipay'] }
    const result = validateSettings(draft, '{}', '["alipay"]', true)
    assert.equal(result.payment.epay.key, key)
    assert.equal(result.payment.epay.pid, '1001')
  }
})

test('新密钥按输入提交，不把掩码当作真实密钥', () => {
  const draft = settings(); draft.payment.active = 'epay'
  draft.payment.epay = { enabled: true, gateway_url: 'https://pay.example.com/', pid: '1001', key: 'new-secret', types: ['alipay'] }
  assert.equal(validateSettings(draft, '{}', '["alipay"]').payment.epay.key, 'new-secret')
})

test('支付网关拒绝非 HTTP 协议和带凭证 URL', () => {
  for (const gateway_url of ['not-a-url', 'javascript:alert(1)', 'ftp://example.com/', 'https://admin:secret@example.com/']) {
    const draft = settings(); draft.payment.active = 'epay'; draft.payment.epay.gateway_url = gateway_url
    assert.throws(() => validateSettings(draft, '{}', '[]'), /网关/)
  }
})

test('payment 缺失、为空对象或只有一个支付方时也能渲染与校验', () => {
  const fresh = { site_name: '全新部署', registration_open: true } as unknown as Parameters<typeof normalizeSettings>[0]
  const normalized = normalizeSettings(fresh)
  assert.deepEqual(normalized.payment.epay, { enabled: false, gateway_url: '', pid: '', key: '', types: ['alipay', 'wxpay'] })
  assert.deepEqual(normalized.payment.manual, { enabled: true, instructions: '转账后请联系管理员确认收款。' })
  assert.equal(normalized.base_concurrency, 1)
  assert.deepEqual(normalizeSettings({ payment: {} }).payment.epay.types, ['alipay', 'wxpay'])
  const partial = normalizeSettings({ payment: { epay: { enabled: true } } })
  assert.equal(partial.payment.epay.enabled, true)
  assert.equal(partial.payment.manual.enabled, true)
  const result = validateSettings({ site_name: '全新部署', hosted_vm_template: {}, base_concurrency: 1, concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0, hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0 } as never, '{}', '[]')
  assert.deepEqual(result.payment.manual, { enabled: true, instructions: '转账后请联系管理员确认收款。' })
  assert.equal(result.selfcheck_require_dedicated_proxy, true)
  assert.equal(result.selfcheck_require_timezone_match, true)
  assert.equal(result.selfcheck_max_rebuilds, 3)
  assert.equal(result.selfcheck_recheck_minutes, 30)
})

test('自检设置校验重建次数与复检间隔边界', () => {
  for (const value of [-1, 11, 0.5, Number.NaN]) {
    const draft = settings(); draft.selfcheck_max_rebuilds = value
    assert.throws(() => validateSettings(draft, '{}', '[]'), /最多重建次数/)
  }
  for (const value of [4, 1441, 5.5, Number.NaN]) {
    const draft = settings(); draft.selfcheck_recheck_minutes = value
    assert.throws(() => validateSettings(draft, '{}', '[]'), /复检间隔分钟/)
  }
  const boundary = settings(); boundary.selfcheck_max_rebuilds = 0; boundary.selfcheck_recheck_minutes = 1440
  assert.equal(validateSettings(boundary, '{}', '[]').selfcheck_max_rebuilds, 0)
})


test('旧配置迁移只选择已启用且完整的易支付，否则选择人工转账', () => {
  assert.equal(normalizeSettings({ payment: { epay: { enabled: true } } }).payment.active, 'manual')
  assert.equal(normalizeSettings({ payment: { epay: { enabled: true, gateway_url: 'https://pay.example.com', pid: '1001', key: '***' } } }).payment.active, 'epay')
  assert.equal(normalizeSettings({ payment: { active: 'manual', epay: { enabled: true, gateway_url: 'https://pay.example.com', pid: '1001', key: '***' } } }).payment.active, 'manual')
})

test('仅当前收款方式必须完整，切换保留其他方式的配置', () => {
  const draft = settings()
  draft.payment.epay = { enabled: true, gateway_url: 'invalid', pid: '', key: '', types: [] }
  draft.payment.alipay_f2f = { app_id: '', private_key: '', alipay_public_key: '', sandbox: true }
  const result = validateSettings(draft, '{}', '[]')
  assert.equal(result.payment.active, 'manual')
  assert.equal(result.payment.epay.gateway_url, 'invalid')
  assert.equal(result.payment.alipay_f2f.sandbox, true)
})

test('当面付校验 AppID、应用私钥和支付宝公钥，已有私钥支持掩码和留空', () => {
  const draft = settings(); draft.payment.active = 'alipay_f2f'
  assert.throws(() => validateSettings(draft, '{}', '[]'), /AppID/)
  draft.payment.alipay_f2f.app_id = '202611160001'
  assert.throws(() => validateSettings(draft, '{}', '[]'), /应用私钥/)
  draft.payment.alipay_f2f.private_key = '***'
  assert.throws(() => validateSettings(draft, '{}', '[]'), /应用私钥/)
  assert.throws(() => validateSettings(draft, '{}', '[]', false, true), /支付宝公钥/)
  draft.payment.alipay_f2f.alipay_public_key = 'public-key'
  for (const private_key of ['***', '']) {
    draft.payment.alipay_f2f.private_key = private_key
    assert.equal(validateSettings(draft, '{}', '[]', false, true).payment.alipay_f2f.private_key, private_key)
  }
})

test('注册验证默认关闭，白名单规范化并保留验证开关', () => {
  assert.equal(normalizeSettings({}).register_email_verification, false)
  assert.deepEqual(normalizeSettings({}).register_email_whitelist, [])
  const draft = settings(); draft.register_email_verification = true; draft.register_email_whitelist = [' QQ.com ', '', 'qq.COM', '*.edu.cn', 'Someone@gmail.com']
  const result = validateSettings(draft, '{}', '[]')
  assert.equal(result.register_email_verification, true)
  assert.deepEqual(result.register_email_whitelist, ['qq.com', '*.edu.cn', 'someone@gmail.com'])
  for (const entry of ['localhost', '*qq.com', 'a.*.com', '-a.com', 'a..com', 'a_b.com', 'Name <a@gmail.com>', 'a@gmail.com,b@gmail.com']) {
    draft.register_email_whitelist = [entry]
    assert.throws(() => validateSettings(draft, '{}', '[]'), /白名单/)
  }
  draft.register_email_whitelist = Array.from({ length: 201 }, (_, i) => `d${i}.example.com`)
  assert.throws(() => validateSettings(draft, '{}', '[]'), /200/)
})
