# seat-hub 设计

账号托管 / 租赁门面。用户在这里买「托管席位」「租赁账号」「并发」，拿 `baseurl + apikey` 接入；请求经门面透传到 vm2api。**计费用量、账号额度全部以 vm2api 接口为准**，门面只管商品、订单、权益、资源绑定和鉴权。

## 1. 总体架构

```text
           用户浏览器                         用户的客户端（Claude Code / SDK / curl）
               │ /  /api/*                          │ /v1/*   Bearer sk-pt-…
               ▼                                    ▼
┌──────────────────────────── seat-hub（Go 单二进制）────────────────────────────┐
│ web（React SPA，go:embed）  portal API（/api）           gateway（/v1 透传）          │
│                           ├ auth / 商品 / 订单 / 支付   ├ 门面 key → vm2api key 换头 │
│                           ├ 权益 / 资源 / 托管上号        ├ 用户级并发闸               │
│                           └ 用量视图（读 vm2api）        └ SSE / WebSocket 原样透传   │
│ provision（后台任务）：建密钥与账号池、建槽、分配/回收租赁号、到期回收、同步并发与范围  │
│ SQLite                                                                              │
└──────────────────────────────────┬─────────────────────────────────────────────────┘
                                   │ Master key：/api/panel/*；用户 sk-vm-…：/v1/*
                                   ▼
                                vm2api
```

- 门面只用 vm2api 的 **Master key** 调 `/api/panel/*`，永不下发给浏览器。
- 用户流量：门面 key `sk-pt-…` → 查出该 key 对应的 vm2api 密钥 `sk-vm-…` → 换掉鉴权头 → 透传到 vm2api `/v1/*`。
- **账号体系分离**：门面用户只存在于门面。vm2api 里只有一个「seat-hub 服务账号」（role=`user`），门面名下所有号和所有上游密钥都归它。vm2api 对 `user_id` 归属的密钥只会调度到该用户名下的 VM（`ownerScopeFromApiKey`），所以 Master key、其他渠道的密钥都碰不到门面的号。
- **按密钥限定可调用的号**：每个门面用户在 vm2api 有一个账号池（`vm_pool_id`），成员是他当前持有的号。门面密钥要么绑定这个池（「全部账号」），要么用白名单只放行勾选的号（「指定账号」）。门面之间的用户隔离完全靠池成员和白名单，由 vm2api 强制执行；号被收回前先移出池和白名单。详见 [KEY_SCOPE.md](KEY_SCOPE.md)。

## 2. vm2api 原语映射

| 门面概念 | vm2api 原语 |
|---|---|
| 服务账号 | 首次需要时 `POST /api/panel/users`（role=user，`vm_create_quota=0`）建一个，id 存 `settings.vm2api_service`。**不删除**（删除会把 `admin_assigned` 槽收回平台池） |
| 门面用户 | 不在 vm2api 建用户；只建一个账号池 `POST /api/panel/vm-pools {name:"sh-…-u<id>", vm_ids}`，成员随持有的号同步（`PATCH /vm-pools/:id`） |
| 门面 API key | `POST /api/panel/api-keys {name, user_id:<服务账号>, group_type:"anthropic", vm_pool_id \| allowed_vms, max_concurrency}`，明文只返回一次 → AES-GCM 加密落库。门面绝不发出 `group_type:"all"` 的密钥 |
| 租赁账号 | 平台池里的 Claude VM，`PATCH /api/panel/vms/:id/owner {user_id:<服务账号>}` 并加入该用户的账号池；到期先移出池和白名单，再 `{user_id:null}` 收回平台池 |
| 托管席位 | 门面以 admin 身份 `POST /api/panel/vms/create`（模板来自后台设置）→ `PATCH owner` → `POST allocate-proxy` → OAuth 授权链接上号。到期/删除：先调度关，宽限期后 `DELETE /vms/:id`（凭证随槽销毁），**绝不**收回平台池 |
| 并发 | 门面网关按用户计数（权威）；同时把每个 vm2api key 的 `max_concurrency` 同步为用户总并发（兜底） |
| 账号真实用量 | `GET /api/panel/vms/:id`：5h/7d 利用率与重置时间、`estimate_*`、`billing.today/window_5h/window_7d`、`usage_stats`（30 天）、`cred_status`、`schedule_state`、席位占用 |
| 用户真实用量 | 按该用户的上游密钥汇总：`GET /api/panel/billing?group_by=key`、`GET /api/panel/request-logs?api_key_id=`、`GET /api/panel/api-keys/:id/stats` |

