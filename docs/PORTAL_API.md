# Portal API 契约（/api）

前后端唯一契约。后端（Go）照此实现，前端（React）照此调用。改动必须先改本文。

## 约定

- JSON。成功 `{"ok":true,"data":…}`；失败 `{"ok":false,"error":{"code":"snake_case","message":"中文可读文案"}}`，HTTP 状态码有意义（400/401/403/404/409/422/429/500/502）。
- 会话：Cookie `portal_session`。前端 `fetch(..., {credentials:"include"})`。
- 写请求（POST/PUT/PATCH/DELETE）必须 `Content-Type: application/json`（无 body 时发 `{}`），否则 415 `unsupported_media_type`。
- 时间：RFC3339 UTC 字符串。金额：整数「分」`*_cents` + `currency`（`CNY`）。vm2api 来源的费用：浮点 `cost_usd`。
- 分页：`?page=1&page_size=20`（≤100）→ `data: {items:[…], total, page, page_size}`。
- ID：门面自有实体 `id` 为整数；`order_no` 为字符串（`PT` + 时间戳 + 随机）。
- 未登录调需登录接口 401 `unauthorized`；非 admin 调 `/api/admin/*` 403 `forbidden`。

## 公共

### `GET /api/public/config`
```json
{"site_name":"…","base_url":"https://portal.example.com","registration_open":true,
 "payment_providers":[{"id":"epay","name":"在线支付"},{"id":"manual","name":"人工转账"},{"id":"mock","name":"模拟支付"}],
 "version":"v0.1.0",
 "dev":false}
```
`base_url` 为空时前端用 `location.origin`。

**脱敏邮箱**：所有接口里的 `email_masked`（Claude 账号邮箱）都只保留邮箱名的前三个字符和域名的最后一段，例如 `fdsiome@protonmail.com` 显示为 `fds***@***.com`。邮箱名较短时最多显示一半（`abcd@x.io` 显示为 `ab***@***.io`）。门面用户自己的登录邮箱不做脱敏。

### `GET /api/public/products`
```json
{"items":[Product]}
```
`Product`:
```json
{"id":1,"kind":"concurrency|hosted_seat|rental_account","name":"5 路并发 · 30 天","description":"…",
 "unit_quantity":5,"duration_days":30,"price_cents":9900,"currency":"CNY",
 "min_qty":1,"max_qty":20,"enabled":true,"sort_order":0,"badge":"热销",
 "stock":12}
```
`stock` 只有 `rental_account` 有值（整数），其它为 `null`。只返回 `enabled=true`。

## 认证

| 方法 | 路径 | 请求 | 响应 data |
|---|---|---|---|
| POST | `/api/auth/register` | `{email,password}`（密码 8–128） | `{user:User}`，同时登录。注册关闭 403 `registration_closed`；邮箱已存在 409 `email_taken` |
| POST | `/api/auth/login` | `{email,password}` | `{user:User}`。错误 401 `invalid_credentials`；停用 403 `user_disabled`；限速 429 `too_many_attempts` |
| POST | `/api/auth/logout` | `{}` | `{}` |
| GET | `/api/auth/me` | — | `{user:User}` |
| POST | `/api/auth/password` | `{old_password,new_password}` | `{}`；撤销其它会话 |

`User`: `{"id":1,"email":"a@b.c","role":"user|admin","status":"active|disabled","created_at":"…"}`

## 用户：总览与订阅

### `GET /api/me/overview`
```json
{"base_url":"https://portal.example.com",
 "concurrency":{"total":6,"in_use":2,"base":1,"purchased":5,"from_accounts":0},
 "hosted_seats":{"total":3,"used":2},
 "rental_accounts":{"total":1,"used":1},
 "accounts_active":3,
 "keys":2,
 "expiring_soon":[Subscription],
 "usage_today":{"requests":120,"tokens":1234567,"cost_usd":3.21}}
```
`expiring_soon`：7 天内到期的 active 订阅。`usage_today` 来自 vm2api（取不到时为 `null`，不伪装 0）。

