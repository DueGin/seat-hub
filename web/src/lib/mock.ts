import type {
  ReleaseInfo, UpdateJob, UpdateStage, UpdateStatus,
  Account, AccountUsage, AdminDashboard, AdminUser, ApiKey, DailyUsage, Job,
  ModelUsage, Onboarding, Order, Overview, Page, Payment, PoolCandidate,
  PoolItem, Product, ProductKind, PublicConfig, Settings, Spend, Subscription,
  UsageLog, UsageSummary, User, NormalizedSettings, Selfcheck, SelfcheckCheck,
  SelfcheckReport,
} from './types'

type Owned<T> = { userId: number; value: T }
type Input = Record<string, unknown>
const DAY = 86_400_000
const startedAt = Date.now()
const today = Date.parse(new Date(startedAt).toISOString().slice(0, 10) + 'T00:00:00Z')
const iso = (offset = 0) => new Date(startedAt + offset).toISOString()
const money = (value: number) => Math.round(value * 1_000_000) / 1_000_000
const sessionKey = 'vm2api.mock.session.v1'
let sequence = 100
const nextId = () => ++sequence
const users: User[] = [
  { id: 1, email: 'demo@example.com', role: 'admin', status: 'active', created_at: iso(-90 * DAY) },
  { id: 2, email: 'user@example.com', role: 'user', status: 'active', created_at: iso(-40 * DAY) },
  { id: 3, email: 'disabled@example.com', role: 'user', status: 'disabled', created_at: iso(-35 * DAY) },
]
const passwords = new Map<number, string>()
const loginAttempts = new Map<string, { count: number; until: number }>()

function readSession(): number | null {
  try {
    const stored = sessionStorage.getItem(sessionKey)
    if (stored === null) return 1
    const parsed: unknown = JSON.parse(stored)
    return typeof parsed === 'number' && Number.isSafeInteger(parsed) ? parsed : null
  } catch { return 1 }
}
let sessionUserId = readSession()
function setSession(id: number | null) {
  sessionUserId = id
  try { sessionStorage.setItem(sessionKey, JSON.stringify(id)) } catch {}
}

let settings: NormalizedSettings = {
  site_name: 'AI 工作台 演示环境', registration_open: true, base_concurrency: 1,
  concurrency_per_hosted_seat: 0, concurrency_per_rental_account: 0,
  hosted_grace_hours: 24, rental_grace_hours: 0, gateway_queue_timeout_ms: 0,
  hosted_vm_template: { runtime_type: 'docker' },
  selfcheck_require_dedicated_proxy: true, selfcheck_require_timezone_match: true,
  selfcheck_max_rebuilds: 3, selfcheck_recheck_minutes: 30,
  payment: {
    epay: { enabled: false, gateway_url: 'https://payment.invalid.example/', pid: '', key: '', types: ['alipay', 'wxpay'] },
    manual: { enabled: true, instructions: '演示环境，无需真实转账；管理员可在订单中模拟确认收款。' },
  },
}
let epayKeyConfigured = false
const products: Product[] = [
  { id: 1, kind: 'concurrency', name: '5 路并发 · 30 天', description: '为工作流增加 5 路独立并发。', unit_quantity: 5, duration_days: 30, price_cents: 9900, currency: 'CNY', min_qty: 1, max_qty: 20, enabled: true, sort_order: 0, badge: '热销', stock: null },
  { id: 2, kind: 'hosted_seat', name: '托管席位 · 30 天', description: '使用你自己的账号，通过 OAuth 安全授权。', unit_quantity: 1, duration_days: 30, price_cents: 4900, currency: 'CNY', min_qty: 1, max_qty: 10, enabled: true, sort_order: 1, badge: '自有账号', stock: null },
  { id: 3, kind: 'rental_account', name: 'Max 成品账号 · 30 天', description: '即开即用的独享租赁账号。', unit_quantity: 1, duration_days: 30, price_cents: 19900, currency: 'CNY', min_qty: 1, max_qty: 5, enabled: true, sort_order: 2, badge: '限量', stock: 0 },
  { id: 4, kind: 'concurrency', name: '1 路并发 · 90 天', description: '适合长期稳定使用。', unit_quantity: 1, duration_days: 90, price_cents: 5900, currency: 'CNY', min_qty: 1, max_qty: 50, enabled: true, sort_order: 3, badge: '', stock: null },
  { id: 5, kind: 'hosted_seat', name: '双席位 · 90 天', description: '团队托管方案，暂未上架。', unit_quantity: 2, duration_days: 90, price_cents: 24900, currency: 'CNY', min_qty: 1, max_qty: 10, enabled: false, sort_order: 4, badge: '', stock: null },
]
const subscriptions: Owned<Subscription>[] = [
  { userId: 1, value: { id: 1, kind: 'concurrency', quantity: 5, status: 'active', starts_at: iso(-27 * DAY), expires_at: iso(3 * DAY), source_order_no: 'PT202610010001', renewable: true, renew_product_ids: [1, 4] } },
  { userId: 1, value: { id: 2, kind: 'hosted_seat', quantity: 4, status: 'active', starts_at: iso(-12 * DAY), expires_at: iso(18 * DAY), source_order_no: 'PT202610020001', renewable: true, renew_product_ids: [2] } },
  { userId: 1, value: { id: 3, kind: 'rental_account', quantity: 1, status: 'active', starts_at: iso(-24 * DAY), expires_at: iso(6 * DAY), source_order_no: 'PT202610030001', renewable: true, renew_product_ids: [3] } },
  { userId: 1, value: { id: 4, kind: 'concurrency', quantity: 5, status: 'expired', starts_at: iso(-65 * DAY), expires_at: iso(-35 * DAY), source_order_no: 'PT202608010001', renewable: true, renew_product_ids: [1, 4] } },
  { userId: 2, value: { id: 5, kind: 'hosted_seat', quantity: 1, status: 'active', starts_at: iso(-5 * DAY), expires_at: iso(25 * DAY), source_order_no: 'PT202610040001', renewable: true, renew_product_ids: [2] } },
]