vm2api 文档与源码（另一个仓库，只读参考）：`docs/PANEL_API.md`、`docs/API.md`、`docs/OAUTH.md`、`src/lib/admin/panel-routes.mjs`、`src/lib/admin/api-keys.mjs`、`src/lib/admin/resource-owner.mjs`。

## 3. 计费维度与权益

三种商品 `kind`：

| kind | 含义 | 一个计量单位 |
|---|---|---|
| `concurrency` | 用户级并发上限加量 | 1 路并发 |
| `hosted_seat` | 可托管的自有 Claude 账号数 | 1 个席位 |
| `rental_account` | 独占租用平台 Claude 账号数 | 1 个账号 |

商品 = `kind` + `unit_quantity`（每份包含几个单位）+ `duration_days` + `price_cents`（CNY 分）。同一 kind 可上架多个规格（如 7 天 / 30 天 / 90 天，或「5 路并发包」）。

- **预付周期订阅**：每笔支付成功的订单生成一条 `subscription`（kind、quantity、starts_at、expires_at）。用户某 kind 的权益 = 所有 `active` 订阅 quantity 之和。
- **续费**：下单带 `renew_subscription_id` → 支付后把该订阅 `expires_at` 顺延 `duration_days`（若已过期则从现在起算）。续费份数 = `subscription.quantity / product.unit_quantity`，不能整除返回 400。
- **有效并发** = `settings.base_concurrency` + Σ并发订阅 + `concurrency_per_hosted_seat` × 活跃托管资源数 + `concurrency_per_rental_account` × 活跃租赁资源数。后三项系数后台可配（默认 base=1，其余 0）。
- **并发 0 = 不允许调用**，不是「无限」。网关对有效并发为 0 的用户直接返回 429 `concurrency_limit_exceeded`；同步到 vm2api key 的 `max_concurrency` 下限为 1（vm2api 用 0 表示不限，门面绝不写 0）。
- **库存**（仅 `rental_account`）：可租 = 租赁池里 `available` 且 vm2api 侧健康（`schedule_state` 为 `on`/`restricted`、凭证可用）的 VM 数 − 未过期待支付订单的预留数。下单时校验，并在订单未支付期间（30 分钟）按订单 `expires_at` 预留。

## 4. 资源（账号）状态机

`resources` 一行 = 用户名下一个具体账号（一个 vm2api VM）。

托管（`kind=hosted`）：

```text
provisioning ──建槽/归属/分配代理──▶ awaiting_auth ──粘贴授权码──▶ bootstrapping ──初装成功──▶ active
     │失败                              │链接过期→重新生成            │初装失败（可重试）
     ▼                                  ▼                            ▼
   failed（可重试）                 awaiting_auth                 failed
active ──凭证失效（vm2api 不可调度 oauth_*）──▶ needs_reauth ──重新授权──▶ bootstrapping
任意 ──订阅到期──▶ suspended（调度关，宽限 hosted_grace_hours）──▶ releasing ──DELETE VM──▶ released
任意 ──用户删除──▶ releasing ──▶ released
```

租赁（`kind=rental`）：`allocating → active →（到期）releasing →（owner=null）released`。宽限 `rental_grace_hours`（默认 0）内调度关。

托管号的 VM 名下永远只有该用户；释放 = 删除 VM。租赁号释放后回租赁池 `available`。

席位占用：托管资源中状态 ∉ {`released`} 的数量 ≤ 托管席位权益。订阅到期使权益 < 已占用时，按 `created_at` 倒序把超出的资源转 `suspended`。租赁同理（按订阅绑定，到期的订阅释放它绑定的那几个号）。

## 5. 托管上号（唯一方式：OAuth 授权链接）