### `GET /api/me/subscriptions?status=active|expired|all`
`{items:[Subscription]}`。`Subscription`:
```json
{"id":3,"kind":"hosted_seat","quantity":2,"status":"active|expired|cancelled",
 "starts_at":"…","expires_at":"…","source_order_no":"PT…",
 "renewable":true,"renew_product_ids":[2,5]}
```
`renew_product_ids`：同 kind、`unit_quantity` 能整除 `quantity` 的上架商品。

## 订单与支付

### `POST /api/orders`
请求 `{product_id, quantity, provider, renew_subscription_id?}`。续费时 `quantity` 忽略（按订阅推算）。
- 库存不足 409 `out_of_stock`；数量越界 422 `invalid_quantity`；续费 kind 不符/不能整除 422 `renew_mismatch`；提供方未启用 422 `provider_unavailable`。
- 响应 `{order:Order, payment:Payment}`。`Payment`:
```json
{"type":"redirect|qrcode|manual|mock","url":"https://pay…","qr":"weixin://…","instructions":"转账说明…"}
```

`Order`:
```json
{"order_no":"PT20261031…","product_id":1,"product_name":"…","kind":"concurrency","quantity":2,
 "amount_cents":19800,"currency":"CNY","provider":"epay",
 "status":"pending|paid|fulfilled|fulfill_failed|cancelled|expired|refunded",
 "renew_subscription_id":null,"created_at":"…","paid_at":null,"fulfilled_at":null,"expires_at":"…"}
```

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/orders?page&page_size` | 我的订单 |
| GET | `/api/orders/{order_no}` | `{order:Order}`，前端支付后轮询 |
| POST | `/api/orders/{order_no}/cancel` | 仅 `pending` |
| POST | `/api/orders/{order_no}/mock-pay` | 仅 `PORTAL_DEV=1` 且 provider=mock |
| GET/POST | `/api/payments/{provider}/notify` | 支付平台异步回调（不走会话、不要求 JSON），按提供方协议回文本 |
| GET | `/api/payments/{provider}/return` | 同步跳回 → 302 到 `/orders/{order_no}` |

## 账号（资源）

`Account`:
```json
{"id":7,"kind":"hosted|rental","label":"主号","email_masked":"abc***@***.com",
 "status":"provisioning|awaiting_auth|bootstrapping|active|needs_reauth|suspended|failed|releasing|released|allocating",
 "status_reason":null,"tier":"pro|max|null","created_at":"…","expires_at":"…|null",
 "usage":AccountUsage|null,"selfcheck":Selfcheck|null}
```
`Selfcheck`（账号、托管上号进度和租赁池行共用）:
```json
{"state":"pending|pass|fail|exhausted","attempts":0,"max_attempts":3,
 "checked_at":"2026-10-31T00:00:00Z",
 "checks":[{"code":"proxy_exclusive","ok":true,"message":"代理独占检查通过"}]}
```
`attempts` 为已重建次数，`max_attempts` 为当前重建上限；尚未检查时 `checked_at=null`。每个检查项还可返回 `skipped:true`（该规则已跳过）、`remedy:"reset|reproxy|manual"`（失败处理方式）；管理员接口可额外返回 `detail`，用户接口仅返回可公开的提示。尚未检查的账号返回 `state:"pending"`、`checks:[]`。检查规则见 `docs/SELFCHECK.md`。

`AccountUsage`（来自 vm2api，取不到为 `null`）:
```json
{"available":true,"schedule_state":"on|off|unschedulable|restricted","restriction_text":null,
 "five_hour":{"utilization":12.5,"resets_at":"…","status":"ok"},
 "seven_day":{"utilization":34,"resets_at":"…","status":"ok"},
 "seven_day_fable":null,
 "estimate_5h":{"status":"ok|no_data|no_reset|log_gap|low_usage|unpriced","spent_usd":1.2,"total_usd":9.6,"remaining_usd":8.4,"used_ratio":0.125},
 "estimate_7d":{…},
 "today":Spend,"window_5h":Spend,"window_7d":Spend,
 "seats_used":1,"seats_max":4,"inflight":1,
 "fetched_at":"…"}
