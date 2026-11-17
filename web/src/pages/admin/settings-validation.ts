import { normalizeRegistrationWhitelist } from '../../lib/registration-email.ts'
import type { NormalizedSettings, Settings } from '@/lib/types'

export const numericSettings = [
  { key: 'base_concurrency', label: '基础并发', hint: '每位用户默认可用的并发数量。' },
  { key: 'concurrency_per_hosted_seat', label: '每个托管席位附加并发', hint: '按用户已购买的托管席位计算。' },
  { key: 'concurrency_per_rental_account', label: '每个租赁账号附加并发', hint: '按用户已购买的租赁账号权益计算。' },
  { key: 'hosted_grace_hours', label: '托管宽限期（小时）', hint: '订阅到期后，托管资源保留的宽限时间。' },
  { key: 'rental_grace_hours', label: '租赁宽限期（小时）', hint: '订阅到期后，租赁资源保留的宽限时间。' },
  { key: 'gateway_queue_timeout_ms', label: '网关排队超时（毫秒）', hint: '0 表示不等待；数值必须为非负整数。' },
] as const

export const selfcheckNumericSettings = [
  { key: 'selfcheck_max_rebuilds', label: '最多重建次数', hint: '自检失败后的自动重建上限，范围为 0–10 次。', min: 0, max: 10 },
  { key: 'selfcheck_recheck_minutes', label: '复检间隔分钟', hint: '后台巡检与授权前复检间隔，范围为 5–1440 分钟。', min: 5, max: 1440 },
] as const

const DEFAULT_EPAY = { enabled: false, gateway_url: '', pid: '', key: '', types: ['alipay', 'wxpay'] }
const DEFAULT_MANUAL = { enabled: true, instructions: '转账后请联系管理员确认收款。' }
const DEFAULT_NUMBERS = { base_concurrency: 1, concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0, hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0 }
const DEFAULT_SELFCHECK = { selfcheck_require_dedicated_proxy: true, selfcheck_require_timezone_match: true, selfcheck_max_rebuilds: 3, selfcheck_recheck_minutes: 30 }

function record(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value) ? value as Record<string, unknown> : {}
}
function numberOr(value: unknown, fallback: number): number { return typeof value === 'number' ? value : fallback }
function stringOr(value: unknown, fallback: string): string { return typeof value === 'string' ? value : fallback }
function booleanOr(value: unknown, fallback: boolean): boolean { return typeof value === 'boolean' ? value : fallback }

// normalizeSettings tolerates any subset of the admin settings document. A fresh install or
// an older stored row may omit payment entirely (or return {}, or only one provider block),
// and the editor must still render: every missing field falls back to the contract default.
export function normalizeSettings(input: Settings | null | undefined): NormalizedSettings {
  const source = record(input)
  const payment = record(source.payment)
  const epay = record(payment.epay)
  const manual = record(payment.manual)
  const alipay = record(payment.alipay_f2f)
  const types = Array.isArray(epay.types) ? epay.types.filter((value): value is string => typeof value === 'string') : DEFAULT_EPAY.types
  return {
    site_name: stringOr(source.site_name, ''),
    registration_open: booleanOr(source.registration_open, true),
    register_email_verification: booleanOr(source.register_email_verification, false),
    register_email_whitelist: Array.isArray(source.register_email_whitelist) ? source.register_email_whitelist.filter((value): value is string => typeof value === 'string') : [],
    base_concurrency: numberOr(source.base_concurrency, DEFAULT_NUMBERS.base_concurrency),
    concurrency_per_hosted_seat: numberOr(source.concurrency_per_hosted_seat, DEFAULT_NUMBERS.concurrency_per_hosted_seat),
    concurrency_per_rental_account: numberOr(source.concurrency_per_rental_account, DEFAULT_NUMBERS.concurrency_per_rental_account),
    hosted_grace_hours: numberOr(source.hosted_grace_hours, DEFAULT_NUMBERS.hosted_grace_hours),
    rental_grace_hours: numberOr(source.rental_grace_hours, DEFAULT_NUMBERS.rental_grace_hours),
    gateway_queue_timeout_ms: numberOr(source.gateway_queue_timeout_ms, DEFAULT_NUMBERS.gateway_queue_timeout_ms),
    selfcheck_require_dedicated_proxy: booleanOr(source.selfcheck_require_dedicated_proxy, DEFAULT_SELFCHECK.selfcheck_require_dedicated_proxy),
    selfcheck_require_timezone_match: booleanOr(source.selfcheck_require_timezone_match, DEFAULT_SELFCHECK.selfcheck_require_timezone_match),
    selfcheck_max_rebuilds: numberOr(source.selfcheck_max_rebuilds, DEFAULT_SELFCHECK.selfcheck_max_rebuilds),
    selfcheck_recheck_minutes: numberOr(source.selfcheck_recheck_minutes, DEFAULT_SELFCHECK.selfcheck_recheck_minutes),
    hosted_vm_template: Object.keys(record(source.hosted_vm_template)).length ? record(source.hosted_vm_template) : {},
    payment: {
      active: payment.active === 'epay' || payment.active === 'alipay_f2f' || payment.active === 'manual' ? payment.active
        : epay.enabled === true && stringOr(epay.gateway_url, '').trim() && stringOr(epay.pid, '').trim() && stringOr(epay.key, '').trim() ? 'epay' : 'manual',
      alipay_f2f: { app_id: stringOr(alipay.app_id, ''), private_key: stringOr(alipay.private_key, ''), alipay_public_key: stringOr(alipay.alipay_public_key, ''), sandbox: booleanOr(alipay.sandbox, false) },
      epay: {
        enabled: booleanOr(epay.enabled, DEFAULT_EPAY.enabled),
        gateway_url: stringOr(epay.gateway_url, DEFAULT_EPAY.gateway_url),
        pid: stringOr(epay.pid, DEFAULT_EPAY.pid),
        key: stringOr(epay.key, DEFAULT_EPAY.key),
        types,
      },
      manual: {
        enabled: booleanOr(manual.enabled, DEFAULT_MANUAL.enabled),
        instructions: stringOr(manual.instructions, DEFAULT_MANUAL.instructions),
      },
    },
  }
}

