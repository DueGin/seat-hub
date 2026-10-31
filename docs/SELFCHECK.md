# 槽位自检

上号前必须通过自检。自检不过的槽位**不允许上号**，系统自动重建该槽位后重新自检，最多重建 3 次；3 次仍不过则标记为人工处理。

## 为什么要自检

vm2api 的槽位默认配置并不满足「一 IP 一号一槽」：

- 代理池默认 `bind_limit=5`（一个出口 IP 最多绑 5 个槽）。
- 本地出口（`scheme=local`）的所有槽共用所在 VPS 的公网 IP。
- 指纹由 vm2api 生成，但 `reset` / `reset-fingerprint` 之后可能与其它槽撞 hostname / device_id / guest_machine_id。

这些情况会让多个 Claude 账号从同一出口、同一套设备指纹登录，是风控的直接特征。门面层必须自己兜住。

## 规则

一次自检对某个槽位运行以下检查（`code` 为稳定标识）：

| code | 判定 | 失败类别 |
|---|---|---|
| `proxy_bound` | 槽位已绑定出口，且 `scheme=socks5`（`local` 表示与同机其它槽共用 VPS 出口，按「一 IP 一槽」不合格；设置 `selfcheck_require_dedicated_proxy=false` 时跳过） | rebuild |
| `proxy_healthy` | 代理 `enabled=true` 且 `status` 不是 `dead`/`fail`，无 `blocked_reason` | rebuild |
| `proxy_exclusive` | 该代理 `proxy_id` 上没有其它槽位（vm2api 侧 `bind_limit=1` 的兜底校验） | rebuild |
| `exit_probe` | `POST /vms/:id/exit-probe` 成功并返回出口 IP | rebuild |
| `exit_ip_unique` | 出口 IP 不等于本系统任何其它槽位的出口 IP | rebuild |
| `exit_geo_timezone` | 出口地理时区与槽位时区一致（`selfcheck_require_timezone_match=false` 时跳过） | rebuild |
| `fingerprint_present` | `fingerprint.device_id`、`guest_machine_id`、`hostname` 均非空 | rebuild |
| `fingerprint_unique` | 上述三项与其它槽位均不重复 | rebuild |
| `account_unique` | 槽位上的 Claude 账号（`account_uuid` 或 email）未出现在其它槽位（上号后才有值） | manual |

判定：

- 任一条 `rebuild` 类失败 → 自动重建（`POST /vms/:id/reset {mode:"traits"}`，重新生成指纹 + 清空家目录；保留 ID 与代理），重建后重跑自检。
- 重建次数达到上限（默认 3，`selfcheck_max_rebuilds`）仍失败 → `selfcheck_state=exhausted`，槽位置 `failed`，后台可见、需人工处理或换代理。
- `manual` 类失败（`account_unique`）不自动重建，直接拦下并提示「该账号已在其它槽位托管」，因为重建解决不了重复账号。
- 自检未通过（`pass` 以外任何状态）时，`GenerateAuthURL` / `SubmitAuthCode` 一律拒绝，返回 409 `selfcheck_failed`。

## 时机

1. **建槽后、授权前**（`ProvisionHosted` 分配完代理后立即跑）。失败先重建再跑，最多 3 轮。
2. **生成授权链接前**（`GenerateAuthURL`）。距上次 `pass` 超过 `selfcheck_recheck_minutes`（默认 30）则重跑一次；仍不过则走上面的重建流程。
3. **提交授权码前**（`SubmitAuthCode`）。同上，防止用户在等待期间代理变了。
4. **租赁池入池**（`POST /api/admin/rental-pool`）。入池即跑一次，不过的 VM 不允许入池（409 `selfcheck_failed`，返回失败项）。
5. **后台巡检**：每 `selfcheck_recheck_minutes` 对「非 released 且有 VM」的槽位跑一次；失败的置 `fail` 并写 `slot_checks`，不自动重建（避免动正在服务的槽），由后台「资源」页展示并允许手工重建/重跑。

## 数据

- `resources.selfcheck_state`：`pending | pass | fail | exhausted`；`selfcheck_attempts` 记录已重建次数；`selfcheck_checked_at`、`selfcheck_summary`（最近一次结果 JSON 摘要）。
- `slot_checks`：每次检查一行，`checks` 为 `[{code,ok,message}]`。
- `rental_pool.selfcheck_state` / `selfcheck_checked_at`：入池时写入。

## 接口

- `GET /api/accounts/{id}` 的 `account` 增加 `selfcheck: {state, attempts, checked_at, summary, checks[]}`。
- `POST /api/admin/accounts/{id}/selfcheck`：立即重跑；`{rebuild:true}` 时先重建再跑。
- `GET /api/admin/rental-pool` 行增加 `selfcheck_state`。
- `POST /api/admin/rental-pool/{vm_id}/selfcheck`：重跑入池自检。
- 用户侧托管向导在 `selfcheck_state=pending/fail` 时展示「正在检查运行环境」与重建进度（第 n/3 次），不展示授权链接。

## 与 vm2api 的配合

- 代理池的 `bind_limit` 由运维在 vm2api 设为 1（门面 `/api/admin/settings` 里有提示与一键检查项 `proxy_exclusive`，不自作主张改 vm2api 配置）。
- 出口探测与指纹采集都经 vm2api 的出站代理，门面本身不直连 Anthropic。
