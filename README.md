# DevPilot-APNS

DevPilot 的**云端推送中继(relay)**:部署在 VPS 上的 Go 单二进制,站在 hook daemon 与 Apple APNs 之间——daemon 连它、iPhone 连它、它替你打 APNs。没有它,「AI Agent 等审批」的事件就断在开发机上,App 被杀后什么都收不到。

## 三项目关系(两 Go 一 iOS,一条链路)

| | DevPilot | DevPilot-Hook | **DevPilot-APNS(本仓库)** |
|---|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| Go 常驻 daemon(用户开发机)| **Go relay(云 VPS)** |
| 职责 | 终端/SSH、收件箱 UI、审批面板、Live Activity、APNs token 注册 | hook 适配(Claude Code/Codex)、本地网关、事件聚合限流、离线缓冲 | **WSS 接入、配对路由表、APNs 推送、Live Activity 远程更新转发** |
| 链路角色 | WSS 客户端(App 端) | WSS 客户端(daemon 端) | **中枢** |
| 代码边界 | App 侧接入(RelayChannel 模型等)| daemon;`pkg/wire` 经 go.mod 引用本仓库 | relay 全部 + **`pkg/wire` 协议类型库(三端线上协议的单一事实源)** |

```
Claude Code/Codex hooks
  → DevPilot-Hook daemon(本地网关 127.0.0.1:24543)
  → DevPilot-APNS relay(本仓库:鉴权 → 路由表 → 选通道)
  → Apple APNs(HTTP/2)──或──App WSS 在线直推
  → iPhone(通知 / Live Activity / 收件箱)
审批回流:iPhone Allow/Deny → relay → daemon → hook 解除阻塞,Agent 继续
```

设计要点:审批链路任何环节超时 = 无决策回落终端原生询问(只会多问一次,不会放行);中继**只路由不读内容**——云通道只传「通知里你看得到的」摘要白名单;对标竞品 moshi 的云端中继。

## 快速开始（部署）

```bash
git clone https://github.com/twotwoba/DevPilot-APNS.git
cd DevPilot-APNS/deploy
cp .env.example .env && chmod 600 .env   # 填 DOMAIN / APNS_KEY_ID / APNS_TEAM_ID / METRICS_TOKEN
mkdir -p secrets && chmod 700 secrets
cp ~/Downloads/AuthKey_XXXX.p8 secrets/apns.p8 && chmod 600 secrets/apns.p8
docker compose up -d --build
curl -fsS https://relay.example.com/readyz
```

Caddy 负责 ACME TLS 与 WSS 反代（`flush_interval -1`），relay 以 distroless +
nonroot、只读根文件系统运行。**反代下务必 `TRUSTED_PROXY_COUNT=1`**（.env.example
已带），relay 才能从 XFF 还原真实客户端 IP 做限流与封禁。完整 EC2 运行手册、
备份、监控与 `.p8` 创建清单见 [deploy/README.md](deploy/README.md)。

Mac 端配对后可用 `test-push` 做真机验证（默认 relay 地址取 `PUBLIC_BASE_URL`）：

```bash
devpilot-relay test-push --relay https://relay.example.com \
  --credential dpd_... --kind approval   # iPhone 收到 time-sensitive 通知
```

## 配额与防滥用（多租户托管）

面向「一个 relay 服务所有用户」的默认设计；自部署单人场景可全部关闭（阈值为 0）。

