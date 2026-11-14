# 部署与发版

seat-hub 是单个程序，前端资源已经打包在里面。它前面接用户和 nginx，后面连 vm2api 的控制面。推荐用 docker compose 部署，这样可以在管理后台一键在线更新。

## 前置条件

- vm2api 已经在同一台机器或内网上运行，并且你有它的 `VM2API_API_KEY`（下文称 Master key）。
- 托管和租赁功能要求 vm2api 那边已经备好系统镜像、`kin-computer` 和代理池（`proxy_pool.free > 0`）。缺任何一项，建槽或分配代理都会失败，资源会停在 `failed` 状态，可以在后台「资源管理」页重试。
- vm2api 代理池的 `bind_limit` 建议设为 1，门面的槽位自检会拦下共用出口的槽位，详见 [SELFCHECK.md](SELFCHECK.md)。

## 1. 用 docker compose 部署（推荐）

```bash
mkdir -p /opt/seat-hub && cd /opt/seat-hub
curl -fsSLO https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/docker-compose.yml
curl -fsSL  https://raw.githubusercontent.com/DueGin/seat-hub/main/deploy/.env.example -o .env
chmod 600 .env
vi .env                 # 至少填写下表中的几项
docker compose up -d
docker compose logs -f  # 看到 "portal listening" 就是启动成功
```

| 变量 | 说明 |
|---|---|
| `PORTAL_PUBLIC_BASE_URL` | 对外地址，例如 `https://seat.example.com`。支付回调和用户看到的接入地址都用它；以 `https` 开头时 Cookie 会带 Secure 标记 |
| `PORTAL_SECRET_KEY` | 至少 32 字节的随机串，可以用 `openssl rand -base64 48` 生成。**设定后不能再改**：它用来加密存储的 vm2api 密钥和签发会话，一旦更换，已存的密钥和所有登录状态都会失效 |
| `PORTAL_ADMIN_EMAIL` / `PORTAL_ADMIN_PASSWORD` | 第一次启动时用它们创建管理员；之后改密码请在界面上操作 |
| `VM2API_BASE_URL` | vm2api 在同一台机器上时填 `http://host.docker.internal:8787` |
| `VM2API_MASTER_KEY` | vm2api 的 `VM2API_API_KEY` |

数据库和在线更新下载的程序都放在名为 `seat-hub-data` 的卷里（容器内的 `/data`）。容器以非 root 用户（uid 10001）运行，默认只监听 `127.0.0.1:8790`，对外访问要经过下一节的反向代理。

## 2. 反向代理（nginx）

用户端页面和 `/v1` 协议接口在同一个域名下。协议接口要用到 SSE 流式输出和 WebSocket，所以必须关闭缓冲，超时时间也要设长一些：

```nginx
map $http_upgrade $connection_upgrade { default upgrade; '' close; }   # 放在 http {} 里

server {
  listen 443 ssl http2;
  server_name seat.example.com;
  ssl_certificate     /etc/letsencrypt/live/seat.example.com/fullchain.pem;
  ssl_certificate_key /etc/letsencrypt/live/seat.example.com/privkey.pem;
  client_max_body_size 128m;

  location / {
    proxy_pass http://127.0.0.1:8790;
    proxy_http_version 1.1;
    proxy_set_header Host $host;
    proxy_set_header X-Forwarded-For $remote_addr;   # 只写一跳，门面只信任这一跳
    proxy_set_header X-Forwarded-Proto $scheme;
    proxy_set_header Upgrade $http_upgrade;          # /v1/responses 用 WebSocket
    proxy_set_header Connection $connection_upgrade;
    proxy_buffering off;
    proxy_request_buffering off;
    proxy_read_timeout 1800s;
    proxy_send_timeout 1800s;
  }
}
```

如果前面还有 Cloudflare，需要先用 `set_real_ip_from` 列出 Cloudflare 的 IP 段，再加上 `real_ip_header CF-Connecting-IP`。这样 `$remote_addr` 才是真实的用户 IP，限流和日志才准确。

## 3. 首次配置

1. 用管理员账号登录，进入「站点设置」：设置站点名、注册开关、基础并发、每个席位或账号附带的并发、宽限期，以及托管建槽模板（即 vm2api `POST /vms/create` 的请求体，例如 `{"runtime_type":"docker"}`）。
2. 「商品管理」：三类商品（并发、托管席位、租赁账号）各上架至少一个。
3. 「租赁池」：从候选 VM 里挑选平台账号加入租赁池，入池时会自动做一次槽位自检。
4. 支付：填写易支付的网关地址、商户号和密钥；暂时不开在线支付的话，保留「人工转账」即可。

## 4. 站内在线更新

在管理后台打开「系统更新」，可以看到当前版本、最新版本和更新说明，点一下就能更新或回滚。

**更新流程**

