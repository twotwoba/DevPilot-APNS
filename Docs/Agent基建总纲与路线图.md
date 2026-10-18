> **副本说明**:本文为 DevPilot 仓库(`~/Desktop/DevPilot/Docs/`)的同步副本,源头与后续修订在 DevPilot 仓库。三项目关系见根 [README.md](../README.md)——本总纲覆盖两线总排期与三仓库代码边界。

# Agent 基建总纲与路线图(mosh 线 × daemon/relay 线)

> 状态:评审稿 v1(2026-10-18)|性质:决策总纲,**本期不含实现**
> 子文档:[Mosh客户端自研设计评审.md](Mosh客户端自研设计评审.md)|[DevPilot-Hook守护进程与推送中继设计.md](DevPilot-Hook守护进程与推送中继设计.md)|前置:[Mosh决策门评审.md](Mosh决策门评审.md)
> 对标:moshi(竞品)三件套——自研 mosh 客户端 + Go hook daemon + 云端推送中继

---

## 1. 总纲定位

DevPilot 的产品主轴是「AI Agent 远程值守」:Agent 在用户的服务器上跑任务,人在 iPhone 上盯状态、等审批、给决策。本总纲覆盖两条基建线,分别补这条主轴的两块地基:

- **mosh 线 = 连接体验地基**:弱网/切网/锁屏回来看,连接不断、屏幕即恢复。服务「盯」的体验下限。
- **daemon + relay 线 = Agent 值班台神经系统**:App 被杀、手机在兜里,审批请求照样到、决策照样回。服务「审批闭环」的到达率。

竞品 Moshi 的护城河正是这两件事的组合(demo 主打 Wake Probe 永不断线 + 收件箱一键审批)。对标不是抄交互,而是补齐同一层的基建能力;交互差异化(收件箱形态、审批面板、Live Activity 卡片)以 DevPilot 现有设计语言为准。

## 2. 既有决策账

### 2.1 mosh 决策门重开(2026-10-18)

[Mosh决策门评审.md](Mosh决策门评审.md)(2026-09-19)以「tmux + 自动重连实测达标」为由无限期推迟 mosh,并写死三条重开条件。本轮竞品拆解后用户拍板重开;**逐条对照结论:三条均未完整触发(条件 3 仅前半「体验落差已实证」,「流失归因」无数据),本次重开是产品负责人的主动战略决策,以 M0 spike 作成本闸门替代观望**——spike 不过,决策门再次关闭,沉没成本以人天计。详见 mosh 文档 §1.2。

### 2.2 L2 自建通道边界不动摇

L2(ntfy/Bark)「用户自备服务器」的定位不变;L3 云端中继是**并列新增**的反向链路(daemon→relay→APNs→设备),不合并、不替换。三链路分工表见 daemon 文档 §1.1。理由:L2 是隐私敏感用户的自持出口,L3 是开箱即用的默认体验,服务不同用户群。

### 2.3 已就位资产清单(两线的地基,不重做)

| 资产 | 状态 | 服务哪条线 |
|---|---|---|
| Live Activity 锁屏+灵动岛(`Widgets/AgentLiveActivityWidget.swift` + `NSSupportsLiveActivities`)| 已上线 | relay(Widget 层零新做) |
| `AgentActivityAttributes` 五字段 ContentState(App/Widget 共享)| 已定稿 | relay content-state 一一对应 |
| 本地通知深链(`NotificationService.openSessionNotification`)| 已上线 | 收件箱跳转 |
| tmux 探测/挂载、断线退避状态机、`ConnectionTypes` 状态机、SwiftTerm driver.feed 泵线 | 已上线 | mosh 线复用全部架构位 |
| `KeychainStore` host 级命名与级联清理、SessionRecordStore 宽容 decoder、HostRecord Optional 先例 | 已上线 | mosh 持久化零迁移 |
| L2 设置页路由与表单形态(`SettingsView` .l2 先例、insetGrouped 自绘卡)| 已上线 | 云端通道页照抄形态 |
| 隐私政策站 + wrangler 先例 | 已上线 | 中继若走 CF 的先例(推荐仍为 VPS) |

## 3. 两线依赖关系

```
daemon+relay 线(无外部阻塞)────────────────────────────▶ D0…D5 立即可开工
mosh 线 ──S1(OCB 裁决,阻塞)+S3(Spectty 尽调,阻塞)──▶ M0 闸门 ─▶ M1…M4
              S2(Tailscale UDP)/S4(后台恢复)非阻塞,随行
```