| 维度 | 默认值 | 环境变量 | 语义 |
|---|---|---|---|
| 事件速率 | free 10/60s、pro 60/60s | `RATE_EVENT_FREE/PRO`、`RATE_EVENT_WINDOW_SECONDS` | per-daemon 令牌桶,超限回 `AckRejected`(daemon 终态丢弃,无重试风暴) |
| 日推送配额 | free 500/天、pro 5000/天 | `QUOTA_DAILY_FREE/PRO` | per-daemon UTC 日历日,只拦 APNs 推送;WSS 实时/收件箱/resync 不受限 |
| 审批豁免 | 开 | `QUOTA_EXEMPT_APPROVALS` | `approval_required/task_complete` 豁免速率闸,**不**豁免日配额 |
| 配对限流 | 15/10 每分钟每 IP | `PAIR_SESSION_LIMIT/PAIR_ATTEMPT_LIMIT` | 固定窗口 |
| IP 封禁 | 100 次 4xx·5xx/5min → 封 24h | `BAN_THRESHOLD/WINDOW_SECONDS/TTL_SECONDS`、`BAN_ENABLED` | fail2ban 式;IPv4 整段、IPv6 /64 聚合;环回豁免 |
| 身份 TTL | 0(禁用) | `DAEMON_TTL_DAYS/DEVICE_TTL_DAYS` | 超期未连接的 daemon 级联注销;首发保持 0,稳定后再开 |
| /metrics | 公开 | `METRICS_TOKEN` | 非空时需 Bearer;Caddy 层另有 404 兜底 |

档位(`free|pro`)在 App 配对时提交,v0.2 为客户端自声明;收据级验证列入下一轮。

## 开发与测试

```bash
go test -race ./...            # 全部单测与集成测试
go build ./cmd/relay           # 本地编译
make docker                    # 构建镜像（需要 Docker 守护进程）
```

APNs 客户端可用 `APNS_MOCK=1` 在没有 `.p8` 的情况下跑通投递链路；单元测试用
假 HTTP/2 服务器覆盖 200/410/429/403/400/5xx、JWT 缓存与 topic 退避。

## 发布

`git tag v0.1.0 && git push origin v0.1.0` 会触发：

- `.github/workflows/docker.yml`：构建并推送 `ghcr.io/twotwoba/devpilot-relay`；
- DevPilot-Hook 的发布依赖本仓库的 tag（`pkg/wire` 是它的模块依赖），
  因此**必须先发布本仓库，再发布 Hook**。

服务器升级用 `docker compose pull && docker compose up -d`。

## 当前状态

- ✅ `pkg/wire` v1：Envelope / Decision / Ack / Hello / Resync / LA token、
  确定性 `approval_id`、摘要白名单、contract vectors
- ✅ relay：配对（15 分钟一次性码、只存 `sha256(secret)`、轮换需旧凭据）、
  bbolt（daemons / devices / pairing_codes / pending / inbox / la_tokens）、
  WSS hub（心跳、鉴权、路由、决策 first-wins、断线补发、resync）、
  `/healthz` `/readyz` `/metrics`
- ✅ APNs：ES256 JWT（50 分钟缓存）、HTTP/2、64 并发闸、4KB 上限、
  错误分类与退避、410 剪枝、审批/任务完成通知与 Live Activity 节流
- ✅ 托管多租户（v0.2.0）：per-daemon 配额（令牌桶 + UTC 日配额，审批豁免速率闸）、
  fail2ban 封禁（IPv6 /64 聚合）、XFF 可信代理链（`TRUSTED_PROXY_COUNT`）、
  生命周期端点（token 重报 / 设备解绑 / daemon 吊销）、schema v1→v2 迁移
  （`Tier`/`LastSeenAt` 回填）、身份 TTL 清理、`test-push` 真机验证工具
- ✅ 部署：Dockerfile（distroless + healthcheck 子命令）、compose + Caddy、
  EC2 runbook、GHCR 发布工作流
- ⬜ 真机 APNs sandbox 验证（EC2 部署 + DevPilot App 配对后执行）
- ⬜ iOS App 的 relay 客户端与审批 UI（进行中,DevPilot 仓库）
- ⬜ tier 收据级验证（当前为客户端自声明）

## 文档

| 文档 | 说明 |
|---|---|
| [中继服务端与APNs推送设计.md](Docs/中继服务端与APNs推送设计.md) | **核心设计**:组件骨架/WSS 协议/APNs 客户端/投递语义/里程碑,实现落本仓库 |
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | 上游总设计(daemon+relay 通盘;APNs 原理详解在其 §7),同步自 DevPilot 仓库 `Docs/` |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | daemon+relay 线总排期与三仓库代码边界,同步自 DevPilot 仓库 |

文档源头在 DevPilot 仓库 `Docs/`,本仓库 Docs 持同步副本。