export function validateSettings(draft: Settings, templateJSON: string, typesJSON: string, hasExistingKey = false, hasExistingPrivateKey = false): NormalizedSettings {
  const settings = normalizeSettings(draft)
  if (!settings.site_name.trim()) throw new Error('站点名称不能为空。')
  for (const field of numericSettings) {
    if (!Number.isSafeInteger(settings[field.key]) || settings[field.key] < 0) throw new Error(`${field.label}必须为非负安全整数。`)
  }
  for (const field of selfcheckNumericSettings) {
    const value = settings[field.key]
    if (!Number.isSafeInteger(value) || value < field.min || value > field.max) throw new Error(`${field.label}必须为 ${field.min}–${field.max} 之间的整数。`)
  }
  let template: unknown
  try { template = JSON.parse(templateJSON) } catch { throw new Error('托管 VM 模板不是有效 JSON，请检查引号、逗号和括号。') }
  if (typeof template !== 'object' || template === null || Array.isArray(template)) throw new Error('托管 VM 模板必须为 JSON 对象，不能是数组或空值。')
  let types: unknown
  try { types = JSON.parse(typesJSON) } catch { throw new Error('支付方式必须是有效的 JSON 字符串数组。') }
  if (!Array.isArray(types) || !types.every((value): value is string => typeof value === 'string' && value.trim().length > 0)) throw new Error('支付方式必须是非空字符串组成的 JSON 数组。')
  const normalizedTypes = [...new Set(types.map((value: string) => value.trim()))]
  const epay = settings.payment.epay
  const gateway = epay.gateway_url.trim()
  if (gateway && settings.payment.active === 'epay') {
    let url: URL
    try { url = new URL(gateway) } catch { throw new Error('支付网关地址不是有效 URL。') }
    if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password) throw new Error('支付网关仅支持 HTTP / HTTPS 地址，且不能包含用户名或密码。')
  }
  const key = epay.key.trim()
  const hasKey = key && key !== '***' || hasExistingKey
  if (settings.payment.active === 'epay' && (!gateway || !epay.pid.trim() || !hasKey || !normalizedTypes.length)) throw new Error('启用在线支付前，请填写网关地址、商户编号、支付密钥并选择至少一种支付方式。')
  if (settings.payment.active === 'manual' && !settings.payment.manual.instructions.trim()) throw new Error('启用人工转账前，请填写转账说明。')
  const alipay = settings.payment.alipay_f2f
  if (settings.payment.active === 'alipay_f2f') {
    if (!/^\d+$/.test(alipay.app_id.trim())) throw new Error('请填写有效的支付宝当面付 AppID，只能包含数字。')
    if ((!alipay.private_key.trim() || alipay.private_key.trim() === '***') && !hasExistingPrivateKey) throw new Error('请填写支付宝当面付应用私钥。')
    if (!alipay.alipay_public_key.trim() || alipay.alipay_public_key.trim() === '***') throw new Error('请填写支付宝当面付支付宝公钥。')
  }
  return {
    ...settings,
    site_name: settings.site_name.trim(),
    register_email_whitelist: normalizeRegistrationWhitelist(settings.register_email_whitelist),
    hosted_vm_template: template as Record<string, unknown>,
    payment: {
      active: settings.payment.active,
      alipay_f2f: { ...alipay, app_id: alipay.app_id.trim(), private_key: alipay.private_key.trim(), alipay_public_key: alipay.alipay_public_key.trim() },
      epay: { ...epay, gateway_url: gateway, pid: epay.pid.trim(), key: epay.key.trim(), types: normalizedTypes },
      manual: { ...settings.payment.manual, instructions: settings.payment.manual.instructions.trim() },
    },
  }
}
