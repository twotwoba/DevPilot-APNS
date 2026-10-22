# relay 部署手册（EC2 + Docker Compose）

目标拓扑：一台 Ubuntu 24.04 EC2 实例，Caddy 终结 TLS 并把 WSS 反代给 relay，
relay 与 bbolt 数据卷跑在 Docker 里。单实例是有意选择——WSS 是有状态连接，
扩容前需要引入共享路由层。

## 1. 准备

1. EC2 实例（t4g.small 起步），分配 Elastic IP，域名 A 记录指向该 IP。
2. 安全组只放行 `22/tcp`（限定你的 IP）、`80/tcp`、`443/tcp`、`443/udp`。
3. 安装 Docker 与 compose 插件，确认时钟同步：

   ```bash
   sudo apt-get update && sudo apt-get install -y docker.io docker-compose-v2 chrony
   sudo systemctl enable --now chrony
   timedatectl status   # System clock synchronized: yes
   ```

   JWT 的 `iat` 对时钟敏感，偏差过大会被 APNs 403。

## 2. 配置

```bash
git clone https://github.com/twotwoba/DevPilot-APNS.git
cd DevPilot-APNS/deploy
cp .env.example .env && chmod 600 .env
mkdir -p secrets && chmod 700 secrets
# 把下载到的 AuthKey_XXXX.p8 复制为 secrets/apns.p8
cp ~/Downloads/AuthKey_XXXX.p8 secrets/apns.p8 && chmod 600 secrets/apns.p8
$EDITOR .env   # 填 DOMAIN / APNS_KEY_ID / APNS_TEAM_ID
```

Linux 上 bind mount 会保留宿主文件属主，而镜像以 distroless 的 `nonroot`
（uid/gid 65532）运行。必须把 `.p8` 交给这个 uid，否则容器内读不到密钥、
`/readyz` 返回 503：

```bash
sudo chown 65532:65532 secrets/apns.p8
chmod 600 secrets/apns.p8
```

（macOS + OrbStack 的挂载层会重映射属主，通常无需这一步；EC2 上必须做。）

`.env` 与 `.p8` 都被 `.gitignore` 与 `.dockerignore` 排除，不会进镜像。

## 3. 启动与自检

```bash
docker compose up -d --build
docker compose ps
curl -fsS https://relay.example.com/healthz   # ok
curl -fsS https://relay.example.com/readyz    # ready（含 APNs 密钥可用性）
curl -fsS https://relay.example.com/metrics | head
```

`/readyz` 返回 503 时看 `docker compose logs relay`：常见原因是 `.p8` 路径、
Key ID / Team ID 不匹配或环境写错（`production` / `sandbox`）。

## 4. 配对与联调

在开发机：

```bash
devpilot-hook install
devpilot-hook pair --relay https://relay.example.com
```

把配对码输入 App 的「添加开发机」。之后 App 与 daemon 分别连接
`/ws/app`、`/ws/daemon`；可在 `/metrics` 看到
`devpilot_daemon_connections` 与 `devpilot_app_connections`。

## 5. 升级

```bash
cd DevPilot-APNS/deploy
docker compose pull && docker compose up -d
```

bbolt schema 通过 `meta.schema_version` 管理，目前没有破坏性迁移。
回滚时把 `image:` 指回上一个 tag 即可。

## 6. 备份

relay 自带一致性快照子命令（bbolt `View → tx.WriteTo`）：

```bash
docker compose exec relay /usr/local/bin/devpilot-relay snapshot --out /data/snapshots/relay-$(date +%F).db
```

宿主机 cron 每日执行，并把 `/data/snapshots` 同步到异地存储。快照可以在
relay 停止后直接替换 `/data/relay.db` 恢复。

## 7. 监控与告警

建议至少观察：

- `/readyz` 非 200；
- `devpilot_apns_pushes_total{status="410"}`（token 失效，客户端会重新注册）；
- `devpilot_apns_pushes_total{status="429"}`（限流）；
- `devpilot_apns_pushes_total{status="403"}`（密钥/时钟问题，必须告警）；
- `devpilot_pending_approvals` 长时间不下降（用户手机收不到通知）。

日志是结构化 JSON（docker json-file 驱动，已限制轮转）。

## 8. `.p8` 与 App 侧清单（凭据到位后执行）

1. Apple Developer → Certificates, Identifiers & Profiles → Keys → `+` →
   Apple Push Notifications service (APNs)，下载 `.p8`（**只能下载一次**）。
2. 记录 Key ID；Team ID 在 Membership 页面（本工程为 `D8N2Q2WZ86`）。
3. App ID `com.twotwoba.devpilot` 勾选 Push Notifications 与
   Time Sensitive Notifications，重新生成 provisioning profile。
4. iOS 工程增加 `aps-environment` 与
   `com.apple.developer.usernotifications.time-sensitive` entitlement。
5. 先用 `APNS_ENVIRONMENT=sandbox` + 开发版 device token 验证，
   再切 `production`。
6. Live Activity 的 topic 必须是
   `com.twotwoba.devpilot.push-type.liveactivity`。

## 9. 常见问题

| 现象 | 排查 |
|---|---|
| WebSocket 升级后立即断开 | Caddy 未配置 `flush_interval -1`；或安全组未放行 443 |
| `/readyz` 503 | `.p8` 不可读 / Key ID / Team ID 错误 |
| APNs 403 `ExpiredProviderToken` | 服务器时钟漂移；检查 chrony |
| APNs 403 `BadCertificateEnvironment` | sandbox token 打到了 production |
| 配对码无效 | 15 分钟过期或已使用；重新 `pair` |
| 417/429 | 同 topic 发送过快，relay 已内置退避，持续出现需扩容或降频 |
