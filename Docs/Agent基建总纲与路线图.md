> **副本说明**:本文为 DevPilot 仓库(`~/Desktop/DevPilot/Docs/`)的同步副本,源头与后续修订在 DevPilot 仓库。三项目关系见根 [README.md](../README.md)——本总纲覆盖 daemon+relay 线总排期与三仓库代码边界。

# Agent 基建总纲与路线图(daemon/relay 线)

> 状态:评审稿 v1(2026-10-18)|性质:决策总纲,**本期不含实现**
> 子文档:[DevPilot-Hook守护进程与推送中继设计.md](DevPilot-Hook守护进程与推送中继设计.md)
> 对标:moshi(竞品)——Go hook daemon + 云端推送中继
> 搁置记录(2026-10-22):自研 mosh 线暂缓,数月后再评估是否重启;设计底稿在 DevPilot 仓库 `Docs/Mosh客户端自研设计评审.md`,重启前本文只覆盖 daemon+relay 线。

---

## 1. 总纲定位

DevPilot 的产品主轴是「AI Agent 远程值守」:Agent 在用户的服务器上跑任务,人在 iPhone 上盯状态、等审批、给决策。本总纲覆盖的基建线补这条主轴的地基:

- **daemon + relay 线 = Agent 值班台神经系统**:App 被杀、手机在兜里,审批请求照样到、决策照样回。服务「审批闭环」的到达率。

竞品 Moshi 的护城河之一正是收件箱一键审批与推送到达率。对标不是抄交互,而是补齐同一层的基建能力;交互差异化(收件箱形态、审批面板、Live Activity 卡片)以 DevPilot 现有设计语言为准。

## 2. 既有决策账

### 2.1 L2 自建通道边界不动摇

L2(ntfy/Bark)「用户自备服务器」的定位不变;L3 云端中继是**并列新增**的反向链路(daemon→relay→APNs→设备),不合并、不替换。三链路分工表见 daemon 文档 §1.1。理由:L2 是隐私敏感用户的自持出口,L3 是开箱即用的默认体验,服务不同用户群。

### 2.2 已就位资产清单(地基,不重做)

| 资产 | 状态 | 用途 |
|---|---|---|
| Live Activity 锁屏+灵动岛(`Widgets/AgentLiveActivityWidget.swift` + `NSSupportsLiveActivities`)| 已上线 | relay(Widget 层零新做) |
| `AgentActivityAttributes` 五字段 ContentState(App/Widget 共享)| 已定稿 | relay content-state 一一对应 |
| 本地通知深链(`NotificationService.openSessionNotification`)| 已上线 | 收件箱跳转 |
| L2 设置页路由与表单形态(`SettingsView` .l2 先例、insetGrouped 自绘卡)| 已上线 | 云端通道页照抄形态 |
| 隐私政策站 + wrangler 先例 | 已上线 | 中继若走 CF 的先例(推荐仍为 VPS) |

## 3. 依赖与仓库落位

daemon+relay 线无外部阻塞,D0…D5 立即可开工。

- **仓库落位(2026-10-18 定为三仓库)**:**DevPilot** = iOS App(App 侧接入);**DevPilot-Hook** = hook daemon(Go,用户开发机);**DevPilot-APNS** = 云端中继 relay(Go,VPS,持 `pkg/wire` 协议类型库,daemon go.mod 引用);线上 WSS 协议是三端唯一契约。文档源头在 DevPilot/Docs,两 Go 仓库 Docs 持同步副本;三项目关系以 DevPilot-APNS 根 README 为准。

## 4. 里程碑总览(量级判断,非承诺)

```
本期        设计文档定稿(本文 + 子文档 + relay 实现级设计)
            │
实现期      daemon+relay 线(≈8-10 人周)
            D0 协议库+部署拍板            0.5
            D1 daemon 核心+hook 适配      2-2.5
            D2 配对+WSS+缓冲             1-1.5
            D3 中继+APNs+部署            2
            D4 App 云端通道/收件箱/LA     2-2.5
            D5 审批闭环+doctor+压测       1.5-2
```

仓库归属:`pkg/wire` 与 D3(中继实现)落 DevPilot-APNS(R0-R3 见其 `Docs/中继服务端与APNs推送设计.md`);D1/D2/D5 daemon 侧落 DevPilot-Hook。

