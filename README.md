# DevPilot-APNS

DevPilot 的**云端推送中继(relay)**:部署在 VPS 上的 Go 单二进制,站在 hook daemon 与 Apple APNs 之间——daemon 连它、iPhone 连它、它替你打 APNs。没有它,「AI Agent 等审批」的事件就断在开发机上,App 被杀后什么都收不到。

## 三项目关系(两 Go 一 iOS,一条链路)

| | DevPilot | DevPilot-Hook | **DevPilot-APNS(本仓库)** |
|---|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| Go 常驻 daemon(用户开发机)| **Go relay(云 VPS)** |
| 职责 | 终端/SSH/mosh、收件箱 UI、审批面板、Live Activity、APNs token 注册 | hook 适配(Claude Code/Codex)、本地网关、事件聚合限流、离线缓冲 | **WSS 接入、配对路由表、APNs 推送、Live Activity 远程更新转发** |
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
cp .env.example .env && chmod 600 .env   # 填 DOMAIN / APNS_KEY_ID / APNS_TEAM_ID
mkdir -p secrets && chmod 700 secrets
cp ~/Downloads/AuthKey_XXXX.p8 secrets/apns.p8 && chmod 600 secrets/apns.p8
docker compose up -d --build
curl -fsS https://relay.example.com/readyz
```

Caddy 负责 ACME TLS 与 WSS 反代（`flush_interval -1`），relay 以 distroless +
nonroot、只读根文件系统运行。完整 EC2 运行手册、备份、监控与 `.p8` 创建清单见
[deploy/README.md](deploy/README.md)。

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
- ✅ 部署：Dockerfile（distroless + healthcheck 子命令）、compose + Caddy、
  EC2 runbook、GHCR 发布工作流
- ⬜ 真机 APNs sandbox 验证（需在 Apple Developer 创建 `.p8` 后执行）
- ⬜ iOS App 的 relay 客户端与审批 UI（下一轮，DevPilot 仓库）

## 文档

| 文档 | 说明 |
|---|---|
| [中继服务端与APNs推送设计.md](Docs/中继服务端与APNs推送设计.md) | **核心设计**:组件骨架/WSS 协议/APNs 客户端/投递语义/里程碑,实现落本仓库 |
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | 上游总设计(daemon+relay 通盘;APNs 原理详解在其 §7),同步自 DevPilot 仓库 `Docs/` |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | mosh × daemon+relay 两线总排期与三仓库代码边界,同步自 DevPilot 仓库 |

mosh 客户端设计(iOS 侧,与本仓库无实现交集)见 DevPilot 仓库 `Docs/Mosh客户端自研设计评审.md`。文档源头在 DevPilot 仓库 `Docs/`,本仓库 Docs 持同步副本。
