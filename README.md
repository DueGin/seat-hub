# vm2api-portal

Claude 账号托管 / 租赁门面，前面挂着 vm2api。用户购买并发、托管席位、租赁账号，拿 `baseurl + apikey` 接入，请求经门面透传到 vm2api；用量与计费数据全部来自 vm2api。

- 设计：[docs/DESIGN.md](docs/DESIGN.md)
- 前后端契约：[docs/PORTAL_API.md](docs/PORTAL_API.md)
- 协作约定：[AGENTS.md](AGENTS.md)

## 快速开始（开发）

```bash
cp .env.example .env      # 填 VM2API_BASE_URL / VM2API_MASTER_KEY / PORTAL_SECRET_KEY
go run ./cmd/mock-vm2api  # 另开一个终端：本地假 vm2api（:8788）
go run ./cmd/portal       # :8790
cd web && pnpm install && pnpm dev
```

生产：`pnpm -C web build` 后 `go build ./cmd/portal`，单二进制带前端静态资源。

## 状态

骨架与接口契约已落地，业务实现进行中。
