# API Key 账号范围（账号体系分离）

本文是 `api_keys` 账号范围改造的约定，所有实现以此为准。

## 目标

- **账号体系分离**：门面用户只存在于门面。vm2api 里**不再**为每个门面用户建对应用户，只有一个「seat-hub 服务账号」。
- **按 key 控制可调用的账号**：每个门面 API Key 都显式限定能调哪些号，限定在 vm2api 侧强制执行。

## vm2api 侧的对象

| 对象 | 数量 | 说明 |
|---|---|---|
| 服务账号（vm2api 用户，role=user） | 每个门面部署 1 个 | 首次需要时自动创建，id 存在 `settings` 表 `vm2api_service`：`{"user_id","username"}` |
| 号（VM） | — | 门面名下的号（租出去的、托管的）**归属服务账号**。未出租的租赁池号保持无主（平台池） |
| 账号池（vm-pool） | 每个门面用户 1 个 | 成员 = 该用户当前可调度的号。名字 `sh-<服务账号 id 前 6 位>-u<门面用户 id>`，id 存在 `users.vm2api_pool_id` |
| 上游 key | 每个门面 key 1 个 | `user_id` = 服务账号；账号范围见下 |

vm2api 的硬约束（源码已核实）：

- 一个号只能属于一个账号池；池名唯一、1–40 字符；成员必须是存在的槽位；有 key 引用的池不能删除。
- key 的 `vm_pool_id` 指向的池不存在、被停用或为空时，请求一律拒绝（默认拒绝）。
- key 用白名单时 `group_type=anthropic` + `allowed_vms`，**白名单不能为空**（vm2api 报 `key_vms_required`）。
- key 什么范围都不设时 `group_type=all`，即「全部可调」——**门面绝不能发出这样的 key**。
- 调度时两个条件同时满足才可用：号归属 key 的 `user_id`（服务账号），且 key 的范围允许该号。

## key 的两种模式

| 模式 | `api_keys.scope_mode` | 上游范围 | 账号变化时 |
|---|---|---|---|
| 全部账号（默认） | `all` | `vm_pool_id` = 用户的账号池 | 只改池成员，一次生效 |
| 指定账号 | `selected` | `group_type=anthropic` + `allowed_vms` = 勾选的号 ∩ 可调度的号 | 逐个更新该用户的指定模式 key |

上游请求体：全部账号 = `{"group_type":"anthropic","vm_pool_id":"pool_xxxxxxxx"}`；指定账号 = `{"group_type":"anthropic","allowed_vms":[...],"vm_pool_id":null}`。

- `api_keys.scope_accounts`：指定模式下勾选的 `resources.id`（JSON 数组）。
- 「可调度的号」= 该用户 `status IN ('active','needs_reauth','suspended')` 且有 `vm_id` 的资源（`provision.SchedulableVMs`）。号是否真的可用（凭证、额度、调度开关）仍由 vm2api 判断；暂停的号已被设为不可调度。
- 指定模式下若勾选的号全部不可调度（例如都已到期释放）：上游 key **停用**，范围改为该用户的账号池（不保留过期白名单，万一被误启用也只能调自己的号）；门面网关以 `key_no_accounts` 拒绝。重新勾选可用的号后自动恢复。
- 创建或修改为指定模式时，勾选的号必须属于该用户且至少一个可调度，否则 422。

上游 key 是否启用 = 门面 key 为 active **且** 用户为 active **且** 范围可用。

## 号归属变化的顺序（防串号）

- **加入**（租赁分配、托管建槽）：先把号归属改为服务账号，再同步该用户的范围（加入池）。
- **移除**（到期释放、删除托管）：资源先标为 `releasing` 且不可调度 → **同步该用户的范围**（移出池、移出白名单）→ 全部成功后才删除槽位或把号交还平台池。同步失败则释放报错并重试，号一直不可调度。

## 并发与加锁

