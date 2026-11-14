# seat-hub

Claude 账号托管与租赁的门面系统，背后对接 [vm2api](https://github.com/dofastted/vm2api)。

用户在这里购买并发、托管席位或租赁账号，然后用 `baseurl + apikey` 接入。请求经门面原样转发给 vm2api，用量和计费数据也全部来自 vm2api，门面自己不记账。

- **托管**：用户通过 OAuth 授权链接把自己的 Claude 账号接入。上号前会先做槽位自检，保证一个出口 IP、一个账号、一个槽位，指纹也不重复。
- **租赁**：从租赁池里分配平台账号，到期后自动回收。
- **用量**：总览、用量分析，以及按账号查看的使用记录。
- **管理后台**：商品、订单、用户、租赁池、资源、任务、站点设置，以及站内一键更新和回滚。
- **界面**：深色和浅色两套主题，默认跟随系统。

## 部署

```bash
mkdir -p /opt/seat-hub && cd /opt/seat-hub
curl -fsSLO https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/.env.example -o .env
vi .env && docker compose up -d
```

反向代理、在线更新和发版流程见 [docs/DEPLOY.md](docs/DEPLOY.md)。

## 本地开发

```bash
cp .env.example .env
go run ./cmd/mock-vm2api      # 本地模拟的 vm2api，监听 :8788
go run ./cmd/seat-hub         # 门面，监听 :8790
cd web && pnpm install && pnpm dev   # 前端开发服务器；不连后端时自带一套演示数据
```

运行测试：`go test ./...`、`pnpm -C web build`。

## 文档

- [设计](docs/DESIGN.md)：架构、状态机、计费口径、托管上号流程、网关行为
- [前后端接口](docs/PORTAL_API.md)
- [槽位自检](docs/SELFCHECK.md)
- [部署与发版](docs/DEPLOY.md)
- [更新记录](CHANGELOG.md)
