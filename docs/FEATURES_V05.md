# v0.5 功能约定：工单、商品预约、支付宝当面付

本文是三个功能的约定，实现以此为准。

## 1. 收款方式：支付宝当面付 + 单选

- 支付配置文档（`site.payment`）新增 `active`：`"epay" | "alipay_f2f" | "manual"`，**同一时间只启用一种**收款方式。各方式的配置块独立保存，切换不丢配置。
- 旧数据没有 `active` 时：易支付已启用且配置完整 → `epay`，否则 → `manual`。
- 下单页只提供 `active` 那一种（开发模式另有「模拟支付」）。用别的方式下单返回 422 `payment_provider_unavailable`。
- 回调：所有**已配置**的方式都继续接收回调，保证切换前创建的未付订单还能到账。
- 支付宝当面付（`alipay_f2f`）配置块：`app_id`、`private_key`（应用私钥，PKCS1/PKCS8 PEM 或去掉头尾的 base64）、`alipay_public_key`（支付宝公钥，公钥模式；不支持证书模式）、`sandbox`（布尔，沙箱网关）。私钥按现有规则脱敏回显，`***` 表示保留原值。
  - 下单：`alipay.trade.precreate`（RSA2），`out_trade_no` = 订单号，金额单位元两位小数，`subject` = 商品名，`notify_url` = `<PUBLIC_BASE_URL>/api/payments/alipay_f2f/notify`，过期时间与订单一致。返回 `qr_code` → 前端显示二维码（`type: "qrcode"`）。
  - 回调：验 RSA2 签名、`app_id`、金额，`trade_status` 为 `TRADE_SUCCESS`/`TRADE_FINISHED` 才算到账；回复 `success`；幂等。
  - 主动查询兜底：用户在订单页等待时，若订单仍未支付且是当面付，最多每 5 秒调一次 `alipay.trade.query`，已支付就按到账处理（防回调丢失）。
  - 订单取消或过期：尽力调用 `alipay.trade.close`，失败不影响本地状态。

## 2. 邮件（SMTP）

- 设置存 `settings` 表 `mail`：`enabled`、`host`、`port`、`security`（`starttls` | `tls` | `none`）、`username`、`password`（AES-GCM 加密存储，接口回显 `***`）、`from_address`、`from_name`、`admin_emails`（管理员通知收件人，空 = 所有启用的管理员的登录邮箱）。
- 接口：`GET/PUT /api/admin/settings/mail`，`POST /api/admin/settings/mail/test {to}`（同步发送，直接返回 SMTP 错误信息）。
- 发送走发件箱（`mail_outbox` 表）+ 后台循环，失败按退避重试（最多 6 次），已发送的 30 天后清理。功能代码只调用 `mail.Service.Enqueue`，未配置邮件时直接丢弃、不报错。

## 3. 商品预约（只收集意向）

- 商品新增 `reservation_only`：开启后该商品**不能购买**，只能预约。下单返回 422 `product_reservation_only`。
- 公开接口：`POST /api/public/products/{id}/reservations {email, note?}`，**不需要登录**；登录用户自动关联账号、邮箱预填。同一商品同一邮箱只记一次（重复预约直接返回成功）。按 IP 限流。商品必须已上架且是预约商品。已配置邮件时给预约人发一封确认邮件。
- 管理端：预约列表（按商品、状态、邮箱筛选，每个商品的预约人数）、导出 CSV、删除；「通知预约用户」：给某商品所有「待通知」的预约发同一封邮件（标题、正文由管理员填写），发送后标记为「已通知」。
- 商品编辑器新增「需要预约（暂不开售）」开关；前台商品卡片显示「需预约」，按钮变为「预约」。

## 4. 工单

- 用户：提交工单（分类：账号问题 / 订单与支付 / API 调用 / 其他；标题；正文 Markdown；可关联自己的订单或账号；可附图片），查看列表和详情、回复、关闭。管理员回复后显示未读。
- 状态：`open`（待处理，等管理员）→ 管理员回复后 `replied`（已回复，等用户）；用户回复 → 回到 `open`；任何一方可关闭 → `closed`；关闭后用户回复会重新打开。
- 图片附件：仅 png/jpeg/gif/webp（按文件内容识别），单张 ≤ 5 MB，每条消息最多 6 张；存到数据卷（`PORTAL_DB_PATH` 同目录下 `attachments/`）；只有工单所属用户和管理员能查看；未绑定到工单的上传 24 小时后清理。上传接口用 multipart，并要求请求头 `X-Seat-Hub-Upload: 1`（防 CSRF）。
- Markdown：前端渲染，不允许原始 HTML；链接新窗口打开（`rel="noopener noreferrer nofollow"`）；图片只显示本工单的附件地址，其他图片地址显示为链接（防止外链图片追踪）。编辑框有「编辑 / 预览」，支持粘贴和拖拽上传图片。
- 邮件通知：新工单和用户回复 → 管理员通知收件人；管理员回复 → 用户登录邮箱。邮件带工单链接。
- 管理端：工单列表（状态、分类、关键字、用户筛选，待处理数）、详情、回复、改状态；侧边栏显示待处理数。管理操作写审计日志。
