# vm2api-portal 工作区约定（所有 agent 必读）

多 agent 并行开发**同一个工作树**。遵守下面规则，否则会互相打断。

## 语言与风格

- 代码注释：英文。UI 文案、文档、提交信息：简体中文。
- Go：`gofmt`；错误信息小写、不重复包名；所有跨包错误用 `fmt.Errorf("...: %w", err)` 包装。
- 前端：TypeScript strict，函数组件，路径别名 `@/`，UI 组件放 `web/src/components/ui`，页面放 `web/src/pages`，API 封装只放 `web/src/lib/api.ts`。
- 金额一律整数分；时间一律 RFC3339 UTC 字符串落库与传输；对外展示时前端转本地时区。

## 并行协作规则（重要）

1. **只改自己名下目录**（见下面「所有权」）。需要改别人的文件，先 `orca orchestration send` 给对方或上报协调者，不要直接改。
2. **不要 `git commit` / `git push` / 切分支 / `git checkout`**。协调者统一提交。
3. 不要改 `go.mod` / `go.sum`（依赖已预装：`modernc.org/sqlite`、`golang.org/x/crypto`、`github.com/coder/websocket` 如需）。确实需要新依赖，先上报协调者。
4. 只跑本包测试：`go test ./internal/<你的包>/...`。不要跑 `go build ./...` 后因为别人半成品报错而去改别人的文件——那不是你的问题。依赖公共包的编译错误可以短暂存在。
5. 不要改 `docs/DESIGN.md`、`docs/PORTAL_API.md`、`internal/vm2api/client.go`、`internal/gateway/contract.go`、`internal/store/migrations/*.sql`。这些是契约，发现契约有问题 → 上报协调者。
6. 不要启动长期驻留进程（`go run` 起服务、watch、dev server 常驻）。需要联调时在测试里 `httptest` 起临时服务。
7. 不要写入 `data/`、不要提交二进制。

## 所有权

| 路径 | 负责人 |
|---|---|
| `docs/DESIGN.md`, `docs/PORTAL_API.md`, `internal/vm2api/client.go`, `internal/vm2api/errors.go`, `internal/gateway/contract.go`, `internal/store/migrations/`, `go.mod` | 协调者（Claude） |
| `internal/vm2api/http.go`, `internal/vm2api/fake.go`, `internal/vm2api/*_test.go`, `cmd/mock-vm2api/`, `internal/provision/vm2api_contract_test.go` | Worker A（codex） |
| `internal/catalog/`, `internal/order/`, `internal/payment/` | Worker B（codex） |
| `internal/provision/`（除 `vm2api_contract_test.go`）, `internal/usage/` | Worker C（kimi） |
| `web/` | Worker D（kimi） |
| `internal/api/`, `internal/gateway/`, `internal/app/`, `internal/config/`, `internal/secret/`, `internal/auth/`, `cmd/portal/`, `Dockerfile`, `deploy/` | 协调者（Claude） |
| `internal/store/`（除 migrations） | 协调者（Claude） |

## 构建与验证

```bash
go build ./cmd/...            # 只保证自己的命令能编
go test ./internal/<pkg>/...  # 自己包的测试
cd web && pnpm install && pnpm build   # 前端产物
```

## 参考

- `docs/DESIGN.md`：架构、状态机、计费口径、托管上号流程、网关行为。
- `docs/PORTAL_API.md`：前后端 HTTP 契约（唯一事实来源）。
- vm2api（只读源码）：`/Users/duegin/project/2api/vm2api`，重点 `docs/PANEL_API.md`、`src/lib/admin/panel-routes.mjs`、`src/lib/admin/api-keys.mjs`、`src/lib/admin/resource-owner.mjs`。