- 每个门面用户一把「范围锁」（`provision.LockScope`）。门面 key 的创建、修改从计算范围到写入数据库全程持有；`SyncUserScope` 也取同一把锁，所以释放流程的同步一定能看到刚建好的 key。
- 加锁顺序：`provision.Service.mu` → 范围锁 → 服务账号锁。范围相关代码不取 `service.mu`。持有范围锁时不能调 `SyncUserScope`。

## 对账（每 10 分钟，`scope_reconcile` 任务）

1. 对每个有 key 或有资源的用户执行 `SyncUserScope`：池成员、每个 key 的范围、启用状态、归属与门面记录一致，不一致就改回来。
2. 服务账号名下有门面不认识的上游 key：停用。
3. 门面名下的号（未释放资源）归属不是服务账号：改回服务账号。

对账开始时会先把启动时没迁完的用户再迁一次；还没迁完的用户不做第 3 步（他的旧 key 还要靠旧归属调用）。管理后台「总览」的「立即重新检查」可以马上触发一次对账。

## 迁移（启动后执行一次，`scope_migrate` 任务，幂等）

针对旧数据（`users.vm2api_user_id` 非空，或 `api_keys.upstream_owner` 为空的 key）：

1. 确保服务账号和该用户的账号池存在。
2. 为每个旧 key 在服务账号下新建上游 key（范围、并发同原设置），替换 `vm2api_key_id` / `vm2api_secret_enc`，把旧上游 key id 追加到 `previous_vm2api_key_ids`，然后删除旧上游 key。用户手里的 `sk-pt-` key 不变。
3. 把该用户名下的号归属改为服务账号。
4. 停用旧的 vm2api 用户（不删除），清空 `users.vm2api_user_id`。

## 外部账号池

vm2api 里一个号只能属于一个账号池。有人在 vm2api 面板手动建了账号池、把门面要用的号放了进去（门面之外的池，称「外部账号池」），门面就没法把这个号放进用户的账号池，vm2api 返回 409 `vm_in_other_pool`。只有管理员能处理：在 vm2api 面板把号移出那个池。在此之前：

- 该用户的范围保持原样：不放宽，也不改一半。迁移卡在这一步时，用户继续按升级前的方式调用。
- 错误会写明是哪个池，例如「账号 vm-08 在 vm2api 账号池「GUO」(pool_xxxxxxxx) 里……」。
- 任务不会因此失败重试，而是记为「范围警告」（`settings.scope_warnings`），显示在管理后台「总览」。每次对账都重新生成这份警告，处理好之后最多 10 分钟自动消失、自动补完迁移或同步。
- 在外部账号池里的号：候选入池列表里标出且不能勾选；入池接口返回 409 `vm_in_foreign_pool`；已在租赁池里的会在列表里标出；不计入可售库存；分配租赁号时跳过。

外部账号池 = vm2api 里所有不是某个门面用户 `users.vm2api_pool_id` 的账号池（`provision.ForeignPoolVMs`）。

## 用量

不再按 vm2api 用户查询。门面用户的用量 = 其上游 key（当前 + `previous_vm2api_key_ids`）的用量：

- 合计与按 key 拆分：`/billing?group_by=key`（`UserBilling(ctx, "", …)`，不带 `user_id`），取该用户的 key。
- 按天、按模型、按账号、成功率、首字延迟：按 key 拉请求日志（`UserRequestLogs(ctx, "", LogQuery{KeyID: …})`）汇总。
- 请求明细：各 key 的日志按时间归并分页（只有一个 key 时直接透传分页）。
- 单个号的用量：按 `vm_id` 拉日志，时间限定在租期内，再只保留该用户 key 的记录。

## 数据库（`004_key_scope.sql`）

- `api_keys.scope_mode`、`api_keys.scope_accounts`、`api_keys.upstream_owner`、`api_keys.previous_vm2api_key_ids`
- `users.vm2api_pool_id`