function spend(requests = 0, tokens = 0, cost = 0): Spend {
  const input = Math.floor(tokens * 0.3)
  const output = Math.floor(tokens * 0.12)
  const cacheRead = Math.floor(tokens * 0.48)
  return { requests, tokens, input_tokens: input, output_tokens: output, cache_read_tokens: cacheRead, cache_creation_tokens: tokens - input - output - cacheRead, cost_usd: money(cost) }
}
function accountUsage(seed: number): AccountUsage {
  const five = 12.5 + seed * 7
  const seven = 28 + seed * 9
  return {
    available: true, schedule_state: 'on', restriction_text: null,
    five_hour: { utilization: five, resets_at: iso(2 * 3_600_000), status: 'ok' },
    seven_day: { utilization: seven, resets_at: iso(3 * DAY), status: 'ok' },
    seven_day_fable: null,
    estimate_5h: { status: 'ok', spent_usd: money(five * 0.12), total_usd: 12, remaining_usd: money(12 - five * 0.12), used_ratio: five / 100 },
    estimate_7d: { status: 'ok', spent_usd: money(seven * 0.6), total_usd: 60, remaining_usd: money(60 - seven * 0.6), used_ratio: seven / 100 },
    today: spend(42 + seed * 9, 840_000 + seed * 51_000, 1.42 + seed * 0.21),
    window_5h: spend(18 + seed, 240_000 + seed * 18_000, five * 0.12),
    window_7d: spend(260 + seed * 20, 4_200_000 + seed * 155_000, seven * 0.6),
    seats_used: 1, seats_max: 4, inflight: seed % 2, fetched_at: iso(),
  }
}
function passingChecks(): SelfcheckCheck[] {
  return [
    { code: 'proxy_bound', ok: true, message: '已绑定独享 SOCKS5 出口' },
    { code: 'proxy_healthy', ok: true, message: '出口代理运行正常' },
    { code: 'exit_ip_unique', ok: true, message: '出口 IP 未与其他槽位重复' },
    { code: 'fingerprint_unique', ok: true, message: '设备指纹未与其他槽位重复' },
  ]
}
function selfcheck(state: Selfcheck['state'], attempts = 0): Selfcheck {
  const checks = passingChecks()
  if (state === 'fail') checks[2] = { code: 'exit_ip_unique', ok: false, message: '出口 IP 与其他槽位重复', detail: '203.0.113.24 同时分配给 demo-hosted-1', remedy: '重建运行环境并重新分配出口。' }
  if (state === 'exhausted') checks[1] = { code: 'proxy_healthy', ok: false, message: '出口代理健康检查失败', detail: 'proxy status=dead', remedy: '请管理员更换可用代理后重新自检。' }
  if (state === 'pending') return { state, attempts, max_attempts: settings.selfcheck_max_rebuilds, checked_at: null, checks: [] }
  return { state, attempts, max_attempts: settings.selfcheck_max_rebuilds, checked_at: iso(-60_000), checks }
}
function duplicateSelfcheck(): Selfcheck {
  return { ...selfcheck('fail'), checks: [...passingChecks(), { code: 'account_unique', ok: false, message: '该账号已在其它槽位托管', detail: 'account_uuid 与资源 #1 重复', remedy: '请移除此账号，或使用尚未托管的账号。' }] }
}
const accounts: Owned<Account>[] = [
  { userId: 1, value: { id: 1, kind: 'hosted', label: '日常开发主号', email_masked: 'dev***@***.com', status: 'active', status_reason: null, tier: 'max', created_at: iso(-12 * DAY), expires_at: null, usage: accountUsage(1), selfcheck: selfcheck('pass') } },
  { userId: 1, value: { id: 2, kind: 'hosted', label: '设计工作室（重建中）', email_masked: 'stu***@***.com', status: 'needs_reauth', status_reason: '环境自检未通过，正在第 2 次重建。', tier: 'pro', created_at: iso(-10 * DAY), expires_at: null, usage: { ...accountUsage(2), available: false, schedule_state: 'unschedulable', restriction_text: '需要重新授权', five_hour: null, seven_day: null, estimate_5h: null, estimate_7d: null, inflight: 0 }, selfcheck: selfcheck('fail', 2) } },
  { userId: 1, value: { id: 3, kind: 'hosted', label: '备用号（转人工）', email_masked: 'bak***@***.com', status: 'failed', status_reason: '多次自检未通过，已转人工处理。', tier: 'pro', created_at: iso(-8 * DAY), expires_at: null, usage: null, selfcheck: selfcheck('exhausted', 3) } },
  { userId: 1, value: { id: 4, kind: 'rental', label: 'Max 独享租赁', email_masked: 'ren***@***.com', status: 'active', status_reason: null, tier: 'max', created_at: iso(-24 * DAY), expires_at: iso(6 * DAY), usage: accountUsage(2), selfcheck: selfcheck('pass') } },
  { userId: 2, value: { id: 5, kind: 'hosted', label: '个人工作账号', email_masked: 'use***@***.com', status: 'active', status_reason: null, tier: 'pro', created_at: iso(-5 * DAY), expires_at: null, usage: accountUsage(0), selfcheck: selfcheck('pass') } },
]
const keys: Owned<ApiKey>[] = [
  { userId: 1, value: { id: 1, name: '开发环境', prefix: 'sk-pt-Demo', status: 'active', created_at: iso(-25 * DAY), last_used_at: iso(-120_000) } },
  { userId: 1, value: { id: 2, name: '自动化工作流', prefix: 'sk-pt-Flow', status: 'active', created_at: iso(-12 * DAY), last_used_at: iso(-1_800_000) } },
  { userId: 1, value: { id: 3, name: '已停用的测试密钥', prefix: 'sk-pt-Test', status: 'disabled', created_at: iso(-30 * DAY), last_used_at: iso(-10 * DAY) } },
  { userId: 2, value: { id: 4, name: '默认', prefix: 'sk-pt-User', status: 'active', created_at: iso(-5 * DAY), last_used_at: iso(-3_600_000) } },
]
const orders: Owned<Order>[] = []
const orderTerms = new Map<string, { unitQuantity: number; durationDays: number }>()
for (const entry of subscriptions) {
  const product = products.find(item => item.kind === entry.value.kind)
  const orderNo = entry.value.source_order_no
  if (!product || !orderNo) continue
  orders.push({ userId: entry.userId, value: {
    order_no: orderNo, product_id: product.id, product_name: product.name,
    kind: product.kind, quantity: entry.value.quantity / product.unit_quantity,
    amount_cents: product.price_cents * entry.value.quantity / product.unit_quantity,
    currency: 'CNY', provider: 'mock', status: 'fulfilled', renew_subscription_id: null,
    created_at: entry.value.starts_at, paid_at: entry.value.starts_at, fulfilled_at: entry.value.starts_at,
    expires_at: new Date(Date.parse(entry.value.starts_at) + 30 * 60_000).toISOString(),
  } })
  orderTerms.set(orderNo, { unitQuantity: product.unit_quantity, durationDays: product.duration_days })
}
orders.push({ userId: 1, value: { order_no: 'PT202610310001', product_id: 1, product_name: products[0].name, kind: 'concurrency', quantity: 1, amount_cents: 9900, currency: 'CNY', provider: 'mock', status: 'pending', renew_subscription_id: null, created_at: iso(-5 * 60_000), paid_at: null, fulfilled_at: null, expires_at: iso(25 * 60_000) } })
orderTerms.set('PT202610310001', { unitQuantity: 5, durationDays: 30 })
const rentalCandidates: PoolCandidate[] = Array.from({ length: 8 }, (_, index) => ({
  vm_id: `demo-vm-${index + 1}`, label: `演示资源 ${index + 1}`, email_masked: `vm${index + 1}***@***.com`, cred_status: 'valid', schedule_state: 'on', tier: index % 2 ? 'pro' : 'max',
}))
const pool: PoolItem[] = rentalCandidates.slice(0, 5).map((candidate, index) => ({
  vm_id: candidate.vm_id, tier: candidate.tier, status: index === 0 ? 'leased' : index === 4 ? 'maintenance' : 'available',
  leased_to_user_id: index === 0 ? 1 : null, leased_to_email: index === 0 ? users[0].email : null,
  note: index === 4 ? '演示：维护中的账号' : '演示资源，请勿用于真实请求',
  vm: { label: candidate.label, email_masked: candidate.email_masked, cred_status: candidate.cred_status, schedule_state: index === 4 ? 'off' : candidate.schedule_state, tier: candidate.tier, five_hour: { utilization: index * 7, resets_at: iso(3_600_000), status: 'ok' }, seven_day: { utilization: index * 11, resets_at: iso(2 * DAY), status: 'ok' } },
  selfcheck: index === 4 ? selfcheck('fail') : selfcheck('pass'),
}))
const accountVMs = new Map<number, string>([[1, 'demo-hosted-1'], [2, 'demo-hosted-2'], [3, 'demo-hosted-3'], [4, 'demo-vm-1'], [5, 'demo-hosted-5']])
const rentalSubscriptions = new Map<number, number>([[4, 3]])
const onboardings = new Map<number, Onboarding>()
const bootstrapFailures = new Set<number>()
const selfcheckReports = new Map<number, SelfcheckReport[]>()
const jobs: Job[] = [
  { id: 1, type: 'hosted.reauthorize', status: 'failed', attempts: 3, run_after: iso(-3_600_000), last_error: '演示：OAuth 授权已失效，请重新授权。', payload: { account_id: 2 }, created_at: iso(-2 * DAY), updated_at: iso(-3_600_000) },
  { id: 2, type: 'usage.refresh', status: 'pending', attempts: 0, run_after: iso(60_000), last_error: null, payload: { account_id: 1 }, created_at: iso(-60_000), updated_at: iso(-60_000) },
  { id: 3, type: 'usage.refresh', status: 'running', attempts: 1, run_after: iso(-30_000), last_error: null, payload: { account_id: 4 }, created_at: iso(-60_000), updated_at: iso(-30_000) },
]
const logs: Owned<UsageLog>[] = []
const models = ['claude-sonnet-4-5', 'claude-opus-4-1', 'claude-haiku-4-5']
for (let day = 29; day >= 0; day--) {
  for (let index = 0; index < 14; index++) {
    const userId = index === 13 ? 2 : 1
    const keyId = userId === 2 ? 4 : index % 2 + 1
    const accountId = userId === 2 ? 5 : index % 3 === 0 ? 4 : 1
    const account = accounts.find(item => item.value.id === accountId)?.value
    const key = keys.find(item => item.value.id === keyId)?.value
    const failed = (day * 7 + index) % 19 === 0
    const input = 850 + (day * 137 + index * 211) % 7_500
    logs.push({ userId, value: {
      request_id: `req-demo-${day}-${index}`, created_at: new Date(Math.min(today - day * DAY + (index + 1) * 5_100_000, startedAt - (14 - index) * 60_000)).toISOString(),
      model: models[(day + index) % models.length], key_id: keyId, key_name: key?.name ?? '', account_id: accountId, account_label: account?.label ?? '',
      stream: index % 4 !== 0, status_code: failed ? 429 : 200, error_label: failed ? 'rate_limit_exceeded' : null,
      input_tokens: failed ? 0 : input, output_tokens: failed ? 0 : Math.floor(input * 0.27), cache_read_tokens: failed ? 0 : input * 3, cache_creation_tokens: failed ? 0 : Math.floor(input * 0.18),
      cost_usd: failed ? 0 : money(input * (index % 3 + 1) * 0.000004), duration_ms: 1_200 + (index * 431 + day * 113) % 6_000, first_token_ms: failed ? 0 : 390 + (index * 79 + day * 31) % 1_400,
    } })
  }
}