```
`Spend`: `{"requests":10,"input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_creation_tokens":4,"tokens":10,"cost_usd":0.12}`
utilization 为 0–100；窗口缺失为 `null`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/accounts?include_released=0` | `{items:[Account]}`（带 usage，后端缓存 30s） |
| GET | `/api/accounts/{id}` | `{account:Account, daily:[DailyUsage], by_model:[ModelUsage]}` |
| PATCH | `/api/accounts/{id}` | `{label}` 改备注 |

`DailyUsage`: `{"date":"2026-10-31","requests":1,"tokens":1,"cost_usd":0.1}`；`ModelUsage`: `{"model":"claude-…","requests":1,"tokens":1,"cost_usd":0.1}`

## 托管上号（仅 OAuth 授权链接）

`Onboarding`:
```json
{"account_id":7,"status":"provisioning|awaiting_auth|bootstrapping|active|needs_reauth|failed",
 "auth_url":"https://claude.ai/oauth/authorize?…","auth_url_expires_at":"…",
 "bootstrap":{"state":"pending|running|succeeded|failed","message":"…"}|null,
 "error":{"code":"…","message":"…"}|null,
 "can_regenerate":true,"selfcheck":Selfcheck|null}
```

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/hosted` | `{label?}` 新建托管账号。无空余席位 409 `no_free_seat`。响应 `{account:Account, onboarding:Onboarding}`（通常 `provisioning`） |
| GET | `/api/hosted/{id}/onboarding` | 轮询（前端 2s） |
| POST | `/api/hosted/{id}/auth-url` | `{}` 重新生成链接（`awaiting_auth`/`needs_reauth`/`failed` 可用） |
| POST | `/api/hosted/{id}/auth-code` | `{code}`。码格式错 422 `invalid_code`；链接过期 409 `auth_url_expired`；vm2api 换票失败 422 `exchange_failed`（message 为可读原因） |
| POST | `/api/hosted/{id}/retry` | `{}` `failed` 时重试建槽/初装 |
| DELETE | `/api/hosted/{id}` | 移除托管账号（销毁凭证、释放席位）。响应 `{}` |

不存在任何其它上号接口（sessionKey / setup-token / 凭证导入）。

生成授权链接和提交授权码前必须通过自检。未通过时返回 409 `selfcheck_failed`；重建次数耗尽返回 409 `selfcheck_exhausted`；账号已在其它槽位托管返回 409 `account_duplicate`，需人工处理。前端在自检 `pending` / `fail` 时展示检查和重建进度，仅在 `pass` 后展示授权链接。

## API Key

`ApiKey`: `{"id":1,"name":"默认","prefix":"sk-pt-AbC1","status":"active|disabled","created_at":"…","last_used_at":null,"scope":{"mode":"all|selected","accounts":[12,15],"usable":true}}`

`scope` 是这个密钥能调用哪些账号（[KEY_SCOPE.md](KEY_SCOPE.md)）：`all` = 该用户现在和以后的全部账号；`selected` = 只能调 `accounts` 里的账号（资源 id）。`usable=false` 表示所选账号当前都不可用，网关以 403 `key_no_accounts` 拒绝，重新选择后恢复。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/keys` | `{items:[ApiKey]}` |
| POST | `/api/keys` | `{name, scope?:{mode, accounts}}` → `{key:ApiKey, secret:"sk-pt-…"}`（明文只此一次）。不带 `scope` 即全部账号。上限 20 个 422 `key_limit`；所选账号不属于自己 422 `key_scope_invalid`；所选账号都不可用 422 `key_scope_empty` |
| PATCH | `/api/keys/{id}` | `{name?, status?:"active|disabled", scope?:{mode, accounts}}`。升级中的旧密钥改范围返回 409 `key_migrating` |
| DELETE | `/api/keys/{id}` | |
| GET | `/api/keys/{id}/stats` | `{daily:[DailyUsage], by_model:[ModelUsage]}`（vm2api 近 30 天） |

## 用量（vm2api 实时数据）

