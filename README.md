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

## 当前状态

- ✅ 设计文档(2026-10-18,评审稿 v1)
- ⬜ R0–R3 实现未开工;与 DevPilot-Hook 均无外部阻塞,可并行开工(`pkg/wire` R0 先行)

## 文档

| 文档 | 说明 |
|---|---|
| [中继服务端与APNs推送设计.md](Docs/中继服务端与APNs推送设计.md) | **核心设计**:组件骨架/WSS 协议/APNs 客户端/投递语义/里程碑,实现落本仓库 |
| [DevPilot-Hook守护进程与推送中继设计.md](Docs/DevPilot-Hook守护进程与推送中继设计.md) | 上游总设计(daemon+relay 通盘;APNs 原理详解在其 §7),同步自 DevPilot 仓库 `Docs/` |
| [Agent基建总纲与路线图.md](Docs/Agent基建总纲与路线图.md) | mosh × daemon+relay 两线总排期与三仓库代码边界,同步自 DevPilot 仓库 |

mosh 客户端设计(iOS 侧,与本仓库无实现交集)见 DevPilot 仓库 `Docs/Mosh客户端自研设计评审.md`。文档源头在 DevPilot 仓库 `Docs/`,本仓库 Docs 持同步副本。