- 只允许 `flavor: "claude_code"` 的授权链接上号。门面 API 与 vm2api client **都不暴露** sessionKey、Setup Token、凭证导入等任何其它入口；flavor 在 client 内写死。
- 用户视角三步（第 0 步由系统完成，见 [SELFCHECK.md](SELFCHECK.md)）：
  0. 建槽、分配代理后先跑**槽位自检**（一代理 IP 一号一槽、指纹不重复）。不通过则自动重建，最多 3 次；仍不过则转人工，用户看到「多次自检未通过」。
  1. 点「添加托管账号」→ 后台异步建槽、分配代理、自检，几十秒后出现「打开授权链接」按钮。
  2. 新窗口打开链接，登录要托管的 Claude 账号并授权，复制页面给出的授权码（形如 `code#state`）。
  3. 粘贴授权码提交 → 显示初装进度 → 完成即可用。链接 30 分钟过期，可一键重新生成。
- 后端：`generate-auth-url {flavor:"claude_code"}` → 存 `oauth_session`（auth_url、session_id、expires_at）；提交码 → `exchange-code`（同步）→ `bootstrapping`，后台任务轮询 `GET /vms/:id/official-cc-bootstrap` 直到成功/失败；成功后若槽仍调度关则 `POST /vms/:id/schedulable {schedulable:true}`。
- 用户界面不出现 VM、代理、槽位、种子等概念；只展示账号标签、邮箱（脱敏）、套餐等级、状态、用量。

## 6. 网关

- 路径：`/v1/*`（含 `POST /v1/messages`、`/v1/chat/completions`、`/v1/completions`、`/v1/responses`、`GET /v1/responses` WebSocket、`GET /v1/models`、`GET /v1/usage`、`POST /v1/messages/count_tokens`）。按原路径转发到 `VM2API_BASE_URL`。
- 鉴权：`Authorization: Bearer sk-pt-…` 或 `x-api-key`。缺失 401 `missing_api_key`；无效 401 `invalid_api_key`；用户停用 403 `user_disabled`；没有任何 `active` 资源 403 `no_active_accounts`；「指定账号」的密钥所选账号都不可用 403 `key_no_accounts`。错误体用 Anthropic 形状 `{"type":"error","error":{"type":"…","message":"…"}}`，`/v1/chat/completions`、`/v1/completions`、`/v1/responses` 用 OpenAI 形状 `{"error":{"message","type","code"}}`。
- 换头：删掉入站 `Authorization`、`x-api-key`、`Cookie`、hop-by-hop 头、**所有 `x-kin-*`**（vm2api 的调试、指定槽位、改写等内部控制头，客户端不能碰）、`x-forwarded-*`；写 `Authorization: Bearer sk-vm-…`；其余（`anthropic-*`、`x-session-id`、`x-claude-code-session-id`、`X-Request-ID`、`content-type`、`accept`…）原样带上。响应头原样回（去 hop-by-hop）。
- 流式：请求体不缓冲；响应逐块 flush（`FlushInterval: -1`）；客户端断开即取消上游。WebSocket Upgrade 原样桥接，一条连接占 1 路并发直到关闭。
- 并发：每用户内存计数器，满了立即 429 `concurrency_limit_exceeded`（带 `Retry-After: 1`）；可选排队 `gateway.queue_timeout_ms`（默认 0 = 不排队）。单实例部署；多实例需换成共享计数（不在 MVP）。
- 不做本地计量。计费只看 vm2api。

## 7. 支付

商品页点「购买」→ 建订单 → 跳支付 → 回调/轮询到 `paid` → `provision` 履约 → `fulfilled`。

- 支付提供方插件接口：`Create(order) → {type: redirect|qrcode|manual, url, qr, instructions}`；`Notify(req) → {order_no, trade_no, amount_cents, paid}`，验签 + 金额校验 + 幂等。
- 内置：`epay`（易支付 v1：`submit.php` 跳转、MD5 签名、异步通知 `notify_url` 回 `success`）、`manual`（展示后台配置的收款说明，管理员在后台「标记已支付」）、`mock`（仅 `PORTAL_DEV=1`，下单即可点「模拟支付」）。
- 订单状态：`pending → paid → fulfilled`；`pending → cancelled | expired`（30 分钟未支付）；`paid → fulfill_failed`（后台「重新履约」可重试）。
- 「30 分钟」只是**发起支付**的窗口，不是**入账**的门槛：易支付回调延迟、人工转账晚到，只要订单仍是 `pending`（含已过期 `pending`）都能入账；管理员也可对 `expired` 订单人工确认收款补单。
- `paid → fulfilled` 由持久任务 `fulfil_order:<订单ID>` 完成（dedupe 保证不重复履约），任务用独立 context 执行：支付回调断开或进程重启不会留下「已付款永不履约」的订单。
- 履约失败写 `fulfill_failed`，不会回滚支付；后台重试或自动退款的路径都从这里开始。