### `GET /api/usage/summary?from=&until=`（默认近 7 天）
```json
{"from":"…","until":"…",
 "totals":Spend,
 "daily":[DailyUsage],
 "by_model":[ModelUsage],
 "by_key":[{"key_id":1,"key_name":"默认","requests":1,"tokens":1,"cost_usd":0.1}],
 "by_account":[{"account_id":7,"label":"主号","requests":1,"tokens":1,"cost_usd":0.1}],
 "stats":{"success_rate":0.99,"avg_ttft_ms":820,"p95_ttft_ms":2100,"error_count":3}}
```

### `GET /api/usage/logs?page&page_size&from&until&model&key_id&account_id&status=success|error`
```json
{"items":[{"request_id":"…","created_at":"…","model":"…","key_id":1,"key_name":"默认",
  "account_id":7,"account_label":"主号","stream":true,"status_code":200,"error_label":null,
  "input_tokens":1,"output_tokens":2,"cache_read_tokens":3,"cache_creation_tokens":4,
  "cost_usd":0.01,"duration_ms":1234,"first_token_ms":456}],
 "total":100,"page":1,"page_size":20}
```

`account_id`（可选，正整数）只看该用户某个托管或租赁账号上的调用，用于「使用记录」页；时间范围自动收窄到该账号的持有期（租赁号被多次租用时只返回本次租期）。账号不属于当前用户返回 404 `not_found`；账号尚未分配槽位时返回空列表；非正整数返回 422 `invalid_request`。

## 站内更新

`UpdateStatus`：
```json
{"current":"v0.1.0","commit":"…","build_time":"…","supervisor":"docker|systemd|","drain_seconds":20,
 "can_update":true,"reason":"不能更新时的原因",
 "latest":Release|null,"changelog":[Release],"update_available":true,
 "checked_at":"…|null","check_error":"无法连接 GitHub 时的说明",
 "job":UpdateJob|null,"pending":false,
 "rollback":{"version":"v0.0.9","source":"image|download"}|null,
 "last_failure":{"version":"v0.2.0","error":"…","at":"…"}|null}
```
`Release`：`{version, name, notes, url, published_at}`，`notes` 是纯文本（Markdown 原文），前端不要按 HTML 渲染。

`UpdateJob`：`{kind:"update|rollback", version, stage:"downloading|verifying|installing|restarting|failed", done, total, error?, started_at}`。

- `pending=true` 表示刚更新的版本还在确认期（启动后约 30 秒），这期间不能再次更新。
- 进入 `restarting` 后服务会短暂不可用。前端应轮询 `GET /api/public/config`，等 `version` 变成目标版本后再刷新页面。
- 错误码：409 `update_unavailable`（附原因）、`update_in_progress`、`update_pending`、`version_not_latest`、`version_not_newer`、`no_rollback`；422 `invalid_version`；502 `update_source_unavailable`。