## 5. 商业化门控建议

对照已拍板的海外美元三档(月 $2.99 / 年 $19.99 / 买断 $49.99,同一套功能,无试用,默认年订,Pro 卡只显档位名)与现有 Pro 边界(主机 3 台免费/悬浮钮/快捷指令限量/主题色/Tailscale):

| 能力 | 建议档位 | 论证 |
|---|---|---|
| **L3 云端中继推送 + 收件箱审批** | **Pro** | relay 是 DevPilot 侧唯一持续付费成本(VPS);「Agent 值班」正是付费用户的核心场景;免费档保留 L1(前台通知)保证基础体验 |
| 免费档不设配额 | — | DevPilot 卖点是能力而非用量;Moshi 的月配额+按次配额范式是其 Relay 托管成本结构所致,我方中继单租户自部署,无按次成本,不引入配额复杂度 |

注意:以上为**建议**,商业化切法最终按功能范围纪律由用户拍板后才立项实现。

## 6. 成本与预算

- **人周**:daemon+relay 线 ≈ 8-10 人周;均为量级判断非承诺。
- **中继 VPS**:单实例小规格(1C/1G 级)即可支撑单租户,月成本个位数美元;TLS 证书 Let's Encrypt 免费续期。
- **APNs**:零费用,但承担运维义务(token 剪枝/错误监控/`.p8` 保管),见 daemon 文档 §7.5/§13。
- **Cloudflare 备选**:Workers+DO 按连接数计费,单租户量级下成本可忽略;迁移成本主要是协议类型双份维护(§8 daemon 文档)。

## 7. 端到端验收标准(可测语句)

**daemon+relay 线**(D5 出口):
1. App 被杀 + 设备锁屏,Agent 触发审批 → 推送到达 ≤15s(P95,受 APNs 波动);
2. 锁屏点推送 → 审批面板 Allow → 终端侧 Agent 在超时窗口内继续执行;
3. daemon 断网 10 分钟期间的事件,重连后按幂等键补发、收件箱无重复无丢失;
4. `uninstall` 后 `~/.claude/settings.json` 与 `~/.codex/hooks.json` 中用户自装 hooks 原样保留;
5. Live Activity 在 App 被杀后仍能被远程更新(状态从「运行中」翻「等待审批」)。

## 8. 风险汇总与缓解索引

子文档风险表为主索引,此处只列**最重**项:

| 风险 | 线 | 等级 | 索引 |
|---|---|---|---|
| hook 超时被误配成「放行」 | relay | 安全高 | daemon 文档 H1(默认回落终端询问,daemon 拒绝该配置) |
| APNs 410 剪枝缺失 → 推送黑洞 | relay | 中 | daemon 文档 H2 |

## 9. 文档交付清单与后续动作

本期交付(纯文档,已落 `Docs/`):
1. [DevPilot-Hook守护进程与推送中继设计.md](DevPilot-Hook守护进程与推送中继设计.md) —— daemon/中继/APNs 原理/审批闭环
2. [Agent基建总纲与路线图.md](Agent基建总纲与路线图.md) —— 本文档
3. DevPilot-APNS 仓库 `Docs/中继服务端与APNs推送设计.md` —— 中继实现级设计(2026-10-18 拆仓新增;两 Go 仓库 Docs 各持本清单文档的同步副本)

用户评审通过后的动作序列(任务卡拆解):

| 卡 | 内容 | 时间盒 | 产出物 |
|---|---|---|---|
| D0 | relay 协议类型库草案 + 部署形态拍板 | 0.5 周 | Go module 骨架 + 部署决定 |

功能范围纪律声明:本文档全部能力(含商业化切法建议)在实现立项前均需用户逐项批准。

---

## 来源与依据

- 竞品:Moshi 官方文档(getmoshi.app/docs)、DeepSeek/豆包竞品拆解对话(2026-10-18,moshi-hook 架构/收件箱五事件/月配额商业化/RN+Rust 栈)
- 商业化既定口径:海外美元三档拍板(2026-10-15,见记忆与 Pro 功能清单)、Pro 能力边界清单
- 子文档的风险/里程碑/成本节为本总纲 §4/§6/§8 的唯一详源,互为索引
- 仓库内部事实索引:见子文档「来源与依据」节(本文不重复引用具体行号)
