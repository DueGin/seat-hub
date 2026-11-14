export interface User { id: number; email: string; role: 'user' | 'admin'; status: 'active' | 'disabled'; created_at: string }
export interface PublicConfig { site_name: string; base_url: string; registration_open: boolean; payment_providers: { id: string; name: string }[]; dev: boolean; version?: string }
export interface ReleaseInfo { version: string; name: string; notes: string; url: string; published_at: string }
export type UpdateStage = 'downloading' | 'verifying' | 'installing' | 'restarting' | 'failed'
export interface UpdateJob { kind: 'update' | 'rollback'; version: string; stage: UpdateStage; done: number; total: number; error?: string; started_at: string }
export interface UpdateStatus { current: string; commit: string; build_time: string; supervisor: string; drain_seconds: number; can_update: boolean; reason?: string; latest: ReleaseInfo | null; changelog: ReleaseInfo[]; update_available: boolean; checked_at: string | null; check_error?: string; job: UpdateJob | null; pending: boolean; rollback: { version: string; source: 'download' | 'image' } | null; last_failure: { version: string; error: string; at: string } | null }
export type ProductKind = 'concurrency' | 'hosted_seat' | 'rental_account'
export interface Product { id: number; kind: ProductKind; name: string; description: string; unit_quantity: number; duration_days: number; price_cents: number; currency: string; min_qty: number; max_qty: number; enabled: boolean; sort_order: number; badge: string; stock: number | null }
export interface Subscription { id: number; kind: ProductKind; quantity: number; status: string; starts_at: string; expires_at: string; source_order_no?: string | null; renewable: boolean; renew_product_ids: number[] }
export interface Spend { requests: number; tokens: number; input_tokens: number; output_tokens: number; cache_read_tokens: number; cache_creation_tokens: number; cost_usd: number }
export interface DailyUsage { date: string; requests: number; tokens: number; cost_usd: number }
export interface ModelUsage { model: string; requests: number; tokens: number; cost_usd: number }
export interface Overview { base_url: string; concurrency: { total: number; in_use: number; base: number; purchased: number; from_accounts: number }; hosted_seats: { total: number; used: number }; rental_accounts: { total: number; used: number }; accounts_active: number; keys: number; expiring_soon: Subscription[]; usage_today: Pick<Spend, 'requests' | 'tokens' | 'cost_usd'> | null }
export interface UsageWindow { utilization: number; resets_at: string | null; status: string }
export interface Estimate { status: string; spent_usd: number; total_usd: number; remaining_usd: number; used_ratio: number }
export interface AccountUsage { available: boolean; schedule_state: string; restriction_text: string | null; five_hour: UsageWindow | null; seven_day: UsageWindow | null; seven_day_fable: UsageWindow | null; estimate_5h: Estimate | null; estimate_7d: Estimate | null; today: Spend; window_5h: Spend; window_7d: Spend; seats_used: number; seats_max: number; inflight: number; fetched_at: string }
export type SelfcheckState = 'pending' | 'pass' | 'fail' | 'exhausted'
export interface SelfcheckCheck { code: string; ok: boolean; skipped?: boolean; remedy?: string; message: string; detail?: string }
export interface Selfcheck { state: SelfcheckState; attempts: number; max_attempts: number; checked_at: string | null; checks: SelfcheckCheck[] }
export interface SelfcheckReport { id: number; vm_id: string; attempt: number; passed: boolean; checks: SelfcheckCheck[]; created_at: string }
export interface Account { id: number; kind: 'hosted' | 'rental'; label: string; email_masked: string; status: string; status_reason: string | null; tier: string | null; created_at: string; expires_at: string | null; usage: AccountUsage | null; selfcheck: Selfcheck | null; user_email?: string; vm_id?: string; last_error?: string }
export interface AccountDetail { account: Account; daily: DailyUsage[]; by_model: ModelUsage[] }
export interface Onboarding { account_id: number; status: string; auth_url: string | null; auth_url_expires_at: string | null; bootstrap: { state: string; message: string } | null; error: { code: string; message: string } | null; can_regenerate: boolean; selfcheck: Selfcheck | null }
export interface ApiKey { id: number; name: string; prefix: string; status: 'active' | 'disabled'; created_at: string; last_used_at: string | null }
export interface UsageSummary { from: string; until: string; totals: Spend; daily: DailyUsage[]; by_model: ModelUsage[]; by_key: { key_id: number; key_name: string; requests: number; tokens: number; cost_usd: number }[]; by_account: { account_id: number; label: string; requests: number; tokens: number; cost_usd: number }[]; stats: { success_rate: number; avg_ttft_ms: number; p95_ttft_ms: number; error_count: number } }
export interface UsageLog { request_id: string; created_at: string; model: string; key_id: number; key_name: string; account_id: number; account_label: string; stream: boolean; status_code: number; error_label: string | null; input_tokens: number; output_tokens: number; cache_read_tokens: number; cache_creation_tokens: number; cost_usd: number; duration_ms: number; first_token_ms: number | null }
export interface Page<T> { items: T[]; total: number; page: number; page_size: number }
export interface Order { order_no: string; product_id: number; product_name: string; kind: ProductKind; quantity: number; amount_cents: number; currency: string; provider: string; status: string; renew_subscription_id: number | null; created_at: string; paid_at: string | null; fulfilled_at: string | null; expires_at: string; user_email?: string }
export interface Payment { type: 'redirect' | 'qrcode' | 'manual' | 'mock'; url?: string; qr?: string; instructions?: string }
// Every field is optional-tolerant: a fresh install or an older stored document can omit
// whole blocks (notably payment), so readers must not assume the shape is complete.
export interface EpaySettings { enabled?: boolean; gateway_url?: string; pid?: string; key?: string; types?: string[] }
export interface ManualSettings { enabled?: boolean; instructions?: string }
export interface PaymentSettings { epay?: EpaySettings; manual?: ManualSettings }
export interface Settings { site_name?: string; registration_open?: boolean; base_concurrency?: number; concurrency_per_hosted_seat?: number; concurrency_per_rental_account?: number; hosted_grace_hours?: number; rental_grace_hours?: number; gateway_queue_timeout_ms?: number; hosted_vm_template?: Record<string, unknown>; payment?: PaymentSettings; selfcheck_require_dedicated_proxy?: boolean; selfcheck_require_timezone_match?: boolean; selfcheck_max_rebuilds?: number; selfcheck_recheck_minutes?: number }
// NormalizedSettings is what the settings editor and validateSettings return: the same
// document with every block filled in, so the form never renders an undefined field.
export interface NormalizedSettings extends Omit<Settings, 'payment'> {
  site_name: string; registration_open: boolean; base_concurrency: number; concurrency_per_hosted_seat: number; concurrency_per_rental_account: number; hosted_grace_hours: number; rental_grace_hours: number; gateway_queue_timeout_ms: number; hosted_vm_template: Record<string, unknown>
  selfcheck_require_dedicated_proxy: boolean; selfcheck_require_timezone_match: boolean; selfcheck_max_rebuilds: number; selfcheck_recheck_minutes: number
  payment: { epay: { enabled: boolean; gateway_url: string; pid: string; key: string; types: string[] }; manual: { enabled: boolean; instructions: string } }
}
export interface AdminDashboard { users: number; paying_users: number; revenue_cents_30d: number; orders_pending: number; accounts: { hosted_active: number; rental_active: number; failed: number }; rental_pool: { total: number; available: number; leased: number; maintenance: number }; vm2api: { ok: boolean; version: string; latency_ms: number; error: string | null } }
export interface AdminUser extends User { concurrency_total: number; hosted_used: number; hosted_total: number; rental_used: number; rental_total: number; vm2api_user_id: string | null }
export interface PoolCandidate { vm_id: string; label: string; email_masked: string; cred_status: string; schedule_state: string; tier: string | null }
export interface PoolItem { vm_id: string; tier: string | null; status: 'available' | 'leased' | 'maintenance'; leased_to_user_id?: number | null; leased_to_email?: string | null; note: string; vm: Omit<PoolCandidate, 'vm_id'> & { five_hour: UsageWindow | null; seven_day: UsageWindow | null }; selfcheck: Selfcheck | null }
export interface Job { id: number; type: string; status: string; attempts: number; run_after: string; last_error: string | null; payload: unknown; created_at: string; updated_at: string }