1. 从 GitHub Release 下载对应平台的程序 `seat-hub_linux_<arch>` 和校验文件 `checksums.txt`。
2. 核对 SHA-256，然后运行新程序的 `version` 子命令，确认它能在本机运行，并且自报的版本号与 Release 一致。
3. 把新程序放进 `/data/app/`，把当前版本记为回滚点。
4. 停止接收新请求，等进行中的请求结束（最多 `PORTAL_UPDATE_DRAIN_SECONDS` 秒，默认 20 秒），然后以退出码 75 退出。
5. Docker 按重启策略拉起容器。容器里的程序启动时发现 `/data/app/` 有更新的版本，就直接切换过去运行。
6. 新版本稳定运行 30 秒后才算更新成功。如果连续 3 次没能正常启动（包括 2 分钟内还没开始提供服务），会自动回滚到更新前的版本。

**需要注意**

- 只有正式发布的版本（带 `vX.Y.Z` 版本号）才支持在线更新；本地 `go run` 或手动运行（没有 Docker 或 systemd 托管）的进程会说明原因并拒绝更新。
- 数据库不会回滚。新版本的数据库迁移只增加表和字段，旧版本遇到不认识的表和字段会直接忽略，所以回滚是安全的。
- 用 `docker compose pull && docker compose up -d` 拉取更新的镜像也照常有效：镜像里的版本更新时，会优先于 `/data/app/` 里较旧的下载版本运行。
- 服务器访问 GitHub 需要代理的话，在 `.env` 里设置 `HTTPS_PROXY`。GitHub API 匿名访问的额度是每小时 60 次，状态页本身有 10 分钟的缓存，一般够用；不够的话设置 `PORTAL_UPDATE_TOKEN`（一个不需要任何权限的 token 即可）。
- 每次更新和回滚都会记入审计日志（`system.update` / `system.rollback`）。
- **从 v0.3.x 及更早版本升级到 v0.4.0**：首次启动会自动迁移账号体系（[KEY_SCOPE.md](KEY_SCOPE.md)）。vm2api 里会出现一个 `seathub_xxxxxxxx` 服务账号和每个用户一个 `sh-xxxxxxxx-u<用户ID>` 账号池；旧的 `portal_<用户ID>` 用户被停用但保留。用户的 `sk-pt-` 密钥不变，迁移时个别请求可能失败几秒。迁移失败会在后台「任务队列」里看到 `scope_migrate`，之后每 10 分钟的 `scope_reconcile` 会自动重试。不要在 vm2api 面板里手动改这些账号池和服务账号下的密钥，对账会把它们改回来。

## 5. 发版

打一个 `v` 开头的 tag 并推送，GitHub Actions（`.github/workflows/release.yml`）会自动完成发版：

1. 先在 `CHANGELOG.md` 顶部加上 `## vX.Y.Z` 小节，这一节的内容会作为 Release 的说明，也会显示在后台的「系统更新」页里。
2. `git tag v0.2.0 && git push origin v0.2.0`
3. Actions 会依次：
   - 跑测试；
   - 编译 `linux/amd64` 和 `linux/arm64` 两个程序，并生成 `checksums.txt`；
   - 创建 GitHub Release；
   - 构建多架构镜像，推送为 `ghcr.io/duegin/seat-hub:v0.2.0` 和 `:latest`。
4. 发布完成后，在线运行的实例会在「系统更新」页看到新版本。

Release 一旦发布，在线实例就会把它当作可更新的版本。所以先确认 CI 通过，再打 tag。

## 6. 用 systemd 部署（可选）

不用 Docker 时，可以从 Release 下载 `seat-hub_linux_amd64`：

```bash
sudo install -D -m 0755 seat-hub_linux_amd64 /opt/seat-hub/seat-hub
sudo cp deploy/seat-hub.service /etc/systemd/system/
sudo useradd -r -s /usr/sbin/nologin portal || true
sudo install -o portal -m 600 .env /opt/seat-hub/.env      # PORTAL_DB_PATH=/opt/seat-hub/data/portal.db
sudo chown -R portal:portal /opt/seat-hub
sudo systemctl daemon-reload && sudo systemctl enable --now seat-hub
```

单元文件里设置了 `SEAT_HUB_SUPERVISOR=systemd` 和 `Restart=always`，所以同样支持在线更新。更新下载的程序放在数据库所在目录下的 `app/` 子目录里。

## 7. 验证

```bash
curl -s https://seat.example.com/api/public/config | jq .data.version
KEY=sk-pt-...   # 在「API 密钥」页创建
curl -s https://seat.example.com/v1/models -H "Authorization: Bearer $KEY" | jq
```

## 8. 运维

- **备份**：数据库在 `seat-hub-data` 卷里。备份时要把 `portal.db`、`portal.db-wal`、`portal.db-shm` 三个文件一起拷走，最好先 `docker compose stop`。
- **任务**：后台「任务队列」页能看到积压和失败的任务，可以逐条重试。
- **与 vm2api 的关系**：门面只调用 vm2api 的 `/api/panel/*` 和 `/v1/*`。vm2api 重启或升级期间，用户「我的账号」里的用量会显示为空（不会显示成 0），下单和支付不受影响。