## 管理后台（role=admin）

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/admin/dashboard` | `{users, paying_users, revenue_cents_30d, orders_pending, accounts:{hosted_active, rental_active, failed}, rental_pool:{total, available, leased, maintenance}, vm2api:{ok, version, latency_ms, error}, scope_warnings:{checked_at, items:[{user_id, user_email, vm_id, pool_id, pool_name, message}]}}`。`scope_warnings` 是需要管理员处理的账号范围问题（号被 vm2api 里的外部账号池占用，见 [KEY_SCOPE.md](KEY_SCOPE.md)），每次对账重算 |
| POST | `/api/admin/scope/recheck` | `{}` 立即排一次账号范围对账（`scope_reconcile`），处理完外部账号池后用 |
| GET/POST | `/api/admin/products` | 列表（含未上架）/ 新建 `Product` 字段（无 `id`、`stock`） |
| PATCH/DELETE | `/api/admin/products/{id}` | 有订单引用时 DELETE 改为下架 |
| GET | `/api/admin/orders?status&user_id&page` | `Order` + `user_email` |
| POST | `/api/admin/orders/{order_no}/mark-paid` | `{trade_no?}` 人工确认收款 → 履约 |
| POST | `/api/admin/orders/{order_no}/cancel` | pending 取消 |
| POST | `/api/admin/orders/{order_no}/refulfill` | `fulfill_failed` 重试 |
| GET | `/api/admin/users?q&role&status&page` | `User` + `{concurrency_total, hosted_used, hosted_total, rental_used, rental_total, vm2api_user_id, vm2api_pool_id, note, last_login_at, last_login_ip, active_sessions}`（`vm2api_user_id` 仅旧数据迁移前有值）。`q` 匹配邮箱或备注，`#12`/`12` 按用户 ID 精确匹配；`role`=`user|admin`，`status`=`active|disabled` |
| POST | `/api/admin/users` | `{email, password?, role?, note?}` 新增用户，返回 201 `{user, password?}`。不传 `password` 时自动生成 16 位密码，只在本次响应里返回一次；邮箱已占用 409 `email_taken` |
| GET | `/api/admin/users/{id}` | `{user, subscriptions, accounts, keys, orders, sessions:[{created_at, expires_at, ip, user_agent}], records:{orders, subscriptions, resources}}` |
| PATCH | `/api/admin/users/{id}` | `{email?, status?, role?, note?}` 至少一项。停用时同步停用该用户的上游密钥，重新启用时一并恢复（用户自己停用的密钥保持停用）。改邮箱后用户须用新邮箱登录 |
| DELETE | `/api/admin/users/{id}` | 删除用户及其 API 密钥（先删上游密钥，再删该用户的账号池）。有订单、订阅或账号资源的用户返回 409 `user_has_records`，只能停用 |
| POST | `/api/admin/users/{id}/password` | `{password?}` 重置密码，并让该用户所有设备的登录失效。不传 `password` 时自动生成，返回 `{password}`（只显示一次） |
| POST | `/api/admin/users/{id}/sessions/revoke` | `{}` 强制下线：让该用户所有设备的登录失效，API 密钥不受影响 |
| PATCH/DELETE | `/api/admin/users/{id}/keys/{key_id}` | 管理员停用、启用（`{status}`）或删除该用户的某个 API 密钥，例如密钥泄露时使用 |


以上用户管理接口都会写入审计日志（`user.create`、`user.update`、`user.reset_password`、`user.revoke_sessions`、`user.delete`、`user.key_update`、`user.key_delete`），记录内容不含密码。管理员不能对自己执行停用、取消管理员身份、重置密码、强制下线或删除操作（409 `self_lockout`），也不能停用、降级或删除最后一个管理员（409 `last_admin`）。

