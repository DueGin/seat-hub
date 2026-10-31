# 部署

单二进制 `vm2api-portal`，内含前端静态资源，前面直连 vm2api 控制面。

## 前置

- vm2api 已在同机或内网可达（`VM2API_BASE_URL`），并持有其 Master key（`VM2API_API_KEY`）。
- 托管 / 租赁都需要 vm2api 侧存在系统镜像、`kin-computer`、代理池（`proxy_pool.free > 0`）；否则建槽或分配代理会失败并进入 `failed`，后台「资源」页可重试。

## 1. 构建

```bash
git clone <repo> && cd vm2api-portal
make build          # 会先 pnpm -C web build 并拷进 internal/webdist/dist
# 产物：bin/vm2api-portal
```

交叉编译 linux/amd64：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "-s -w" -o bin/vm2api-portal ./cmd/portal
```

（web/dist 需先构建好；`modernc.org/sqlite` 是纯 Go，无需 CGO。）

## 2. 配置

复制 `.env.example` 为 `.env`，至少改这几项：

| 变量 | 说明 |
|---|---|
| `PORTAL_SECRET_KEY` | ≥32 字节随机串。**用于加密 vm2api 密钥与派生会话 HMAC，之后不可更换**（换了旧密钥与旧会话全部失效） |
| `PORTAL_PUBLIC_BASE_URL` | 对外地址，例如 `https://api.example.com`。支付回调与用户看到的 base_url 都用它 |
| `PORTAL_ADMIN_EMAIL` / `PORTAL_ADMIN_PASSWORD` | 首次启动创建管理员；以后改密码走界面 |
| `VM2API_BASE_URL` | 例如 `http://127.0.0.1:8787` |
| `VM2API_MASTER_KEY` | vm2api 的 `VM2API_API_KEY` |
| `PORTAL_DB_PATH` | 默认 `./data/portal.db`，目录不存在会自动创建 |

生成密钥：`openssl rand -base64 48`。

## 3. systemd

```bash
sudo install -D -m 0755 bin/vm2api-portal /opt/vm2api-portal/vm2api-portal
sudo cp -r deploy .env /opt/vm2api-portal/
sudo cp /opt/vm2api-portal/deploy/vm2api-portal.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin portal || true
sudo chown -R portal:portal /opt/vm2api-portal
sudo systemctl daemon-reload && sudo systemctl enable --now vm2api-portal
```

## 4. nginx

用户端与协议口是同一个域名下的不同前缀，必须关掉缓冲（SSE / WebSocket）：

```nginx
server {
  listen 443 ssl http2;
  server_name api.example.com;

  client_max_body_size 128m;

  location /v1/ {
    proxy_pass http://127.0.0.1:8790;
    proxy_http_version 1.1;
    proxy_set_header Upgrade $http_upgrade;          # /v1/responses WebSocket
    proxy_set_header Connection $connection_upgrade;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 900s;
    proxy_send_timeout 900s;
  }

  location / {
    proxy_pass http://127.0.0.1:8790;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-Proto $scheme;
  }
}
```

`map $http_upgrade $connection_upgrade { default upgrade; '' close; }` 放在 `http {}` 里。

## 5. 首次配置

1. 用管理员登录 → 「设置」：站点名、注册开关、基础并发、每席位/每账号附赠并发、宽限期、托管建槽模板（vm2api `POST /vms/create` 的 body，例如 `{"runtime_type":"docker"}`）。
2. 「商品」：三类商品各上架至少一个（并发 / 托管席位 / 租赁账号）。
3. 「租赁池」：从候选 VM 里选平台账号入池，设为 `available`。
4. 支付：填易支付网关、商户号、密钥；不开在线支付时保留「人工转账」。

## 6. 验证

```bash
curl -s https://api.example.com/api/public/products | jq
# 管理员拿到 key 后
BASE=https://api.example.com
KEY=sk-pt-...
curl -s $BASE/v1/models -H "Authorization: Bearer $KEY" | jq
curl -s -X POST $BASE/v1/messages -H "Authorization: Bearer $KEY" -H 'content-type: application/json' \
  -d '{"model":"claude-sonnet-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}'
```

## 7. 运维

- 备份：停服务后拷 `PORTAL_DB_PATH`（SQLite + WAL：`.db`、`.db-wal`、`.db-shm` 三件一起）。
- 后台「任务」页能看到堆积与失败的任务，可单条重试。
- 升级：替换二进制后 `systemctl restart`；迁移文件随二进制走，启动时自动执行。
- 与 vm2api 的关系：门面只调它的 `/api/panel/*` 与 `/v1/*`；vm2api 升级或重启期间，门面「我的账号」用量会显示为空（不伪造 0），下单与支付不受影响。

## 8. 回滚

`PORTAL_SECRET_KEY` 不变即可回滚到旧二进制；库结构只增不删，旧版本遇到新表会忽略。