class MockError extends Error {
  readonly status: number
  readonly code: string
  readonly checks: SelfcheckCheck[]
  constructor(status: number, code: string, message: string, checks: SelfcheckCheck[] = []) { super(message); this.status = status; this.code = code; this.checks = checks }
}
function fail(status: number, code: string, message: string, checks: SelfcheckCheck[] = []): never { throw new MockError(status, code, message, checks) }
function ok(data: unknown = {}, status = 200): Response {
  return new Response(JSON.stringify({ ok: true, data }), { status, headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' } })
}
function record(value: unknown): Input {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) fail(400, 'invalid_request', '请求内容必须是 JSON 对象。')
  return value as Input
}
function text(input: Input, key: string, fallback?: string): string {
  const value = input[key]
  if (value === undefined && fallback !== undefined) return fallback
  if (typeof value !== 'string') fail(422, `invalid_${key}`, `${key} 必须是文本。`)
  return value.trim()
}
function integer(input: Input, key: string, minimum = 0, fallback?: number): number {
  const value = input[key] ?? fallback
  if (typeof value !== 'number' || !Number.isSafeInteger(value) || value < minimum) fail(422, `invalid_${key}`, `${key} 必须是大于等于 ${minimum} 的整数。`)
  return value
}
function boolean(input: Input, key: string, fallback: boolean): boolean {
  const value = input[key] ?? fallback
  if (typeof value !== 'boolean') fail(422, `invalid_${key}`, `${key} 必须是布尔值。`)
  return value
}
function limitedText(input: Input, key: string, fallback: string, maximum = 40): string {
  const value = text(input, key, fallback)
  if ([...value].length > maximum) fail(422, `invalid_${key}`, `${key} 最多 ${maximum} 个字符。`)
  return value
}
function oneOf<T extends string>(value: unknown, choices: readonly T[], code: string): T {
  const found = choices.find(choice => choice === value)
  if (found === undefined) fail(422, code, '所选类型或状态不正确。')
  return found
}
function kind(value: unknown): ProductKind { return oneOf(value, ['concurrency', 'hosted_seat', 'rental_account'], 'invalid_kind') }
function positiveId(value: string): number {
  const id = Number(value)
  if (!Number.isSafeInteger(id) || id < 1) fail(404, 'not_found', '未找到该资源。')
  return id
}
function findOwned<T extends { id: number }>(items: Owned<T>[], id: number, userId: number): Owned<T> {
  return items.find(item => item.value.id === id && item.userId === userId) ?? fail(404, 'not_found', '资源不存在或无权访问。')
}
function emailOf(userId: number) { return users.find(user => user.id === userId)?.email ?? '' }
function myValues<T>(items: Owned<T>[], userId: number): T[] { return items.filter(item => item.userId === userId).map(item => item.value) }
function paginate<T>(items: T[], query: URLSearchParams): Page<T> {
  const pageValue = Number(query.get('page') ?? 1)
  const sizeValue = Number(query.get('page_size') ?? 20)
  const page = Number.isSafeInteger(pageValue) && pageValue > 0 ? pageValue : 1
  const pageSize = Number.isSafeInteger(sizeValue) && sizeValue > 0 ? Math.min(sizeValue, 100) : 20
  return { items: items.slice((page - 1) * pageSize, page * pageSize), total: items.length, page, page_size: pageSize }
}
function currentUser(): User {
  const user = users.find(item => item.id === sessionUserId)
  if (!user) fail(401, 'unauthorized', '请先登录。')
  if (user.status === 'disabled') fail(403, 'user_disabled', '该演示用户已停用。')
  return user
}
// In-site update simulation: a job walks through the real stages, then the
// "restarted" server reports the new version. The running version survives the
// page reload that follows, like a real update would.
const MOCK_VERSION_KEY = 'seat-hub.mock.version'
const mockReleases: ReleaseInfo[] = [
  { version: 'v0.2.0', name: 'v0.2.0', notes: '- 新增「使用记录」页，按账号查看每次调用\n- 深色与浅色两套主题，默认跟随系统\n- 管理后台支持站内一键更新与回滚', url: 'https://github.com/DueGin/seat-hub/releases/tag/v0.2.0', published_at: iso(-DAY) },
  { version: 'v0.1.0', name: 'v0.1.0', notes: '- 首个版本', url: 'https://github.com/DueGin/seat-hub/releases/tag/v0.1.0', published_at: iso(-10 * DAY) },
]
let mockJob: { kind: 'update' | 'rollback'; version: string; startedAt: number } | null = null
function mockVersion(): string { return sessionStorage.getItem(MOCK_VERSION_KEY) || 'v0.1.0' }
function mockJobView(): UpdateJob | null {
  if (!mockJob) return null
  const elapsed = Date.now() - mockJob.startedAt
  if (elapsed > 7000) { sessionStorage.setItem(MOCK_VERSION_KEY, mockJob.version); mockJob = null; return null }
  const total = mockJob.kind === 'update' ? 21_400_000 : 0
  const stage: UpdateStage = mockJob.kind === 'rollback' ? 'restarting' : elapsed < 3000 ? 'downloading' : elapsed < 3800 ? 'verifying' : elapsed < 4300 ? 'installing' : 'restarting'
  return { kind: mockJob.kind, version: mockJob.version, stage, done: Math.min(total, Math.round(total * elapsed / 3000)), total, started_at: new Date(mockJob.startedAt).toISOString() }
}
function mockUpdateStatus(): UpdateStatus {
  const job = mockJobView()
  const current = mockVersion()
  const latest = mockReleases[0]
  const updated = current === latest.version
  return {
    current, commit: 'a1b2c3d4e5f6', build_time: iso(-DAY), supervisor: 'docker', drain_seconds: 20, can_update: true,
    latest, changelog: mockReleases.filter(release => release.version > current), update_available: latest.version > current,
    checked_at: new Date().toISOString(), job, pending: false,
    rollback: updated ? { version: 'v0.1.0', source: 'image' } : null, last_failure: null,
  }
}

function publicConfig(): PublicConfig {
  mockJobView()
  return {
    version: mockVersion(),
    site_name: settings.site_name, base_url: '', registration_open: settings.registration_open, dev: true,
    payment_providers: [
      ...(settings.payment.epay.enabled ? [{ id: 'epay', name: '在线支付（演示）' }] : []),
      ...(settings.payment.manual.enabled ? [{ id: 'manual', name: '人工转账（演示）' }] : []),
      { id: 'mock', name: '模拟支付' },
    ],
  }
}
function productView(product: Product): Product {
  const reserved = orders.filter(item => item.value.kind === 'rental_account' && ['pending', 'paid'].includes(item.value.status)).reduce((sum, item) => sum + rentalNeeded(item.value), 0)
  return { ...product, stock: product.kind === 'rental_account' ? Math.max(0, pool.filter(item => item.status === 'available').length - reserved) : null }
}
function subscriptionView(subscription: Subscription): Subscription {
  const ids = products.filter(product => product.enabled && product.kind === subscription.kind && subscription.quantity % product.unit_quantity === 0).map(product => product.id)
  return { ...subscription, renewable: subscription.status !== 'cancelled' && ids.length > 0, renew_product_ids: ids }
}
function liveAccount(account: Account): boolean { return !['released', 'releasing'].includes(account.status) }
function vmHasOwner(vmId: string): boolean { return accounts.some(item => liveAccount(item.value) && accountVMs.get(item.value.id) === vmId) }
function overview(userId: number): Overview {
  const ownedAccounts = myValues(accounts, userId).filter(liveAccount)
  const activeSubs = myValues(subscriptions, userId).filter(item => item.status === 'active')
  const total = (type: ProductKind) => activeSubs.filter(item => item.kind === type).reduce((sum, item) => sum + item.quantity, 0)
  const hosted = total('hosted_seat')
  const rental = total('rental_account')
  const purchased = total('concurrency')
  const fromAccounts = ownedAccounts.filter(item => item.kind === 'hosted' && item.status === 'active').length * settings.concurrency_per_hosted_seat
    + ownedAccounts.filter(item => item.kind === 'rental' && item.status === 'active').length * settings.concurrency_per_rental_account
  const concurrency = settings.base_concurrency + purchased + fromAccounts
  const inflight = ownedAccounts.reduce((sum, item) => sum + (item.status === 'active' ? item.usage?.inflight ?? 0 : 0), 0)
  const usage = usageSummary(userId, new URLSearchParams({ from: new Date(today).toISOString(), until: new Date(Date.now()).toISOString() }))
  return {
    base_url: '', concurrency: { total: concurrency, in_use: Math.min(concurrency, inflight), base: settings.base_concurrency, purchased, from_accounts: fromAccounts },
    hosted_seats: { total: hosted, used: ownedAccounts.filter(item => item.kind === 'hosted').length },
    rental_accounts: { total: rental, used: ownedAccounts.filter(item => item.kind === 'rental').length },
    accounts_active: ownedAccounts.filter(item => item.status === 'active').length,
    keys: myValues(keys, userId).filter(item => item.status === 'active').length,
    expiring_soon: activeSubs.filter(item => Date.parse(item.expires_at) <= Date.now() + 7 * DAY).map(subscriptionView),
    usage_today: ownedAccounts.some(item => item.usage !== null) ? { requests: usage.totals.requests, tokens: usage.totals.tokens, cost_usd: usage.totals.cost_usd } : null,
  }
}
function releaseAccount(account: Account) {
  account.status = 'released'; account.status_reason = null; account.usage = null
  onboardings.delete(account.id); bootstrapFailures.delete(account.id)
  const item = pool.find(candidate => candidate.vm_id === accountVMs.get(account.id))
  if (item) { item.status = 'available'; item.leased_to_user_id = null; item.leased_to_email = null }
}
function refreshState() {
  for (const entry of orders) {
    if (entry.value.status === 'pending' && Date.parse(entry.value.expires_at) <= Date.now()) entry.value.status = 'expired'
  }
  for (const entry of subscriptions) {
    if (entry.value.status === 'active' && Date.parse(entry.value.expires_at) <= Date.now()) entry.value.status = 'expired'
  }
  for (const [accountId, subscriptionId] of rentalSubscriptions) {
    const subscription = subscriptions.find(item => item.value.id === subscriptionId)?.value
    const account = accounts.find(item => item.value.id === accountId)?.value
    if (account && liveAccount(account) && subscription && subscription.status !== 'active') releaseAccount(account)
  }
  for (const user of users) {
    const seats = myValues(subscriptions, user.id).filter(item => item.kind === 'hosted_seat' && item.status === 'active').reduce((sum, item) => sum + item.quantity, 0)
    const hosted = myValues(accounts, user.id).filter(item => item.kind === 'hosted' && liveAccount(item)).sort((a, b) => a.created_at.localeCompare(b.created_at))
    for (const account of hosted.slice(seats)) {
      if (account.status === 'suspended') continue
      account.status = 'suspended'; account.status_reason = '托管席位已到期或被取消，请续费后联系管理员重试。'; account.usage = null
      onboardings.delete(account.id)
    }
  }
}

function tokenCount(log: UsageLog) { return log.input_tokens + log.output_tokens + log.cache_read_tokens + log.cache_creation_tokens }
function logView(log: UsageLog): UsageLog {
  return { ...log, key_name: keys.find(item => item.value.id === log.key_id)?.value.name ?? log.key_name, account_label: accounts.find(item => item.value.id === log.account_id)?.value.label ?? log.account_label }
}
function timeRange(query: URLSearchParams, defaultDays: number): { from: string; until: string } {
  const from = query.get('from') || new Date(today - (defaultDays - 1) * DAY).toISOString()
  const until = query.get('until') || new Date(Date.now()).toISOString()
  if (!Number.isFinite(Date.parse(from)) || !Number.isFinite(Date.parse(until)) || Date.parse(from) > Date.parse(until)) fail(422, 'invalid_range', '请选择正确的起止时间。')
  return { from: new Date(from).toISOString(), until: new Date(until).toISOString() }
}
function filteredLogs(userId: number, query: URLSearchParams, days = 30): UsageLog[] {
  const range = timeRange(query, days)
  const status = query.get('status')
  if (status && status !== 'success' && status !== 'error') fail(422, 'invalid_status', '用量状态只能为 success 或 error。')
  return myValues(logs, userId).filter(log => log.created_at >= range.from && log.created_at <= range.until
    && (!query.get('model') || log.model === query.get('model'))
    && (!query.get('key_id') || log.key_id === Number(query.get('key_id')))
    && (!query.get('account_id') || log.account_id === Number(query.get('account_id')))
    && (!status || (status === 'success' ? log.status_code < 400 : log.status_code >= 400))).map(logView)
}
function chartData(items: UsageLog[], range: { from: string; until: string }): { daily: DailyUsage[]; by_model: ModelUsage[] } {
  const daily = new Map<string, DailyUsage>()
  const from = Date.parse(range.from.slice(0, 10) + 'T00:00:00Z')
  const until = Date.parse(range.until.slice(0, 10) + 'T00:00:00Z')
  if ((until - from) / DAY > 366) fail(422, 'invalid_range', '演示环境最多查询 367 天。')
  for (let date = from; date <= until; date += DAY) {
    const key = new Date(date).toISOString().slice(0, 10)
    daily.set(key, { date: key, requests: 0, tokens: 0, cost_usd: 0 })
  }
  const byModel = new Map<string, ModelUsage>()
  for (const log of items) {
    const date = log.created_at.slice(0, 10)
    const day = daily.get(date) ?? { date, requests: 0, tokens: 0, cost_usd: 0 }
    day.requests++; day.tokens += tokenCount(log); day.cost_usd = money(day.cost_usd + log.cost_usd)
    daily.set(date, day)
    const model = byModel.get(log.model) ?? { model: log.model, requests: 0, tokens: 0, cost_usd: 0 }
    model.requests++; model.tokens += tokenCount(log); model.cost_usd = money(model.cost_usd + log.cost_usd)
    byModel.set(log.model, model)
  }
  return { daily: [...daily.values()].sort((a, b) => a.date.localeCompare(b.date)), by_model: [...byModel.values()].sort((a, b) => b.cost_usd - a.cost_usd) }
}
function statsFor(userId: number, accountId?: number, keyId?: number) {
  const range = timeRange(new URLSearchParams(), 30)
  return chartData(filteredLogs(userId, new URLSearchParams()).filter(log => (accountId === undefined || log.account_id === accountId) && (keyId === undefined || log.key_id === keyId)), range)
}
function usageSummary(userId: number, query: URLSearchParams): UsageSummary {
  const range = timeRange(query, 7)
  const items = filteredLogs(userId, query, 7)
  const totals = spend()
  const byKey = new Map<number, UsageSummary['by_key'][number]>()
  const byAccount = new Map<number, UsageSummary['by_account'][number]>()
  for (const log of items) {
    totals.requests++; totals.tokens += tokenCount(log); totals.input_tokens += log.input_tokens; totals.output_tokens += log.output_tokens
    totals.cache_read_tokens += log.cache_read_tokens; totals.cache_creation_tokens += log.cache_creation_tokens; totals.cost_usd = money(totals.cost_usd + log.cost_usd)
    const key = byKey.get(log.key_id) ?? { key_id: log.key_id, key_name: log.key_name, requests: 0, tokens: 0, cost_usd: 0 }
    key.requests++; key.tokens += tokenCount(log); key.cost_usd = money(key.cost_usd + log.cost_usd); byKey.set(log.key_id, key)
    const account = byAccount.get(log.account_id) ?? { account_id: log.account_id, label: log.account_label, requests: 0, tokens: 0, cost_usd: 0 }
    account.requests++; account.tokens += tokenCount(log); account.cost_usd = money(account.cost_usd + log.cost_usd); byAccount.set(log.account_id, account)
  }
  const successful = items.filter(log => log.status_code < 400)
  const timings = successful.map(log => log.first_token_ms).filter((value): value is number => value !== null).sort((a, b) => a - b)
  return {
    ...range, totals, ...chartData(items, range), by_key: [...byKey.values()], by_account: [...byAccount.values()],
    stats: { success_rate: items.length ? successful.length / items.length : 0, avg_ttft_ms: timings.length ? Math.round(timings.reduce((sum, value) => sum + value, 0) / timings.length) : 0, p95_ttft_ms: timings[Math.max(0, Math.ceil(timings.length * 0.95) - 1)] ?? 0, error_count: items.length - successful.length },
  }
}

function onboardingFor(account: Account): Onboarding {
  if (account.status === 'suspended') fail(409, 'account_suspended', '账号已暂停，请恢复托管席位并联系管理员重试。')
  let onboarding = onboardings.get(account.id)
  if (!onboarding) {
    onboarding = { account_id: account.id, status: account.status, auth_url: null, auth_url_expires_at: null, bootstrap: account.status === 'active' ? { state: 'succeeded', message: '演示账号已就绪。' } : null, error: account.status === 'failed' && account.selfcheck?.state !== 'exhausted' ? { code: 'provision_failed', message: '演示：初始化失败，请重试。' } : null, can_regenerate: ['awaiting_auth', 'needs_reauth', 'failed'].includes(account.status), selfcheck: account.selfcheck }
    onboardings.set(account.id, onboarding)
  }
  onboarding.selfcheck = account.selfcheck
  return onboarding
}
function requireSelfcheck(account: Account) {
  if (account.selfcheck?.state === 'exhausted') fail(409, 'selfcheck_exhausted', '多次自检未通过，已转人工处理，请联系管理员。', account.selfcheck.checks)
  if (account.selfcheck?.checks.some((check) => check.code === 'account_unique' && !check.ok)) fail(409, 'account_duplicate', '该账号已在其它槽位托管，请勿重复添加。', account.selfcheck.checks)
  if (account.selfcheck?.state !== 'pass') fail(409, 'selfcheck_failed', '运行环境自检未通过，请等待自动重建完成后重试。', account.selfcheck?.checks ?? [])
}
function generateAuthURL(account: Account): Onboarding {
  requireSelfcheck(account)
  const onboarding = onboardingFor(account)
  const url = new URL('https://oauth.invalid.example/authorize')
  url.searchParams.set('demo', 'true'); url.searchParams.set('account_id', String(account.id)); url.searchParams.set('generation', String(nextId()))
  account.status = 'awaiting_auth'; account.status_reason = null
  Object.assign(onboarding, { status: 'awaiting_auth', auth_url: url.href, auth_url_expires_at: new Date(Date.now() + 10 * 60_000).toISOString(), bootstrap: null, error: null, can_regenerate: true })
  return onboarding
}
function pollOnboarding(account: Account): Onboarding {
  if (account.status === 'provisioning') {
    if (account.selfcheck?.state === 'pending') {
      if (account.label.includes('重复')) account.selfcheck = duplicateSelfcheck()
      else if (account.label.includes('耗尽')) { account.selfcheck = selfcheck('exhausted', settings.selfcheck_max_rebuilds); account.status = 'failed'; account.status_reason = '多次自检未通过，已转人工处理。' }
      else if (account.label.includes('自检失败')) account.selfcheck = selfcheck('fail', Math.min(2, settings.selfcheck_max_rebuilds))
      else account.selfcheck = selfcheck('pass')
    }
    if (account.selfcheck?.state === 'pass') return generateAuthURL(account)
    return onboardingFor(account)
  }
  const onboarding = onboardingFor(account)
  if (account.status === 'bootstrapping') {
    if (bootstrapFailures.delete(account.id)) {
      account.status = 'failed'; account.status_reason = '演示：初始化失败，可点击重试。'; account.usage = null
      Object.assign(onboarding, { status: 'failed', bootstrap: { state: 'failed', message: account.status_reason }, error: { code: 'bootstrap_failed', message: account.status_reason }, can_regenerate: true })
      return onboarding
    }
    account.status = 'active'; account.status_reason = null; account.tier = 'pro'; account.email_masked ||= 'dev***@***.com'; account.usage = accountUsage(0)
    Object.assign(onboarding, { status: 'active', auth_url: null, auth_url_expires_at: null, bootstrap: { state: 'succeeded', message: '演示初始化完成，账号已可用。' }, error: null, can_regenerate: false })
  }
  return onboarding
}
function restartAccount(account: Account): Onboarding {
  account.status = 'provisioning'; account.status_reason = null; account.usage = null; account.selfcheck = selfcheck('pending'); onboardings.delete(account.id); bootstrapFailures.delete(account.id)
  return onboardingFor(account)
}
function orderNumber() { return `PT${new Date().toISOString().replace(/\D/g, '').slice(0, 17)}${nextId()}` }
function orderEntry(orderNo: string, userId?: number): Owned<Order> {
  return orders.find(item => item.value.order_no === orderNo && (userId === undefined || item.userId === userId)) ?? fail(404, 'not_found', '订单不存在。')
}
function paymentFor(order: Order): Payment {
  if (order.provider === 'manual') return { type: 'manual', instructions: settings.payment.manual.instructions }
  if (order.provider === 'epay') return { type: 'redirect', url: `https://payment.invalid.example/checkout?order_no=${encodeURIComponent(order.order_no)}&demo=true` }
  return { type: 'mock', instructions: '这是开发期模拟订单，不会产生真实扣款。点击模拟支付后轮询订单即可完成履约。' }
}
function createOrder(userId: number, input: Input): Owned<Order> {
  const product = products.find(item => item.id === integer(input, 'product_id', 1) && item.enabled) ?? fail(404, 'not_found', '商品不存在或已下架。')
  const provider = text(input, 'provider')
  if (!publicConfig().payment_providers.some(item => item.id === provider)) fail(422, 'provider_unavailable', '该支付方式当前不可用。')
  const renewId = input.renew_subscription_id === undefined || input.renew_subscription_id === null ? null : integer(input, 'renew_subscription_id', 1)
  let quantity: number
  if (renewId !== null) {
    const subscription = findOwned(subscriptions, renewId, userId).value
    if (subscription.status === 'cancelled' || subscription.kind !== product.kind || subscription.quantity % product.unit_quantity !== 0) fail(422, 'renew_mismatch', '该商品不能续费此订阅。')
    quantity = subscription.quantity / product.unit_quantity
    const needed = subscription.kind === 'rental_account' ? Math.max(0, subscription.quantity - rentalBound(subscription.id).length) : 0
    if (needed > (productView(product).stock ?? 0)) fail(409, 'out_of_stock', '续费所需的租赁库存不足。')
  } else {
    quantity = integer(input, 'quantity', 1)
    if (quantity < product.min_qty || quantity > product.max_qty) fail(422, 'invalid_quantity', '购买数量超出商品允许范围。')
    if (product.kind === 'rental_account' && quantity * product.unit_quantity > (productView(product).stock ?? 0)) fail(409, 'out_of_stock', '当前可租赁库存不足。')
  }
  if (!Number.isSafeInteger(product.price_cents * quantity)) fail(422, 'invalid_quantity', '订单金额超出允许范围。')
  const entry: Owned<Order> = { userId, value: { order_no: orderNumber(), product_id: product.id, product_name: product.name, kind: product.kind, quantity, amount_cents: product.price_cents * quantity, currency: 'CNY', provider, status: 'pending', renew_subscription_id: renewId, created_at: new Date().toISOString(), paid_at: null, fulfilled_at: null, expires_at: new Date(Date.now() + 30 * 60_000).toISOString() } }
  orderTerms.set(entry.value.order_no, { unitQuantity: product.unit_quantity, durationDays: product.duration_days })
  orders.unshift(entry)
  return entry
}
function rentalBound(subscriptionId: number): Account[] {
  return accounts.filter(item => rentalSubscriptions.get(item.value.id) === subscriptionId && liveAccount(item.value)).map(item => item.value)
}
function rentalNeeded(order: Order): number {
  if (order.kind !== 'rental_account') return 0
  const quantity = order.quantity * (orderTerms.get(order.order_no)?.unitQuantity ?? 1)
  return Math.max(0, quantity - (order.renew_subscription_id === null ? 0 : rentalBound(order.renew_subscription_id).length))
}
function fulfill(entry: Owned<Order>) {
  const order = entry.value
  if (order.status === 'fulfilled') return
  if (order.status !== 'paid' && order.status !== 'fulfill_failed') fail(409, 'invalid_order_status', '当前订单状态不能履约。')
  const terms = orderTerms.get(order.order_no)
  if (!terms) { order.status = 'fulfill_failed'; return }
  const available = pool.filter(item => item.status === 'available')
  const needed = rentalNeeded(order)
  if (available.length < needed) { order.status = 'fulfill_failed'; return }
  let subscription: Subscription
  if (order.renew_subscription_id !== null) {
    const existing = subscriptions.find(item => item.value.id === order.renew_subscription_id && item.userId === entry.userId)
    if (!existing || existing.value.status === 'cancelled') { order.status = 'fulfill_failed'; return }
    subscription = existing.value
    subscription.expires_at = new Date(Math.max(Date.now(), Date.parse(subscription.expires_at)) + terms.durationDays * DAY).toISOString()
    subscription.status = 'active'; subscription.source_order_no = order.order_no
    for (const [accountId, subscriptionId] of rentalSubscriptions) {
      if (subscriptionId !== subscription.id) continue
      const account = accounts.find(item => item.value.id === accountId)?.value
      if (account && liveAccount(account)) account.expires_at = subscription.expires_at
    }
  } else {
    const quantity = order.quantity * terms.unitQuantity
    subscription = { id: nextId(), kind: order.kind, quantity, status: 'active', starts_at: new Date().toISOString(), expires_at: new Date(Date.now() + terms.durationDays * DAY).toISOString(), source_order_no: order.order_no, renewable: true, renew_product_ids: [] }
    subscriptions.unshift({ userId: entry.userId, value: subscription })
  }
  if (order.kind === 'rental_account') {
    for (const resource of available.slice(0, needed)) {
      resource.status = 'leased'; resource.leased_to_user_id = entry.userId; resource.leased_to_email = emailOf(entry.userId)
      const account: Account = { id: nextId(), kind: 'rental', label: resource.vm.label, email_masked: resource.vm.email_masked, status: 'active', status_reason: null, tier: resource.tier, created_at: new Date().toISOString(), expires_at: subscription.expires_at, usage: accountUsage(0), selfcheck: resource.selfcheck }
      accounts.unshift({ userId: entry.userId, value: account }); accountVMs.set(account.id, resource.vm_id); rentalSubscriptions.set(account.id, subscription.id)
    }
  }
  order.status = 'fulfilled'; order.fulfilled_at = new Date().toISOString()
}
function markPaid(entry: Owned<Order>, immediate: boolean) {
  if (entry.value.status === 'fulfilled') return
  if (entry.value.status === 'paid') { if (immediate) fulfill(entry); return }
  if (entry.value.status !== 'pending') fail(409, 'invalid_order_status', '只有待支付订单可以确认收款。')
  entry.value.status = 'paid'; entry.value.paid_at = new Date().toISOString()
  if (immediate) fulfill(entry)
}
function cancelOrder(entry: Owned<Order>) {
  if (entry.value.status !== 'pending') fail(409, 'invalid_order_status', '仅待支付订单可以取消。')
  entry.value.status = 'cancelled'
}

async function passwordDigest(password: string): Promise<string> {
  const bytes = await crypto.subtle.digest('SHA-256', new TextEncoder().encode(password))
  return [...new Uint8Array(bytes)].map(byte => byte.toString(16).padStart(2, '0')).join('')
}
const seedPasswords = passwordDigest('demo123456').then(digest => { for (const user of users) if (user.id <= 3) passwords.set(user.id, digest) })
function passwordValue(input: Input, name: string): string {
  const value = input[name]
  if (typeof value !== 'string' || value.length < 8 || value.length > 128) fail(422, 'invalid_password', '密码长度须为 8–128 个字符。')
  return value
}
function emailValue(input: Input): string {
  const email = text(input, 'email').toLowerCase()
  if (email.length > 254 || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) fail(422, 'invalid_email', '请输入有效的邮箱地址。')
  return email
}
function addUser(email: string, digest: string): User {
  const user: User = { id: nextId(), email, role: 'user', status: 'active', created_at: new Date().toISOString() }
  users.push(user); passwords.set(user.id, digest)
  return user
}

async function authRequest(path: string, method: string, input: Input): Promise<Response | undefined> {
  if (path === '/auth/logout' && method === 'POST') { setSession(null); return ok() }
  if (path === '/auth/me' && method === 'GET') return ok({ user: currentUser() })
  if (path === '/auth/password' && method === 'POST') {
    const user = currentUser()
    const previous = await passwordDigest(passwordValue(input, 'old_password'))
    if (passwords.get(user.id) !== previous) fail(422, 'invalid_password', '原密码不正确。')
    passwords.set(user.id, await passwordDigest(passwordValue(input, 'new_password')))
    return ok()
  }
  if ((path !== '/auth/login' && path !== '/auth/register') || method !== 'POST') return undefined
  const email = emailValue(input)
  const password = path === '/auth/register' ? passwordValue(input, 'password') : typeof input.password === 'string' ? input.password : ''
  if (path === '/auth/login' && (password.length < 8 || password.length > 128)) fail(401, 'invalid_credentials', '邮箱或密码不正确。')
  let user = users.find(item => item.email === email)
  if (path === '/auth/register') {
    if (!settings.registration_open) fail(403, 'registration_closed', '当前未开放注册。')
    if (user) fail(409, 'email_taken', '此邮箱已经注册。')
    const digest = await passwordDigest(password)
    if (users.some(item => item.email === email)) fail(409, 'email_taken', '此邮箱已经注册。')
    user = addUser(email, digest); setSession(user.id)
    return ok({ user }, 201)
  }
  const attempts = loginAttempts.get(email)
  if (attempts && attempts.until > Date.now() && attempts.count >= 5) fail(429, 'too_many_attempts', '尝试次数过多，请一分钟后重试。')
  const digest = await passwordDigest(password)
  user = users.find(item => item.email === email)
  if (!user && settings.registration_open) user = addUser(email, digest)
  if (!user || passwords.get(user.id) !== digest) {
    const activeAttempts = attempts && attempts.until > Date.now() ? attempts : { count: 0, until: Date.now() + 60_000 }
    activeAttempts.count++; loginAttempts.set(email, activeAttempts)
    fail(401, 'invalid_credentials', '邮箱或密码不正确。演示种子用户密码为 demo123456。')
  }
  if (user.status === 'disabled') fail(403, 'user_disabled', '该用户已停用。')
  loginAttempts.delete(email); setSession(user.id)
  userProfiles.set(user.id, { ...profileOf(user.id), last_login_at: new Date().toISOString(), last_login_ip: '127.0.0.1' }); mockSessions.set(user.id, (mockSessions.get(user.id) ?? 0) + 1)
  return ok({ user })
}

function productInput(input: Input, existing?: Product): Product {
  const product: Product = {
    id: existing?.id ?? nextId(), kind: input.kind === undefined && existing ? existing.kind : kind(input.kind),
    name: limitedText(input, 'name', existing?.name ?? '', 120), description: limitedText(input, 'description', existing?.description ?? '', 2_000),
    unit_quantity: integer(input, 'unit_quantity', 1, existing?.unit_quantity), duration_days: integer(input, 'duration_days', 1, existing?.duration_days),
    price_cents: integer(input, 'price_cents', 0, existing?.price_cents), currency: text(input, 'currency', existing?.currency ?? 'CNY'),
    min_qty: integer(input, 'min_qty', 1, existing?.min_qty ?? 1), max_qty: integer(input, 'max_qty', 1, existing?.max_qty ?? 20),
    enabled: boolean(input, 'enabled', existing?.enabled ?? true), sort_order: integer(input, 'sort_order', Number.MIN_SAFE_INTEGER, existing?.sort_order ?? 0),
    badge: limitedText(input, 'badge', existing?.badge ?? '', 40), stock: null,
  }
  if (!product.name || product.currency !== 'CNY' || product.min_qty > product.max_qty || product.duration_days > 3_650) fail(422, 'invalid_product', '请检查商品名称、金额、数量和有效天数。')
  return product
}
function adminAccount(entry: Owned<Account>): Account {
  return { ...entry.value, user_email: emailOf(entry.userId), vm_id: accountVMs.get(entry.value.id) ?? '', last_error: entry.value.status_reason ?? '' }
}
// Admin-only profile fields the demo keeps beside User.
const userProfiles = new Map<number, { note: string; last_login_at: string; last_login_ip: string }>()
const mockSessions = new Map<number, number>() // user id -> signed-in devices
function profileOf(id: number) { return userProfiles.get(id) ?? { note: '', last_login_at: '', last_login_ip: '' } }
function adminUser(user: User): AdminUser {
  const summary = overview(user.id)
  return { ...user, ...profileOf(user.id), active_sessions: mockSessions.get(user.id) ?? (user.id === sessionUserId ? 1 : 0), concurrency_total: user.status === 'active' ? summary.concurrency.total : 0, hosted_used: summary.hosted_seats.used, hosted_total: summary.hosted_seats.total, rental_used: summary.rental_accounts.used, rental_total: summary.rental_accounts.total, vm2api_user_id: `demo-user-${user.id}` }
}
function dashboard(): AdminDashboard {
  const paidOrders = orders.filter(item => ['paid', 'fulfilled', 'fulfill_failed'].includes(item.value.status) && item.value.provider !== 'grant')
  return {
    users: users.length, paying_users: new Set(paidOrders.map(item => item.userId)).size,
    revenue_cents_30d: paidOrders.filter(item => item.value.paid_at !== null && Date.parse(item.value.paid_at) >= Date.now() - 30 * DAY).reduce((sum, item) => sum + item.value.amount_cents, 0),
    orders_pending: orders.filter(item => item.value.status === 'pending').length,
    accounts: { hosted_active: accounts.filter(item => item.value.kind === 'hosted' && item.value.status === 'active').length, rental_active: accounts.filter(item => item.value.kind === 'rental' && item.value.status === 'active').length, failed: accounts.filter(item => item.value.status === 'failed').length },
    rental_pool: { total: pool.length, available: pool.filter(item => item.status === 'available').length, leased: pool.filter(item => item.status === 'leased').length, maintenance: pool.filter(item => item.status === 'maintenance').length },
    vm2api: { ok: true, version: '演示 · 无真实连接', latency_ms: 0, error: null },
  }
}
function settingsView(): Settings { return { ...settings, payment: { ...settings.payment, epay: { ...settings.payment.epay, key: epayKeyConfigured ? '***' : '' } } } }
function patchSettings(input: Input) {
  const updated: NormalizedSettings = structuredClone(settings)
  if (input.site_name !== undefined) updated.site_name = limitedText(input, 'site_name', '', 120)
  if (!updated.site_name) fail(422, 'invalid_site_name', '站点名称不能为空。')
  if (input.registration_open !== undefined) updated.registration_open = boolean(input, 'registration_open', true)
  const numberKeys = ['base_concurrency', 'concurrency_per_hosted_seat', 'concurrency_per_rental_account', 'hosted_grace_hours', 'rental_grace_hours', 'gateway_queue_timeout_ms'] as const
  for (const key of numberKeys) if (input[key] !== undefined) updated[key] = integer(input, key)
  if (input.selfcheck_require_dedicated_proxy !== undefined) updated.selfcheck_require_dedicated_proxy = boolean(input, 'selfcheck_require_dedicated_proxy', true)
  if (input.selfcheck_require_timezone_match !== undefined) updated.selfcheck_require_timezone_match = boolean(input, 'selfcheck_require_timezone_match', true)
  if (input.selfcheck_max_rebuilds !== undefined) updated.selfcheck_max_rebuilds = integer(input, 'selfcheck_max_rebuilds')
  if (input.selfcheck_recheck_minutes !== undefined) updated.selfcheck_recheck_minutes = integer(input, 'selfcheck_recheck_minutes', 5)
  if (updated.selfcheck_max_rebuilds > 10 || updated.selfcheck_recheck_minutes > 1440) fail(422, 'invalid_selfcheck_settings', '最多重建次数须为 0–10，复检间隔须为 5–1440 分钟。')
  if (input.hosted_vm_template !== undefined) updated.hosted_vm_template = structuredClone(record(input.hosted_vm_template))
  let configured = epayKeyConfigured
  if (input.payment !== undefined) {
    const payment = record(input.payment)
    const epay = updated.payment.epay
    const manual = updated.payment.manual
    if (payment.manual !== undefined) {
      const manualInput = record(payment.manual)
      if (manualInput.enabled !== undefined) manual.enabled = boolean(manualInput, 'enabled', true)
      if (manualInput.instructions !== undefined) manual.instructions = limitedText(manualInput, 'instructions', '', 4_000)
    }
    if (payment.epay !== undefined) {
      const epayInput = record(payment.epay)
      if (epayInput.enabled !== undefined) epay.enabled = boolean(epayInput, 'enabled', false)
      if (epayInput.gateway_url !== undefined) {
        const gateway = text(epayInput, 'gateway_url')
        let url: URL
        try { url = new URL(gateway) } catch { return fail(422, 'invalid_gateway_url', '请输入有效的 HTTPS 支付地址。') }
        if (url.protocol !== 'https:') fail(422, 'invalid_gateway_url', '支付地址必须使用 HTTPS。')
        epay.gateway_url = gateway
      }
      if (epayInput.pid !== undefined) epay.pid = text(epayInput, 'pid')
      if (epayInput.key !== undefined) { const key = text(epayInput, 'key'); if (key !== '***') configured = key.length > 0 }
      if (epayInput.types !== undefined) {
        const types = epayInput.types
        if (!Array.isArray(types) || !types.every((value: unknown): value is string => typeof value === 'string')) fail(422, 'invalid_types', '支付类型必须是文本数组。')
        epay.types = [...types]
      }
    }
  }
  settings = updated; epayKeyConfigured = configured
}

function recordSelfcheck(account: Account) {
  const current = account.selfcheck ?? selfcheck('pending')
  const reports = selfcheckReports.get(account.id) ?? []
  reports.unshift({ id: nextId(), vm_id: accountVMs.get(account.id) ?? '', attempt: current.attempts, passed: current.state === 'pass', checks: current.checks, created_at: new Date().toISOString() })
  selfcheckReports.set(account.id, reports)
}
function runAccountSelfcheck(account: Account, rebuild: boolean): Selfcheck {
  const previous = account.selfcheck ?? selfcheck('pending')
  const duplicate = previous.checks.some((check) => check.code === 'account_unique' && !check.ok)
  if (duplicate) account.selfcheck = duplicateSelfcheck()
  else if (rebuild || previous.state === 'pass' || previous.state === 'pending') account.selfcheck = selfcheck('pass')
  else account.selfcheck = { ...previous, checked_at: new Date().toISOString() }
  if (account.selfcheck.state === 'pass' && account.status === 'failed') {
    account.status = 'awaiting_auth'; account.status_reason = null
    const onboarding = onboardings.get(account.id)
    if (onboarding) Object.assign(onboarding, { status: 'awaiting_auth', error: null, can_regenerate: true })
  }
  recordSelfcheck(account)
  return account.selfcheck
}
function selfcheckHistory(account: Account): SelfcheckReport[] {
  if (!selfcheckReports.has(account.id)) recordSelfcheck(account)
  return selfcheckReports.get(account.id) ?? []
}

function adminRequest(path: string, method: string, query: URLSearchParams, input: Input): Response | undefined {
  const parts = path.split('/').filter(Boolean)
  const resource = parts[1]
  const rawId = parts[2]
  const action = parts[3]
  if (path === '/admin/dashboard' && method === 'GET') return ok(dashboard())
  if (path === '/admin/system/update' && method === 'GET') return ok(mockUpdateStatus())
  if (path === '/admin/system/update' && method === 'POST') {
    if (mockJob) fail(409, 'update_in_progress', '已有更新或回滚正在进行，请稍候')
    const latest = mockReleases[0].version
    if (input.version !== latest) fail(409, 'version_not_latest', '只能更新到最新发布的版本，请刷新后重试')
    if (mockVersion() === latest) fail(409, 'version_not_newer', '该版本不比当前运行的版本新')
    mockJob = { kind: 'update', version: latest, startedAt: Date.now() }
    return ok(mockJobView(), 202)
  }
  if (path === '/admin/system/rollback' && method === 'POST') {
    const target = mockUpdateStatus().rollback
    if (!target) fail(409, 'no_rollback', '没有可以回滚到的上一个版本')
    if (mockJob) fail(409, 'update_in_progress', '已有更新或回滚正在进行，请稍候')
    mockJob = { kind: 'rollback', version: target!.version, startedAt: Date.now() + 2500 }
    return ok(mockJobView() ?? { kind: 'rollback', version: target!.version, stage: 'restarting', done: 0, total: 0, started_at: new Date().toISOString() }, 202)
  }
  if (path === '/admin/settings') {
    if (method === 'GET') return ok(settingsView())
    if (method === 'PUT') { patchSettings(input); return ok(settingsView()) }
  }
  if (resource === 'products') {
    if (!rawId && method === 'GET') return ok({ items: [...products].sort((a, b) => a.sort_order - b.sort_order).map(productView) })
    if (!rawId && method === 'POST') { const product = productInput(input); products.push(product); return ok({ product: productView(product) }, 201) }
    if (rawId && parts.length === 3 && ['PATCH', 'DELETE'].includes(method)) {
      const index = products.findIndex(item => item.id === positiveId(rawId))
      const product = products[index] ?? fail(404, 'not_found', '商品不存在。')
      if (method === 'DELETE') {
        if (orders.some(item => item.value.product_id === product.id)) product.enabled = false
        else products.splice(index, 1)
        return ok()
      }
      const updated = productInput(input, product)
      Object.assign(product, updated); return ok({ product: productView(product) })
    }
  }
  if (resource === 'orders') {
    if (!rawId && method === 'GET') return ok(paginate(orders.filter(item => (!query.get('status') || item.value.status === query.get('status')) && (!query.get('user_id') || item.userId === Number(query.get('user_id')))).map(item => ({ ...item.value, user_email: emailOf(item.userId) })), query))
    if (rawId && action && method === 'POST' && parts.length === 4 && ['mark-paid', 'cancel', 'refulfill'].includes(action)) {
      const entry = orderEntry(rawId)
      if (action === 'mark-paid') markPaid(entry, true)
      if (action === 'cancel') cancelOrder(entry)
      if (action === 'refulfill') {
        if (entry.value.status !== 'fulfill_failed') fail(409, 'invalid_order_status', '仅履约失败的订单可以重试。')
        fulfill(entry)
      }
      return ok()
    }
  }
  if (resource === 'users') {
    if (!rawId && method === 'GET') return ok(paginate(users.filter(user => !query.get('q') || user.email.toLowerCase().includes((query.get('q') ?? '').toLowerCase())).map(adminUser), query))
    if (rawId && (parts.length === 3 || (parts.length === 4 && action === 'grant' && method === 'POST'))) {
      const user = users.find(item => item.id === positiveId(rawId)) ?? fail(404, 'not_found', '用户不存在。')
      if (!action && method === 'GET') return ok({ user: adminUser(user), subscriptions: myValues(subscriptions, user.id).map(subscriptionView), accounts: myValues(accounts, user.id), keys: myValues(keys, user.id), orders: myValues(orders, user.id) })
      if (!action && method === 'PATCH') {
        const status = input.status === undefined ? user.status : oneOf(input.status, ['active', 'disabled'], 'invalid_status')
        const role = input.role === undefined ? user.role : oneOf(input.role, ['admin', 'user'], 'invalid_role')
        if (user.id === sessionUserId && (status === 'disabled' || role !== 'admin')) fail(409, 'self_change_forbidden', '不能停用当前管理员或移除自己的管理员权限。')
        if (user.role === 'admin' && user.status === 'active' && (role !== 'admin' || status !== 'active') && users.filter(item => item.role === 'admin' && item.status === 'active').length <= 1) fail(409, 'last_admin', '必须保留至少一名可用的管理员。')
        user.status = status; user.role = role
        if (status === 'disabled') for (const key of keys) if (key.userId === user.id) key.value.status = 'disabled'
        return ok({ user })
      }
      if (action === 'grant' && method === 'POST') {
        const grantKind = kind(input.kind)
        const quantity = integer(input, 'quantity', 1)
        const duration = integer(input, 'duration_days', 1)
        limitedText(input, 'note', '', 1_000)
        if (quantity > 10_000 || duration > 3_650) fail(422, 'invalid_grant', '赠送数量或天数超出允许范围。')
        if (grantKind === 'rental_account' && pool.filter(item => item.status === 'available').length < quantity) fail(409, 'out_of_stock', '当前可租赁库存不足。')
        const order: Order = { order_no: orderNumber(), product_id: 0, product_name: '管理员赠送', kind: grantKind, quantity, amount_cents: 0, currency: 'CNY', provider: 'grant', status: 'paid', renew_subscription_id: null, created_at: new Date().toISOString(), paid_at: new Date().toISOString(), fulfilled_at: null, expires_at: new Date().toISOString() }
        orderTerms.set(order.order_no, { unitQuantity: 1, durationDays: duration })
        const entry = { userId: user.id, value: order }; orders.unshift(entry); fulfill(entry)
        return ok()
      }
    }
  }
  if (resource === 'subscriptions' && rawId && parts.length === 3 && method === 'PATCH') {
    if (input.status === undefined && input.expires_at === undefined) fail(422, 'invalid_body', '请提供到期时间或订阅状态。')
    const subscription = subscriptions.find(item => item.value.id === positiveId(rawId))?.value ?? fail(404, 'not_found', '订阅不存在。')
    const status = input.status === undefined ? subscription.status : oneOf(input.status, ['cancelled'], 'invalid_status')
    let expires = subscription.expires_at
    if (input.expires_at !== undefined) {
      const value = text(input, 'expires_at')
      if (!/T.*(?:Z|[+-]\d{2}:\d{2})$/.test(value) || !Number.isFinite(Date.parse(value))) fail(422, 'invalid_expires_at', '到期时间必须为有效的 RFC3339 时间。')
      expires = new Date(value).toISOString()
      if (Date.parse(expires) <= Date.parse(subscription.starts_at)) fail(422, 'invalid_expires_at', '到期时间必须晚于开始时间。')
    }
    subscription.status = status === 'cancelled' ? status : Date.parse(expires) > Date.now() ? 'active' : 'expired'; subscription.expires_at = expires
    for (const [accountId, subscriptionId] of rentalSubscriptions) if (subscriptionId === subscription.id) {
      const account = accounts.find(item => item.value.id === accountId)?.value
      if (account) account.expires_at = expires
    }
    refreshState(); return ok()
  }
  if (resource === 'rental-pool') {
    if (!rawId && method === 'GET') return ok({ items: pool })
    if (rawId === 'candidates' && parts.length === 3 && method === 'GET') return ok({ items: rentalCandidates.filter(item => !pool.some(candidate => candidate.vm_id === item.vm_id) && !vmHasOwner(item.vm_id)) })
    if (!rawId && method === 'POST') {
      const ids = input.vm_ids
      if (!Array.isArray(ids) || ids.length === 0 || !ids.every((value: unknown): value is string => typeof value === 'string')) fail(422, 'invalid_vm_ids', '请选择至少一个资源。')
      const note = limitedText(input, 'note', '', 1_000)
      const candidates = ids.map(id => rentalCandidates.find(candidate => candidate.vm_id === id) ?? fail(404, 'not_found', '待入池资源不存在。'))
      if (new Set(ids).size !== ids.length || candidates.some(candidate => pool.some(item => item.vm_id === candidate.vm_id) || vmHasOwner(candidate.vm_id))) fail(409, 'vm_unavailable', '资源已入池或已归属用户。')
      const failedCandidate = candidates.find((candidate) => candidate.vm_id === 'demo-vm-8')
      if (failedCandidate) {
        const failed = selfcheck('fail')
        fail(409, 'selfcheck_failed', `${failedCandidate.label} 入池自检未通过。`, failed.checks)
      }
      for (const candidate of candidates) pool.push({ vm_id: candidate.vm_id, tier: candidate.tier, status: 'available', leased_to_user_id: null, leased_to_email: null, note, vm: { label: candidate.label, email_masked: candidate.email_masked, cred_status: candidate.cred_status, schedule_state: candidate.schedule_state, tier: candidate.tier, five_hour: null, seven_day: null }, selfcheck: selfcheck('pass') })
      return ok()
    }
    if (rawId && parts.length === 4 && action === 'selfcheck' && method === 'POST') {
      const item = pool.find((candidate) => candidate.vm_id === rawId) ?? fail(404, 'not_found', '资源池账号不存在。')
      item.selfcheck = selfcheck('pass')
      return ok(item.selfcheck)
    }
    if (rawId && parts.length === 3 && ['PATCH', 'DELETE'].includes(method)) {
      const index = pool.findIndex(item => item.vm_id === rawId)
      const item = pool[index] ?? fail(404, 'not_found', '资源池账号不存在。')
      if (item.status === 'leased') fail(409, 'resource_leased', '租用中的资源不能修改或移出资源池。')
      if (method === 'DELETE') { pool.splice(index, 1); return ok() }
      const status = oneOf(input.status, ['available', 'maintenance'], 'invalid_status')
      const note = input.note === undefined ? item.note : limitedText(input, 'note', '', 1_000)
      item.status = status; item.note = note; item.vm.schedule_state = status === 'available' ? 'on' : 'off'
      return ok()
    }
  }
  if (resource === 'accounts') {
    if (!rawId && method === 'GET') return ok(paginate(accounts.filter(item => (!query.get('kind') || item.value.kind === query.get('kind')) && (!query.get('status') || item.value.status === query.get('status')) && (!query.get('user_id') || item.userId === Number(query.get('user_id')))).map(adminAccount), query))
    if (rawId && parts.length === 4 && action === 'selfchecks' && method === 'GET') {
      const account = accounts.find(item => item.value.id === positiveId(rawId))?.value ?? fail(404, 'not_found', '账号不存在。')
      return ok({ items: selfcheckHistory(account) })
    }
    if (rawId && parts.length === 4 && action === 'selfcheck' && method === 'POST') {
      const account = accounts.find(item => item.value.id === positiveId(rawId))?.value ?? fail(404, 'not_found', '账号不存在。')
      return ok(runAccountSelfcheck(account, boolean(input, 'rebuild', false)))
    }
    if (rawId && parts.length === 4 && method === 'POST' && (action === 'retry' || action === 'release')) {
      const account = accounts.find(item => item.value.id === positiveId(rawId))?.value ?? fail(404, 'not_found', '账号不存在。')
      if (action === 'release') releaseAccount(account)
      else if (account.kind === 'hosted' && ['failed', 'needs_reauth', 'suspended'].includes(account.status)) restartAccount(account)
      else fail(409, 'invalid_account_status', '当前账号状态不能重试。')
      return ok()
    }
  }
  if (resource === 'jobs') {
    if (!rawId && method === 'GET') return ok(paginate(jobs.filter(job => !query.get('status') || job.status === query.get('status')), query))
    if (rawId && parts.length === 4 && action === 'retry' && method === 'POST') {
      const job = jobs.find(item => item.id === positiveId(rawId)) ?? fail(404, 'not_found', '任务不存在。')
      if (job.status !== 'failed') fail(409, 'invalid_job_status', '仅失败任务可以重试。')
      job.status = 'pending'; job.run_after = new Date().toISOString(); job.updated_at = new Date().toISOString(); job.last_error = null; job.attempts++
      return ok()
    }
  }
  return undefined
}

function paymentRequest(path: string, method: string, query: URLSearchParams, input: Input): Response | undefined {
  const match = /^\/payments\/([^/]+)\/(notify|return)$/.exec(path)
  if (!match || (match[2] === 'notify' ? !['GET', 'POST'].includes(method) : method !== 'GET')) return undefined
  if (!['mock', 'manual', 'epay'].includes(match[1])) fail(404, 'not_found', '支付提供方不存在。')
  const orderNo = query.get('order_no') || query.get('out_trade_no') || (typeof input.order_no === 'string' ? input.order_no : typeof input.out_trade_no === 'string' ? input.out_trade_no : '')
  if (match[2] === 'return') {
    const order = orders.find(item => item.value.order_no === orderNo && item.value.provider === match[1])
    return new Response(null, { status: 302, headers: { Location: order ? `/orders/${encodeURIComponent(orderNo)}` : '/orders', 'Cache-Control': 'no-store' } })
  }
  if (match[1] !== 'mock') return new Response('fail', { status: 400, headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
  const entry = orders.find(item => item.value.order_no === orderNo && item.value.provider === 'mock')
  if (!entry) return new Response('fail', { status: 404, headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
  markPaid(entry, true)
  return new Response('success', { headers: { 'Content-Type': 'text/plain; charset=utf-8' } })
}

function userRequest(path: string, method: string, query: URLSearchParams, input: Input, user: User): Response | undefined {
  const parts = path.split('/').filter(Boolean)
  if (path === '/me/overview' && method === 'GET') return ok(overview(user.id))
  if (path === '/me/subscriptions' && method === 'GET') {
    const status = oneOf(query.get('status') || 'active', ['active', 'expired', 'all'], 'invalid_status')
    return ok({ items: myValues(subscriptions, user.id).filter(item => status === 'all' || item.status === status).map(subscriptionView) })
  }
  if (path === '/usage/summary' && method === 'GET') return ok(usageSummary(user.id, query))
  if (path === '/usage/logs' && method === 'GET') return ok(paginate(filteredLogs(user.id, query).sort((a, b) => b.created_at.localeCompare(a.created_at)), query))
  if (path === '/accounts' && method === 'GET') return ok({ items: myValues(accounts, user.id).filter(account => query.get('include_released') === '1' || liveAccount(account)) })
  if (parts[0] === 'accounts' && parts.length === 2 && ['GET', 'PATCH'].includes(method)) {
    const account = findOwned(accounts, positiveId(parts[1]), user.id).value
    if (method === 'GET') return ok({ account, ...statsFor(user.id, account.id) })
    account.label = limitedText(input, 'label', '')
    return ok()
  }
  if (path === '/hosted' && method === 'POST') {
    const summary = overview(user.id)
    if (summary.hosted_seats.used >= summary.hosted_seats.total) fail(409, 'no_free_seat', '没有空闲托管席位，请先购买席位。')
    const account: Account = { id: nextId(), kind: 'hosted', label: limitedText(input, 'label', '新的托管账号'), email_masked: '', status: 'provisioning', status_reason: null, tier: null, created_at: new Date().toISOString(), expires_at: null, usage: null, selfcheck: selfcheck('pending') }
    accounts.unshift({ userId: user.id, value: account }); accountVMs.set(account.id, `demo-hosted-${account.id}`)
    return ok({ account, onboarding: onboardingFor(account) }, 201)
  }
  if (parts[0] === 'hosted' && parts.length >= 2 && parts.length <= 3) {
    const action = parts[2]
    if ((parts.length === 2 && method === 'DELETE') || (action === 'onboarding' && method === 'GET') || (['auth-url', 'auth-code', 'retry'].includes(action) && method === 'POST')) {
      const account = findOwned(accounts, positiveId(parts[1]), user.id).value
      if (account.kind !== 'hosted' || !liveAccount(account)) fail(404, 'not_found', '托管账号不存在。')
      if (method === 'DELETE') { releaseAccount(account); return ok() }
      if (action === 'onboarding') return ok(pollOnboarding(account))
      if (action === 'auth-url') {
        if (!['awaiting_auth', 'needs_reauth', 'failed'].includes(account.status)) fail(409, 'invalid_account_status', '当前状态不能重新生成授权链接。')
        return ok(generateAuthURL(account))
      }
      if (action === 'retry') {
        if (account.status !== 'failed') fail(409, 'invalid_account_status', '仅失败的托管账号可以重试。')
        return ok(restartAccount(account))
      }
      const code = text(input, 'code')
      if (code.length < 8 || code.length > 8_192 || /\s/.test(code)) fail(422, 'invalid_code', '授权码格式不正确，请输入至少 8 位演示授权码。')
      const onboarding = onboardingFor(account)
      requireSelfcheck(account)
      if (code.startsWith('duplicate-account')) fail(409, 'account_duplicate', '该账号已在其它槽位托管，请勿重复添加。', duplicateSelfcheck().checks)
      if (code.startsWith('selfcheck-exhausted')) fail(409, 'selfcheck_exhausted', '多次自检未通过，已转人工处理，请联系管理员。', selfcheck('exhausted', 3).checks)
      if (code.startsWith('selfcheck-failed')) fail(409, 'selfcheck_failed', '运行环境自检未通过，请等待自动重建完成后重试。', selfcheck('fail', 2).checks)
      if (!['awaiting_auth', 'needs_reauth'].includes(account.status)) fail(409, 'invalid_account_status', '当前账号不处于等待授权状态。')
      if (!onboarding.auth_url_expires_at || Date.parse(onboarding.auth_url_expires_at) <= Date.now()) fail(409, 'auth_url_expired', '授权链接已过期，请重新生成。')
      if (/^(invalid|fail|expired)/i.test(code)) fail(422, 'exchange_failed', '演示：授权码无效或已使用，请重新获取授权码。')
      if (code.startsWith('bootstrap-fail')) bootstrapFailures.add(account.id)
      account.status = 'bootstrapping'; account.status_reason = null
      Object.assign(onboarding, { status: 'bootstrapping', auth_url: null, auth_url_expires_at: null, bootstrap: { state: 'running', message: '演示：正在初始化账号，下一次轮询将完成。' }, error: null, can_regenerate: false })
      return ok(onboarding)
    }
  }
  if (path === '/keys' && method === 'GET') return ok({ items: myValues(keys, user.id) })
  if (path === '/keys' && method === 'POST') {
    if (myValues(keys, user.id).length >= 20) fail(422, 'key_limit', '最多只能创建 20 个密钥。')
    const name = limitedText(input, 'name', '默认') || '默认'
    const bytes = crypto.getRandomValues(new Uint8Array(32))
    const secret = `sk-pt-${[...bytes].map(byte => byte.toString(16).padStart(2, '0')).join('')}`
    const key: ApiKey = { id: nextId(), name, prefix: secret.slice(0, 12), status: 'active', created_at: new Date().toISOString(), last_used_at: null }
    keys.unshift({ userId: user.id, value: key })
    return ok({ key, secret }, 201)
  }
  if (parts[0] === 'keys' && parts.length >= 2 && parts.length <= 3 && ((parts[2] === 'stats' && method === 'GET') || (parts.length === 2 && ['PATCH', 'DELETE'].includes(method)))) {
    const entry = findOwned(keys, positiveId(parts[1]), user.id)
    if (parts[2] === 'stats') return ok(statsFor(user.id, undefined, entry.value.id))
    if (method === 'DELETE') { keys.splice(keys.indexOf(entry), 1); return ok() }
    const name = input.name === undefined ? entry.value.name : limitedText(input, 'name', '')
    const status = input.status === undefined ? entry.value.status : oneOf(input.status, ['active', 'disabled'], 'invalid_status')
    entry.value.name = name || '默认'; entry.value.status = status
    return ok({ key: entry.value })
  }
  if (path === '/orders' && method === 'GET') return ok(paginate(myValues(orders, user.id).sort((a, b) => b.created_at.localeCompare(a.created_at)), query))
  if (path === '/orders' && method === 'POST') {
    const entry = createOrder(user.id, input)
    return ok({ order: entry.value, payment: paymentFor(entry.value) }, 201)
  }
  if (parts[0] === 'orders' && ((parts.length === 2 && method === 'GET') || (parts.length === 3 && method === 'POST' && ['cancel', 'mock-pay'].includes(parts[2])))) {
    const entry = orderEntry(parts[1], user.id)
    if (method === 'GET') { if (entry.value.status === 'paid') fulfill(entry); return ok({ order: entry.value }) }
    if (parts[2] === 'cancel') cancelOrder(entry)
    else {
      if (entry.value.provider !== 'mock') fail(422, 'provider_unavailable', '仅模拟支付订单可以使用此操作。')
      markPaid(entry, false)
    }
    return ok()
  }
  return undefined
}

// Admin user management that needs hashing (create, reset) or touches several
// collections; mirrors internal/api/admin_users.go.
function mockPassword(): string {
  const alphabet = 'abcdefghijkmnopqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789'
  return Array.from(crypto.getRandomValues(new Uint32Array(16)), (n) => alphabet[n % alphabet.length]).join('')
}
function mockNote(input: Input): string {
  const note = typeof input.note === 'string' ? input.note.trim() : ''
  if (note.length > 500) fail(422, 'invalid_note', '备注最多 500 个字')
  return note
}
function mockRecords(id: number) {
  return { orders: myValues(orders, id).length, subscriptions: myValues(subscriptions, id).length, resources: myValues(accounts, id).length }
}
async function adminUserRequest(path: string, method: string, query: URLSearchParams, input: Input): Promise<Response | undefined> {
  const parts = path.split('/').filter(Boolean)
  if (parts[0] !== 'admin' || parts[1] !== 'users') return undefined
  const actor = currentUser()
  if (actor.role !== 'admin') fail(403, 'forbidden', '需要管理员权限')
  if (parts.length === 2 && method === 'GET') {
    const q = (query.get('q') ?? '').trim().toLowerCase()
    const role = query.get('role') ?? ''
    const status = query.get('status') ?? ''
    const id = Number(q.replace(/^#/, ''))
    return ok(paginate(users.filter(user => (!q || user.email.includes(q) || profileOf(user.id).note.toLowerCase().includes(q) || (Number.isSafeInteger(id) && id === user.id)) && (!role || user.role === role) && (!status || user.status === status)).sort((a, b) => b.id - a.id).map(adminUser), query))
  }
  if (parts.length === 2 && method === 'POST') {
    const email = emailValue(input)
    const role = input.role === undefined || input.role === '' ? 'user' : oneOf(input.role, ['admin', 'user'], 'invalid_role')
    const note = mockNote(input)
    const given = typeof input.password === 'string' ? input.password : ''
    if (given && (given.length < 8 || given.length > 128)) fail(422, 'invalid_password', '密码长度须为 8–128')
    if (users.some(item => item.email === email)) fail(409, 'email_taken', '该邮箱已注册')
    const password = given || mockPassword()
    const user = addUser(email, await passwordDigest(password)); user.role = role
    userProfiles.set(user.id, { note, last_login_at: '', last_login_ip: '' })
    return ok({ user: adminUser(user), ...(given ? {} : { password }) }, 201)
  }
  const user = users.find(item => item.id === positiveId(parts[2])) ?? fail(404, 'user_not_found', '用户不存在')
  const self = user.id === actor.id
  const lastAdmin = user.role === 'admin' && user.status === 'active' && users.filter(item => item.role === 'admin' && item.status === 'active').length <= 1
  if (parts.length === 3 && method === 'GET') {
    return ok({ user: adminUser(user), subscriptions: myValues(subscriptions, user.id).map(subscriptionView), accounts: myValues(accounts, user.id), keys: myValues(keys, user.id), orders: myValues(orders, user.id), sessions: Array.from({ length: mockSessions.get(user.id) ?? (self ? 1 : 0) }, () => ({ created_at: profileOf(user.id).last_login_at || new Date().toISOString(), expires_at: new Date(Date.now() + 7 * DAY).toISOString(), ip: '127.0.0.1', user_agent: navigator.userAgent })), records: mockRecords(user.id) })
  }
  if (parts.length === 3 && method === 'PATCH') {
    if (['email', 'status', 'role', 'note'].every(key => input[key] === undefined)) fail(422, 'invalid_body', '请至少修改一项')
    const status = input.status === undefined ? user.status : oneOf(input.status, ['active', 'disabled'], 'invalid_status')
    const role = input.role === undefined ? user.role : oneOf(input.role, ['admin', 'user'], 'invalid_role')
    const email = input.email === undefined ? user.email : emailValue(input)
    const note = input.note === undefined ? profileOf(user.id).note : mockNote(input)
    if (self && (status !== user.status || role !== user.role)) fail(409, 'self_lockout', '不能停用自己或取消自己的管理员身份，请让其他管理员操作')
    if (lastAdmin && (status === 'disabled' || role === 'user')) fail(409, 'last_admin', '不能停用、降级或删除最后一个管理员')
    if (email !== user.email && users.some(item => item.email === email)) fail(409, 'email_taken', '该邮箱已被其他用户使用')
    user.email = email; user.role = role; user.status = status
    userProfiles.set(user.id, { ...profileOf(user.id), note })
    if (status === 'disabled') mockSessions.set(user.id, 0)
    return ok({ user: adminUser(user) })
  }
  if (parts.length === 3 && method === 'DELETE') {
    if (self) fail(409, 'self_lockout', '不能删除自己的账号')
    if (lastAdmin) fail(409, 'last_admin', '不能停用、降级或删除最后一个管理员')
    const records = mockRecords(user.id)
    if (records.orders || records.subscriptions || records.resources) fail(409, 'user_has_records', `该用户有 ${records.orders} 笔订单、${records.subscriptions} 个订阅、${records.resources} 个账号资源，不能删除，请改为停用`)
    users.splice(users.indexOf(user), 1); passwords.delete(user.id); userProfiles.delete(user.id); mockSessions.delete(user.id)
    for (let index = keys.length - 1; index >= 0; index--) if (keys[index].userId === user.id) keys.splice(index, 1)
    return ok()
  }
  if (parts[3] === 'password' && method === 'POST') {
    if (self) fail(409, 'self_lockout', '修改自己的密码请在「账户安全」页操作')
    const given = typeof input.password === 'string' ? input.password : ''
    if (given && (given.length < 8 || given.length > 128)) fail(422, 'invalid_password', '密码长度须为 8–128')
    const password = given || mockPassword()
    passwords.set(user.id, await passwordDigest(password)); mockSessions.set(user.id, 0)
    return ok(given ? {} : { password })
  }
  if (parts[3] === 'sessions' && parts[4] === 'revoke' && method === 'POST') {
    if (self) fail(409, 'self_lockout', '不能在这里让自己下线，请使用「退出登录」')
    mockSessions.set(user.id, 0)
    return ok()
  }
  if (parts[3] === 'keys' && parts[4]) {
    const entry = keys.find(item => item.userId === user.id && item.value.id === positiveId(parts[4])) ?? fail(404, 'key_not_found', '密钥不存在')
    if (method === 'DELETE') { keys.splice(keys.indexOf(entry), 1); return ok() }
    if (method === 'PATCH') { entry.value.status = oneOf(input.status, ['active', 'disabled'], 'invalid_status'); return ok({ key: entry.value }) }
  }
  return undefined
}

export async function mockRequest(path: string, method: string, body?: unknown): Promise<Response> {
  try {
    await seedPasswords
    const url = new URL(path, 'https://portal.invalid.example')
    const normalized = url.pathname.length > 1 ? url.pathname.replace(/\/$/, '') : url.pathname
    const verb = method.toUpperCase()
    const input = body === undefined ? {} : record(body)
    refreshState()
    if (normalized === '/public/config' && verb === 'GET') return ok(publicConfig())
    if (normalized === '/public/products' && verb === 'GET') return ok({ items: [...products].filter(product => product.enabled).sort((a, b) => a.sort_order - b.sort_order).map(productView) })
    const payment = paymentRequest(normalized, verb, url.searchParams, input)
    if (payment) return payment
    if (normalized.startsWith('/auth/')) {
      const response = await authRequest(normalized, verb, input)
      return response ?? fail(404, 'not_found', '此演示接口不存在。')
    }
    const user = currentUser()
    if (normalized.startsWith('/admin/')) {
      if (user.role !== 'admin') fail(403, 'forbidden', '仅管理员可以访问此接口。')
      return (await adminUserRequest(normalized, verb, url.searchParams, input)) ?? adminRequest(normalized, verb, url.searchParams, input) ?? fail(404, 'not_found', '此管理接口尚未实现。')
    }
    return userRequest(normalized, verb, url.searchParams, input, user) ?? fail(404, 'not_found', '此演示接口不存在。')
  } catch (error: unknown) {
    const known = error instanceof MockError
    return new Response(JSON.stringify({ ok: false, error: { code: known ? error.code : 'mock_error', message: known ? error.message : '演示请求处理失败，请刷新页面后重试。', ...(known && error.checks.length ? { checks: error.checks } : {}) } }), { status: known ? error.status : 500, headers: { 'Content-Type': 'application/json; charset=utf-8', 'Cache-Control': 'no-store' } })
  }
}