## 8. 用量视图（全部来自 vm2api）

- 我的账号：每个 `active/needs_reauth/suspended` 资源 → `vm2api.Client.GetAccountUsage(vmID)`，后端缓存 30s。展示：5h/7d 利用率 + 重置倒计时、当前 5h/7d 窗口花费与 token、今日、30 天日用量、按模型、席位占用、状态。
- 我的用量：`UserBilling`（区间汇总、按 key/按账号）、`UserRequestStats`、`UserRequestLogs`（分页明细：时间、模型、输入/输出/缓存 token、费用、状态、耗时、key 名）。门面把 vm2api key id 映射回门面 key 名；不返回出站报文、上游错误原文、VM 内部字段。
- 费用口径：vm2api 官方价（USD）。门面不重算价格。

## 9. 数据与安全

- SQLite（`modernc.org/sqlite`，纯 Go，无 CGO），WAL。迁移文件 `internal/store/migrations/*.sql` 按序执行。
- 密码 argon2id。会话 Cookie `portal_session`（HttpOnly、SameSite=Lax、Secure 跟随 `PORTAL_PUBLIC_BASE_URL` 是否 https），7 天。写操作要求 `Content-Type: application/json`（挡表单 CSRF）。登录限速。
- 门面 key：`sk-pt-` + 40 位 base62；落库 `HMAC-SHA256(PORTAL_SECRET_KEY, key)` 与前 10 位展示前缀。
- `vm2api` 密钥明文、易支付商户密钥：AES-256-GCM（密钥由 `PORTAL_SECRET_KEY` 派生）。服务账号的密码随机生成、不落库（没人用它登录）。
- 管理员：首次启动由 `PORTAL_ADMIN_EMAIL` / `PORTAL_ADMIN_PASSWORD` 灌入。

## 10. 配置（环境变量）

| 变量 | 默认 | 说明 |
|---|---|---|
| `PORTAL_LISTEN` | `:8790` | |
| `PORTAL_PUBLIC_BASE_URL` | 空 | 对外地址，展示给用户的 base_url，支付回调地址 |
| `PORTAL_DB_PATH` | `./data/portal.db` | |
| `PORTAL_SECRET_KEY` | 必填 | ≥32 字节随机串 |
| `PORTAL_ADMIN_EMAIL` / `PORTAL_ADMIN_PASSWORD` | 空 | 首个管理员 |
| `PORTAL_DEV` | 空 | `1` 开 mock 支付、宽松 Cookie |
| `VM2API_BASE_URL` | 必填 | 如 `http://127.0.0.1:8787` |
| `VM2API_MASTER_KEY` | 必填 | vm2api `VM2API_API_KEY` |
| `PORTAL_MAX_BODY_MB` | `128` | 网关请求体上限 |

## 11. 代码布局与所有权

```text
cmd/seat-hub/            主程序（装配、迁移、后台任务、静态资源）
cmd/mock-vm2api/       本地联调用的 vm2api 假服务
internal/config/       环境变量
internal/store/        SQLite、迁移、各表 repo
internal/auth/         密码、会话、中间件
internal/catalog/      商品、库存
internal/order/        订单、续费、过期
internal/payment/      支付提供方（epay / manual / mock）
internal/provision/    后台任务队列、履约、托管上号、租赁分配、到期回收、并发同步
internal/usage/        用量视图（调 vm2api 并脱敏映射）
internal/api/          HTTP handlers（/api/*）
internal/gateway/      /v1 透传 + 并发闸
internal/vm2api/       vm2api 客户端（Client 接口 + HTTP 实现）+ fake
internal/secret/       AES-GCM、HMAC
web/                   React 前端
```