| 方法 | 路径 | 说明 |
|---|---|---|
| POST | `/api/admin/users/{id}/grant` | `{kind, quantity, duration_days, note}` 赠送订阅（生成 provider=`grant` 的已履约订单） |
| PATCH | `/api/admin/subscriptions/{id}` | `{expires_at?, status?:"cancelled"}` |
| GET | `/api/admin/rental-pool` | `{items:[{vm_id, tier, status:"available|leased|maintenance", leased_to_user_id, leased_to_email, note, selfcheck:Selfcheck|null, vm:{label, email_masked, cred_status, schedule_state, tier, five_hour, seven_day}, foreign_pool?}]}`。`foreign_pool` 有值表示该号在 vm2api 的外部账号池里，不会被分配 |
| GET | `/api/admin/rental-pool/candidates` | vm2api 中未归属、Claude、未入池的 VM：`{items:[{vm_id,label,email_masked,cred_status,schedule_state,tier,foreign_pool?}]}`，`foreign_pool` 有值的不能入池 |
| POST | `/api/admin/rental-pool` | `{vm_ids:[…], note?}` 入池前自检，未通过返回 409 `selfcheck_failed`；号在外部账号池里返回 409 `vm_in_foreign_pool` |
| PATCH | `/api/admin/rental-pool/{vm_id}` | `{status:"available|maintenance", note?}`（`leased` 不可手改） |
| DELETE | `/api/admin/rental-pool/{vm_id}` | 仅非 leased |
| POST | `/api/admin/rental-pool/{vm_id}/selfcheck` | `{}` 重跑入池自检 → `Selfcheck`；`vm_id` 不能为空或全空白 |
| GET | `/api/admin/accounts?kind&status&user_id&page` | 全部资源 + `user_email` + `vm_id` + 最近错误 |
| POST | `/api/admin/accounts/{id}/selfcheck` | `{rebuild?:bool}` → `Selfcheck`。默认 `false` 立即重跑；`true` 重置重建计数并先重建再检查 |
| GET | `/api/admin/accounts/{id}/selfchecks?limit=20` | `{items:[SelfcheckReport]}`。`limit` 省略默认 20，须为 1–100 的整数；无历史时 `items:[]` |
| POST | `/api/admin/accounts/{id}/retry` | 重跑失败步骤 |
| POST | `/api/admin/accounts/{id}/release` | 强制释放 |
| GET | `/api/admin/jobs?status=failed|pending|running&page` | `{items:[{id,type,status,attempts,run_after,last_error,payload,created_at,updated_at}]}` |
| POST | `/api/admin/jobs/{id}/retry` | |
| GET/PUT | `/api/admin/settings` | `Settings`（PUT 部分更新；密钥字段只写，GET 返回 `"***"` 或空） |
| GET | `/api/admin/system/update?refresh=1` | 站内更新状态 `UpdateStatus`。默认用服务端缓存（最多 10 分钟查一次 GitHub）；`refresh=1` 强制重查，但 15 秒内重复请求仍用缓存 |
| POST | `/api/admin/system/update` | `{version}` 更新到指定版本，只能是最新发布且比当前新的版本。返回 202 和 `UpdateJob`，之后轮询状态接口看进度 |
| POST | `/api/admin/system/rollback` | `{}` 回滚到上一个版本并重启。返回 202 和 `UpdateJob` |

以上三个自检接口仅管理员可调用，均使用标准成功/失败信封。账号 `id` 必须为正整数；非法 ID、`rebuild` 类型、空白 VM ID，以及空值、非整数、重复或超出 1–100 的 `limit` 返回 422 `invalid_request`。自检执行成功时返回 HTTP 200 和实际检查状态，`state=fail|exhausted` 也会返回检查明细；执行错误沿用错误信封。

`SelfcheckReport`（自检历史中的一次检查）:
```json
{"id":12,"vm_id":"vm-7","attempt":1,"passed":false,
 "checks":[{"code":"proxy_exclusive","ok":false,"remedy":"rebuild",
            "message":"代理已被其它槽位使用","detail":"管理员诊断信息"}],
 "created_at":"2026-10-31T00:00:00Z"}
```

`Settings`（全新安装也返回完整的 `payment` 两段文档，前端可直接读 `payment.epay.*` / `payment.manual.*`）:
```json
{"site_name":"AI 工作台","registration_open":true,
 "base_concurrency":1,"concurrency_per_hosted_seat":0,"concurrency_per_rental_account":0,
 "hosted_grace_hours":24,"rental_grace_hours":0,"gateway_queue_timeout_ms":0,
 "hosted_vm_template":{"runtime_type":"docker"},
 "selfcheck_require_dedicated_proxy":true,"selfcheck_require_timezone_match":true,
 "selfcheck_max_rebuilds":3,"selfcheck_recheck_minutes":30,
 "payment":{"epay":{"enabled":false,"gateway_url":"","pid":"","key":"***","types":["alipay","wxpay"]},
            "manual":{"enabled":true,"instructions":"转账后请联系管理员确认收款。"}}}
```

自检设置：`selfcheck_require_dedicated_proxy` 默认 `true`，要求独占 SOCKS5 代理；`selfcheck_require_timezone_match` 默认 `true`，要求出口地理时区与槽位时区一致；`selfcheck_max_rebuilds` 默认 `3`，控制自动重建上限；`selfcheck_recheck_minutes` 默认 `30`，控制授权前复查间隔和后台巡检周期。`GET /api/admin/settings` 返回这四个字段，`PUT` 可部分更新。
