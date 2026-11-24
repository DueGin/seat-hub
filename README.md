<p align="center"><img src="docs/logo.svg" width="72" height="72" alt="seat-hub"></p>

# seat-hub

Claude 账号托管与租赁的门面系统，背后对接 [vm2api](https://github.com/dofastted/vm2api)。

用户在这里购买并发、托管席位或租赁账号，然后用 `baseurl + apikey` 接入。请求经门面原样转发给 vm2api，用量和计费数据也全部来自 vm2api，门面自己不记账。

- **托管**：用户通过 OAuth 授权链接把自己的 Claude 账号接入。上号前会先做槽位自检，保证一个出口 IP、一个账号、一个槽位，指纹也不重复。
- **租赁**：从租赁池里分配平台账号，到期后自动回收。
- **账号隔离**：每个门面用户对应一个 vm2api 用户；API 密钥可使用名下全部账号，也可指定账号。
- **用量**：总览、用量分析，以及按账号查看的使用记录。
- **工单**：用户提交问题（Markdown + 图片），管理员回复，邮件通知双方。
- **预约**：未开售的商品先收集意向邮箱，开售时一键群发通知。
- **收款**：易支付、支付宝当面付、人工转账三选一。
- **管理后台**：商品、订单、用户、租赁池、资源、任务、站点设置，以及站内一键更新和回滚。
- **界面**：深色和浅色两套主题，默认跟随系统。

## 部署

```bash
mkdir -p /opt/seat-hub && cd /opt/seat-hub
curl -fsSLO https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/.env.example -o .env
vi .env && docker compose up -d
```

需要 vm2api v1.4.041 或更新版本。反向代理、在线更新和发版流程见 [docs/DEPLOY.md](docs/DEPLOY.md)。

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
- [API Key 账号范围](docs/KEY_SCOPE.md)
- [工单、预约、当面付约定](docs/FEATURES_V05.md)
- [部署与发版](docs/DEPLOY.md)
- [更新记录](CHANGELOG.md)
