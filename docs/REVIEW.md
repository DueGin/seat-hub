# 评审与修复记录

两轮只读评审（共 63 条发现）与修复状态。评审是独立 agent 做的，修复按文件集分派，避免并行改同一文件。

## 结论速览

| 严重度 | 数量 | 状态 |
|---|---|---|
| critical | 3 | 已修（含一处重复报告） |
| high | 20 | 已修 / 部分由适配层批次收尾 |
| medium | 26 | 大部分已修，少量在剩余批次 |
| low | 14 | 已修 / 记录为已知限制 |

## 已修（关键项）

**库存恒 0（critical）** — `catalog.New(db, nil)` 从未装上健康探针，租赁商品永远 0 库存、无法下单。
修法：`catalog.Service.SetHealth` + `AvailableVMs` 在无探针时按「未知即可用」计数；`app.New` 用带 30s 缓存的 `cachedVMHealth` 接入 `vm2api` 的 `GetAccountUsage`。缓存是必要的：公开商品列表会遍历整个租赁池。

**api.Err 运行时无锁追加（critical）** — 每个请求错误都往包级切片 append，可被未鉴权请求无界放大并产生 data race。
修法：注册表加读写锁，同 sentinel + 同 code 去重。

**网关路径穿越（high）** — `/v1/../api/panel/login` 会被原样转发到 vm2api 管理面。
修法：`canonicalPath` 拒绝含 `.`/`..` 段的请求并清 `RawPath`；转发用规范化后的路径。

**WebSocket 升级必败（high）** — 101 响应被删掉 `Connection`，`ReverseProxy` 判定协议不符 → 502。
修法：101 时整组跳过 hop-by-hop 剥离。新增真实 hijack 上游的升级用例。

**并发 0 = 无限（high）** — 0 路并发被网关与 vm2api key 都当成不限。
修法：网关对 `<=0` 直接 429 `concurrency_limit_exceeded`；文档与后端同步语义（vm2api key 下限 1）。

**改密不撤销其它会话（high）** — `Sessions.DeleteOthers` 实现了却从未调用。
修法：`auth.Service.ChangePassword/RevokeOthers`，改密后保留当前会话、撤销其余。

**支付回调匿名写库（high）** — 未注册/离线 provider 也被写入 `payment_events`，raw 最大 64KB，无清理。
修法：不注册的 provider 直接拒绝且不落库；无订单号的验签失败不落库；raw 截断到 2048；新增 `PruneEvents`。

**登录限速内存无上限 / 反代后 IP 退化（medium）** — 桶表永不清理；`RemoteAddr` 在 nginx 后恒为回环。
修法：惰性清扫 + 上限 10000 + key 截断；`clientIP` 仅在直连对端是回环/私网时采信 `X-Forwarded-For` 的最后一跳。

**首个用户并发注册产生多个管理员（low）** — check-then-act。
修法：包级互斥包住「计数 + 创建」。

**部署类**
- `Dockerfile` 运行镜像先 `mkdir -p /data && chown`，否则 uid 10001 建不了 SQLite。
- 新增 `.dockerignore`，避免把宿主 `node_modules` 带进构建。
- systemd `ExecStart` 与 `DEPLOY.md` 安装路径统一为 `/opt/seat-hub/seat-hub`。
- `.env.example` 的 master key 与 mock 默认值对齐（`mock-master-key`），`make mock` 会加载 `.env`。
- `DefaultSite` 返回完整的 `payment` 两段文档，全新安装的设置页不再白屏。
- `base_url` 优先取 `PORTAL_PUBLIC_BASE_URL`，不再由请求 Host 决定。

**契约与前端**
- CSRF 防护改为严格匹配 `application/json`（原来 `text/json` 也能过）。
- 任务队列 payload 不再是二次编码的字符串。
- 备注长度上限前后端统一 40；订阅来源单号为空显示「—」；租赁池不再谎称「暂无租用人」。

## 验证记录

- `go build ./...`、`go test ./...`、`go vet ./...` 全绿；`go test -race` 在 gateway / auth / order / provision / portal / payment / app 通过。
- 端到端（进程内）：注册 → 建商品 → 下单 → mock 支付 → 履约 → 建门面 key → 网关透传，断言上游收到的是该用户的 vm2api 密钥、请求体原样转发。
- 真实二进制冒烟（`cmd/mock-vm2api` + `cmd/seat-hub`）：
  - 租赁池入池后商品库存由 0 变 1，下单可履约（原 critical 已修）。
  - 托管上号：创建 → 授权链接 → 粘贴授权码 → 初装 6 秒后转 `active`。
  - `/v1/models`、`/v1/messages` 经门面正常返回；`/v1/../api/panel/users` 返回 400（不再打到管理面）。
  - 全新库的 `GET /api/admin/settings` 返回完整 `payment.epay/manual` 结构。
- 前端：`pnpm exec tsc --noEmit`、`pnpm build` 通过。

## 已知限制（不在 MVP 内）

- 网关的并发计数在进程内内存中，单实例部署成立；多实例需要共享计数器。
- 门面 key 的并发上限变更依赖把有效并发同步到 vm2api key 上，同步失败时以门面侧为准（网关仍会拦截）。
- 租赁池的健康探测有 30 秒缓存，池里账号刚失效时可能多卖出一单，履约阶段会失败并进入可重试状态。
- 真实支付只接了易支付 v1（MD5 签名）与人工转账；其它网关需要新增 `payment.Provider` 实现。
- 未对接真实 vm2api 做端到端验证：托管建槽模板、代理分配、OAuth 换票与初装目前只在 Fake/mock 上跑过。