- **依赖方向**:mosh 线被 S1/S3 两个阻塞 spike 卡头,闸门通过才进实现;daemon+relay 线零外部依赖。
- **排期结论**:daemon+relay 先行全速开工;mosh spike 并行跑(1-1.5 人周即见分晓),通过则无缝接 M1。
- **仓库落位(2026-10-18 定为三仓库)**:**DevPilot**(本仓库)= iOS App(App 侧接入 + 全部 mosh 线);**DevPilot-Hook** = hook daemon(Go,用户开发机);**DevPilot-APNS** = 云端中继 relay(Go,VPS,持 `pkg/wire` 协议类型库,daemon go.mod 引用);线上 WSS 协议是三端唯一契约。文档源头在 DevPilot/Docs,两 Go 仓库 Docs 持同步副本;三项目关系以 DevPilot-APNS 根 README 为准。
- **交叉点(共享资源,接口即契约)**:
  1. **影子终端快照**:mosh 线的自研 SSP 状态模型副产品,供收件箱预览与会话缩略图——relay 线的收件箱 UI 预留快照位,mosh 未上线时留空;
  2. **收件箱**:relay 线建(五事件模型),mosh 线不另起炉灶;
  3. **Live Activity**:relay 远程更新喂现有 `AgentActivityAttributes`,mosh 线不触碰通知层;
  4. **商业化**:relay 有持续 VPS 成本,由 Pro 档承担(§6)。

## 4. 决策门重开判定与退路

| 情形 | 判定 | 动作 |
|---|---|---|
| S1+S3 通过(S2 任一出路可行更佳)| 决策门正式放行 | 进 mosh M1 |
| S1 失败(OCB 变体无法黑盒裁决)| 阻塞 | 决策门再次关闭,记录归档;转 daemon+relay 单线 |
| S3 失败(Spectty 审计可疑)| 法律阻塞 | 维持「仅交叉参照」升级为「零接触」;mosh 线成本重估后再议 |
| S2 全挂(内嵌 Tailscale 无 UDP 路径)| 高风险 | 不阻塞——mosh 先支持系统级 VPN 主机 + 直连主机,tailnet 写明限制 |
| **全挂退路** | — | **维持 tmux + SSH + 自动重连主线不变**(决策门原文仍是有效基线,实测达标从未被推翻) |

## 5. 里程碑总览(量级判断,非承诺)

```
本期        三份设计文档(本文 + 两子文档)
            │
spike 期    mosh:S1/S2/S3/S4(≈1-1.5 人周)          daemon:无 spike,直接 D0
            │
实现期      daemon+relay 线(≈8-10 人周,先行)        mosh 线(≈8-12 人周,过闸门后)
            D0 协议库+部署拍板            0.5        M0 spike 已在上行计入
            D1 daemon 核心+hook 适配      2-2.5      M1 加密+SSP        3-4
            D2 配对+WSS+缓冲             1-1.5      M2 UDP/漫游/execBootstrap 2-2.5
            D3 中继+APNs+部署            2          M3 架构集成+影子终端 1.5-2
            D4 App 云端通道/收件箱/LA     2-2.5      M4 互通矩阵        1.5-2
            D5 审批闭环+doctor+压测       1.5-2      P2 预测回显(独立决策门)+1-2
```

两线并行时的唯一人员约束:通知/Live Activity/会话模型三处归 relay 线所有,mosh 线不进这三处(§3 交叉点契约)。

仓库归属:`pkg/wire` 与 D3(中继实现)落 DevPilot-APNS(R0-R3 见其 `Docs/中继服务端与APNs推送设计.md`);D1/D2/D5 daemon 侧落 DevPilot-Hook。

## 6. 商业化门控建议

对照已拍板的海外美元三档(月 $2.99 / 年 $19.99 / 买断 $49.99,同一套功能,无试用,默认年订,Pro 卡只显档位名)与现有 Pro 边界(主机 3 台免费/悬浮钮/快捷指令限量/主题色/Tailscale):

| 能力 | 建议档位 | 论证 |
|---|---|---|
| **L3 云端中继推送 + 收件箱审批** | **Pro** | relay 是 DevPilot 侧唯一持续付费成本(VPS);「Agent 值班」正是付费用户的核心场景;免费档保留 L1(前台通知)保证基础体验 |
| **Mosh 传输** | **Pro** | 连接体验增量卖点;对标 Moshi 把 mosh 放付费墙后的做法;SSH 基线免费档完整可用,不存在「免费用户被砍命脉」问题 |
| 影子终端快照/会话缩略图 | 跟随宿主能力(收件箱是 Pro 则 Pro) | 不单独设墙,避免碎切 |
| 免费档不设配额 | — | DevPilot 卖点是能力而非用量;Moshi 的月配额+按次配额范式是其 Relay 托管成本结构所致,我方中继单租户自部署,无按次成本,不引入配额复杂度 |

注意:以上为**建议**,商业化切法最终按功能范围纪律由用户拍板后才立项实现。

## 7. 成本与预算

- **人周**:两线合计 ≈ 16-22 人周(8-12 + 8-10),不含 P2 预测回显;均为量级判断非承诺。
- **中继 VPS**:单实例小规格(1C/1G 级)即可支撑单租户,月成本个位数美元;TLS 证书 Let's Encrypt 免费续期。
- **APNs**:零费用,但承担运维义务(token 剪枝/错误监控/`.p8` 保管),见 daemon 文档 §7.5/§13。
- **Cloudflare 备选**:Workers+DO 按连接数计费,单租户量级下成本可忽略;迁移成本主要是协议类型双份维护(§8 daemon 文档)。

