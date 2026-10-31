import type { Settings } from '@/lib/types'

export const numericSettings = [
  { key: 'base_concurrency', label: '基础并发', hint: '每位用户默认可用的并发数量。' },
  { key: 'concurrency_per_hosted_seat', label: '每个托管席位附加并发', hint: '按用户已购买的托管席位计算。' },
  { key: 'concurrency_per_rental_account', label: '每个租赁账号附加并发', hint: '按用户已购买的租赁账号权益计算。' },
  { key: 'hosted_grace_hours', label: '托管宽限期（小时）', hint: '订阅到期后，托管资源保留的宽限时间。' },
  { key: 'rental_grace_hours', label: '租赁宽限期（小时）', hint: '订阅到期后，租赁资源保留的宽限时间。' },
  { key: 'gateway_queue_timeout_ms', label: '网关排队超时（毫秒）', hint: '0 表示不等待；数值必须为非负整数。' },
] as const

export function validateSettings(draft: Settings, templateJSON: string, typesJSON: string, hasExistingKey = false): Settings {
  if (!draft.site_name.trim()) throw new Error('站点名称不能为空。')
  for (const field of numericSettings) {
    if (!Number.isSafeInteger(draft[field.key]) || draft[field.key] < 0) throw new Error(`${field.label}必须为非负安全整数。`)
  }
  let template: unknown
  try { template = JSON.parse(templateJSON) } catch { throw new Error('托管 VM 模板不是有效 JSON，请检查引号、逗号和括号。') }
  if (typeof template !== 'object' || template === null || Array.isArray(template)) throw new Error('托管 VM 模板必须为 JSON 对象，不能是数组或空值。')
  let types: unknown
  try { types = JSON.parse(typesJSON) } catch { throw new Error('支付方式必须是有效的 JSON 字符串数组。') }
  if (!Array.isArray(types) || !types.every((value): value is string => typeof value === 'string' && value.trim().length > 0)) throw new Error('支付方式必须是非空字符串组成的 JSON 数组。')
  const normalizedTypes = [...new Set(types.map((value: string) => value.trim()))]
  const epay = draft.payment.epay
  const gateway = epay.gateway_url.trim()
  if (gateway) {
    let url: URL
    try { url = new URL(gateway) } catch { throw new Error('支付网关地址不是有效 URL。') }
    if (!['https:', 'http:'].includes(url.protocol) || url.username || url.password) throw new Error('支付网关仅支持 HTTP / HTTPS 地址，且不能包含用户名或密码。')
  }
  const key = epay.key.trim()
  const hasKey = key && key !== '***' || hasExistingKey
  if (epay.enabled && (!gateway || !epay.pid.trim() || !hasKey || !normalizedTypes.length)) throw new Error('启用在线支付前，请填写网关地址、商户编号、支付密钥并选择至少一种支付方式。')
  if (draft.payment.manual.enabled && !draft.payment.manual.instructions.trim()) throw new Error('启用人工转账前，请填写转账说明。')
  return {
    ...draft,
    site_name: draft.site_name.trim(),
    hosted_vm_template: template as Record<string, unknown>,
    payment: {
      epay: { ...epay, gateway_url: gateway, pid: epay.pid.trim(), key: epay.key.trim(), types: normalizedTypes },
      manual: { ...draft.payment.manual, instructions: draft.payment.manual.instructions.trim() },
    },
  }
}