## 8. 端到端验收标准(可测语句)

**mosh 线**(M4 出口):
1. 对 Homebrew mosh 1.4 与 Ubuntu apt mosh 1.3:bootstrap → 屏幕同步 → 击键 → 杀 App 重进恢复,全流程绿;
2. iPhone 锁屏 10 分钟回前台,≤3s 恢复可交互(目标值,M0 后校准);
3. Wi-Fi↔蜂窝切换,会话不掉、屏幕续上;
4. 与官方 mosh-client 同服务器互操作双向可用;
5. 审核:飞行 UDP 的环境里 Auto 传输静默回落 SSH,功能完整。

**daemon+relay 线**(D5 出口):
1. App 被杀 + 设备锁屏,Agent 触发审批 → 推送到达 ≤15s(P95,受 APNs 波动);
2. 锁屏点推送 → 审批面板 Allow → 终端侧 Agent 在超时窗口内继续执行;
3. daemon 断网 10 分钟期间的事件,重连后按幂等键补发、收件箱无重复无丢失;
4. `uninstall` 后 `~/.claude/settings.json` 与 `~/.codex/hooks.json` 中用户自装 hooks 原样保留;
5. Live Activity 在 App 被杀后仍能被远程更新(状态从「运行中」翻「等待审批」)。

## 9. 风险汇总与缓解索引

两子文档风险表为主索引,此处只列**跨线**与**最重**项:

| 风险 | 线 | 等级 | 索引 |
|---|---|---|---|
| OCB 变体不匹配 → 加密层返工 | mosh | 阻塞 | mosh 文档 R1/S1 |
| Spectty 净室声明不实 → 污染传导 | mosh | 法律阻塞 | mosh 文档 R4/S3 |
| 内嵌 Tailscale 桥无 UDP → tailnet 主机不可用 | mosh | 高 | mosh 文档 R2/S2 |
| hook 超时被误配成「放行」 | relay | 安全高 | daemon 文档 H1(默认回落终端询问,daemon 拒绝该配置) |
| APNs 410 剪枝缺失 → 推送黑洞 | relay | 中 | daemon 文档 H2 |
| 两线并行挤占同一批通知/会话模型代码 | 跨线 | 中 | §3 交叉点契约:三处归 relay 线所有 |

## 10. 文档交付清单与后续动作

本期交付(纯文档,已落 `Docs/`):
1. [Mosh客户端自研设计评审.md](Mosh客户端自研设计评审.md) —— 协议拆解/净室策略/架构集成/spike 判据
2. [DevPilot-Hook守护进程与推送中继设计.md](DevPilot-Hook守护进程与推送中继设计.md) —— daemon/中继/APNs 原理/审批闭环
3. [Agent基建总纲与路线图.md](Agent基建总纲与路线图.md) —— 本文档
4. DevPilot-APNS 仓库 `Docs/中继服务端与APNs推送设计.md` —— 中继实现级设计(2026-10-18 拆仓新增;两 Go 仓库 Docs 各持本清单文档的同步副本)

用户评审通过后的动作序列(spike 任务卡拆解):

| 卡 | 内容 | 时间盒 | 产出物 |
|---|---|---|---|
| S1 | OCB 变体黑盒裁决 | 3-5 天 | 裁决报告:OCB1 还是 RFC 7253 + 我方可解密真实握手包的验证代码(不入库亦可) |
| S2 | 内嵌 Tailscale UDP 三出路 | 2-3 天 | 选型结论 + 漫游退化文案口径 |
| S3 | Spectty 尽调审计 + 接触登记表建账 | 2-3 天 | 风险分级报告 + 依赖边界决定 |
| S4 | 真机后台三档恢复实测 | 3 天 | 时延数据 → 校准验收目标值 |
| D0 | relay 协议类型库草案 + 部署形态拍板 | 0.5 周 | Go module 骨架 + 部署决定 |

每个 spike 卡独立可开工;S1/S3 任一失败即触发 §4 的对应退路。功能范围纪律声明:本文档全部能力(含商业化切法建议)在实现立项前均需用户逐项批准。

---

## 来源与依据

- 竞品:Moshi 官方文档(getmoshi.app/docs)、DeepSeek/豆包竞品拆解对话(2026-10-18,moshi-hook 架构/收件箱五事件/月配额商业化/RN+Rust 栈)
- 前置决策:[Mosh决策门评审.md](Mosh决策门评审.md)(2026-09-19,三条重开条件原文)
- 商业化既定口径:海外美元三档拍板(2026-10-15,见记忆与 Pro 功能清单)、Pro 能力边界清单
- 两子文档的风险/里程碑/成本节为本总纲 §5/§7/§9 的唯一详源,互为索引
- 仓库内部事实索引:见两子文档「来源与依据」节(本文不重复引用具体行号)
