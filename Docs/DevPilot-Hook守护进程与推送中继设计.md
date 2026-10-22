> **副本说明**:本文为 DevPilot 仓库(`~/Desktop/DevPilot/Docs/`)的同步副本(daemon+relay 总设计;APNs 原理详解在其 §7,本文 §8 为 relay 概要)。**relay 的实现级设计见本仓库 [中继服务端与APNs推送设计.md](中继服务端与APNs推送设计.md)**;三项目关系见根 [README.md](../README.md)。

# DevPilot Hook 守护进程与推送中继设计(daemon + relay)

> 状态:评审稿 v1(2026-10-18)|性质:决策 + 设计文档,**本期不含实现**
> 关联:[Agent基建总纲与路线图.md](Agent基建总纲与路线图.md)
> 对标:moshi-hook(Go 单二进制、win/mac/linux、WebSocket 连云端中继)
> 仓库落位(2026-10-18 更新为三仓库):daemon 的 Go 实现落 **DevPilot-Hook**;relay 中继的 Go 实现落 **DevPilot-APNS**(实现级设计见该仓库 `Docs/中继服务端与APNs推送设计.md`,本文 §8 为概要);本仓库(DevPilot)承担 App 侧接入(RelayChannel 模型/云端通道页/收件箱/Live Activity push token 上报,§11)。文档源头在 DevPilot/Docs,两 Go 仓库 Docs 持同步副本。

---

## 1. 目标与范围

### 1.1 要解决的问题

DevPilot 现有通知链路(§11 现状清单)全部依赖 **App 在前台或刚退后台**:`LiveActivityService` 注释自证「退后台被挂起后停止更新」。用户把手机揣兜里半小时,Agent 等审批时什么都收不到——「AI Agent 远程值守」在 iOS 后台机制下缺一条 **App 被杀也能到达**的反向链路。

三条链路分工(本设计是第三条):

| 链路 | 到达条件 | 状态 |
|---|---|---|
| L1 本地通知 + Live Activity | App 进程活着/刚挂起 | 已上线 |
| L2 用户自建通道(ntfy/Bark) | 用户自备服务器,边缘覆盖 | 已上线,语义不动 |
| **L3 云端中继(daemon→relay→APNs)** | **App 被杀、设备锁屏,均可达** | 本设计 |

### 1.2 范围

- **做**:用户开发机上常驻 daemon(macOS/Linux/Windows),注册进 Claude Code/Codex hooks;自建中继服务端;APNs 推送;App 侧收件箱与审批闭环;Live Activity 远程更新。
- **不做**(边界):不动 L2;不做 tmux send-keys 注入方案(审批走 hook 协议,见 §4);不做多用户 SaaS 化(中继单租户自部署)。

---

## 2. 总体架构与数据流

```
┌── 用户开发机 ────────────────────────────────┐
│  Claude Code / Codex                         │
│    │ ①hook 触发(事件/审批请求)              │
│    ▼                                         │
│  devpilot-hook daemon(常驻,Go 单二进制)    │
│    │ ②本地网关 127.0.0.1:24543(loopback HTTP)│
│    │ ③摘要最小化(§4.3 白名单+截断)          │
└────┼─────────────────────────────────────────┘
     │ ④WSS(双向:上行事件/下行审批决策)
     ▼
┌── 中继服务端(单 VPS Go 二进制,自建)────────┐
│  配对 token→device 路由表 · WSS 接入         │
│  APNs 客户端(HTTP/2 + .p8)                  │
└────┬─────────────────────────────────────────┘
     │ ⑤APNs 推送
     ▼
┌── iPhone ────────────────────────────────────┐
│  APNs→系统通知 / Live Activity 远程更新       │
│  收件箱:审批 Allow/Deny                      │
│    │ ⑥决策经 WSS 回流(App→中继→daemon)     │
│    ▼                                         │
│  PreToolUse hook 解除阻塞,Agent 继续        │
└──────────────────────────────────────────────┘
```

五段:①hook 触发 ②本地 IPC ③云通道最小化 ④WSS 双向 ⑤⑥APNs 下行 + 决策回流。**核心闭环 = approval_required 事件下行 + 审批决策上行**,其余事件是单向遥测。

---

## 3. daemon 进程模型与子命令

### 3.1 形态:Go 单二进制常驻 daemon(已拍板)

选型论证:

| 候选 | 判定 | 理由 |
|---|---|---|
| **Go 单二进制** | ✅ | **决定性理由 = win/mac/linux 一套代码交叉编译**(GOOS/GOARCH 矩阵零成本);单静态二进制用户拷贝即用;WSS/HTTP 服务端生态成熟(gorilla/websocket、net/http) |
| Swift | ❌(本期) | macOS 一线质量高,但 Windows 支持二线(用户拍板 Go 的原因);SwiftNIO 跨平台包袱重 |
| 无守护进程薄方案 | ❌ | 用户拍板一步到位常驻:审批闭环要求「会话在跑时 hook 必须有人接」,短命 hook 进程自己等决策会随 Claude Code 进程树被搅;常驻进程统一管理 WSS 长连接、限流、离线缓冲 |

### 3.2 子命令(对标 moshi-hook)

```
devpilot-hook install     # 幂等写入 ~/.claude/settings.json 与 ~/.codex/hooks.json 的 hooks 段
devpilot-hook pair        # 生成/轮换配对 token,打印配对码供 App 扫码或粘贴
devpilot-hook serve       # 前台运行(调试);常驻经 service 子命令
devpilot-hook doctor      # 自检:配置文件解析/hook 注册在位/网关端口/WSS 连通/配对态/APNs 侧(经中继查询)
devpilot-hook service     # 注册/管理常驻:launchd(macOS LaunchAgent)/ systemd --user(Linux)/ Windows 服务
devpilot-hook uninstall   # 只删自家条目(§12 卫生),可选 --purge 清全部本地状态
```

### 3.3 进程模型

- 常驻单进程:WSS 客户端(带指数退避重连)+ 本地网关(loopback HTTP)+ 事件聚合器(§5 限流/合并);
- 崩溃恢复:事件先落本地队列(磁盘 WAL,单文件),WSS 断连期间缓冲,重连后按幂等键补发;
- 资源占用目标:常驻内存 <30MB,空转 CPU ≈0(事件驱动)。

---

## 4. 事件模型与 hook 适配

### 4.1 五事件类型(对标 moshi 收件箱)

| 事件 | 来源 hook | App 处置 | 限流 |
|---|---|---|---|
| `session_started` | SessionStart | 收件箱新行 | 无 |
| `tool_running` | PostToolUse | 更新该会话行副标题 | **每会话 N 分钟/条**(建议 5min) |
| `tool_finished` | PostToolUse(结果) | 更新行状态 | 同上 |
| `approval_required` | PreToolUse | **推送 + 卡片强调 + 审批按钮** | 无(人等它) |
| `task_complete` | Stop | 推送 + 行置完成 | 无 |

收件箱 UI 纪律:**每个 agent 会话永远单行**(合并键 = 会话 id,新事件改写旧行),不堆叠刷屏。

### 4.2 审批闭环:PreToolUse 同步决策(核心)

Claude Code hook 协议:PreToolUse hook 可同步返回 JSON `{"permissionDecision": "allow"|"deny"|"ask"}`,进程退出前 Claude Code 阻塞等待。这是**协议级审批通道,不需要 tmux send-keys 注入**:

```
PreToolUse 触发 → hook(薄壳)POST 本地网关 → daemon 生成 approval_id
  → WSS 上行 approval_required → 中继 → APNs(Time Sensitive)
  → iPhone 推送/Live Activity → 用户点 Allow/Deny
  → App 经 WSS 发 {approval_id, decision} → 中继路由回该 daemon
  → daemon 应答等待中的 hook → hook 输出决策 JSON 退出 → Agent 继续
```

**超时回退(安全默认)**:daemon/hook 任意环节超时(建议 5min,可配),hook 返回「无决策」exit 0 → **落回终端内原生权限询问**。即:远程审批链路任何故障,Agent 的安全性不劣于不用 DevPilot 的基线——审批失败只会「多问你一次」,不会「放行」。

### 4.3 云通道摘要最小化(字段级白名单)

原则(对齐 moshi):**服务器只收到通知里你看得到的内容**。上行字段白名单 + 截断:

| 字段 | 上限 | 说明 |
|---|---|---|
| 会话标题/主机名 | 120 字符 | 用户可见 |
| 事件类型/状态 | 枚举 | — |
| 审批请求:工具名 + 命令摘要 | **256 字符** | 与锁屏通知可见内容一致 |
| Agent 回复摘要 | 80 字符 | 通知预览同源 |
| Prompt 摘要 | 200 字符 | 通知预览同源 |

不出机器的:完整输出、文件内容、环境变量、cwd 之外的路径。**Live Activity content-state 同白名单**(§9.3)——这条同时是隐私策略和端到端加密不可行的替代防线(§6.4)。

### 4.4 Codex 适配

`~/.codex/hooks.json` 事件映射同一五类型;两端 hook 配置写同一 daemon 网关,事件体带 `agent_kind: claude|codex` 区分。

---

## 5. 本地网关与 IPC

### 5.1 选型:loopback HTTP(已拍板)

`127.0.0.1:24543`,hook 以 HTTP POST 调用 daemon。**选 loopback HTTP 而非 Unix domain socket 的理由:Windows 对齐**——命名管道/UDS 在 Windows 上是另一套 API,loopback HTTP 三平台同一实现,且 curl 一条命令可手测、可观测、易超时控制。端口固定 24543 并在 doctor 里检测占用冲突;占用时 fallback 动态端口 + 配置文件记录。

### 5.2 端点(草案)

```
POST /v1/events          # 事件上报(五类型),带幂等键 event_id(uuid)
POST /v1/approvals/:id   # hook 查询/长轮询审批结果(hook 薄壳用)
GET  /healthz            # doctor 用
```

### 5.3 聚合纪律

- 幂等:event_id 去重(本地环形 LRU 足够);
- 合并:同会话行内覆盖;`tool_running/finished` 按会话令牌桶限流(§4.1);
- `approval_required` 永不限流、永不合并不投递。

---

## 6. 配对与安全

### 6.1 配对流程

1. 用户在开发机 `devpilot-hook pair` → 生成高熵配对码(带有效期,一次性);
2. App 内「云端通道」页输入配对码(或扫码)→ App 直连中继 `/pair` 完成设备注册;
3. 中继建立 `pairing_token → device(APNs token + WSS 会话)` 路由;daemon 侧持有同 token 连 WSS。

轮换:pair 重跑即轮换,旧 token 立即失效;App 端检测 401 时提示重新配对。

### 6.2 通道安全

- WSS 强制 TLS;双向鉴权:daemon 与 App 各持同一 token 派生的凭据,中继校验后按 token 路由;
- token 泄露处置:中继单租户,泄露 = 换 token + 中继重启路由表(§13 运维);
- 中继**只路由不解密业务内容**(payload 对它是不透明字节;最小化白名单后它本就只见摘要,见 §4.3)。

### 6.3 APNs 侧凭据

- `.p8` 密钥(ES256)只存在于中继服务器;泄露处置 = Apple 开发者后台撤销重签;
- device token 每台设备每重装可变,中继维护 token 表并在 410 时剪枝(§7.4)。

### 6.4 端到端加密:边界如实写明

- **alert 推送**:可行——Notification Service Extension 收到加密 payload 后本机解密再展示,中继/APNs 只见过密文;
- **Live Activity 远程更新**:**不可行**——content-state 由**系统**解码渲染 Widget,不经 App 代码,系统只认明文 JSON;
- **结论:v1 采用「最小载荷白名单」策略**(§4.3),不做 NSE 端到端——摘要本就是通知可见内容,加密它不减少任何暴露面;若未来上行字段扩容再评估 NSE 路线。

---

## 7. APNs 推送原理详解

> 本节面向不熟悉 APNs 的读者,把中继服务端要打的每一个 HTTP 头讲透。

### 7.1 它是什么

APNs(Apple Push Notification service)是苹果的常驻推送通道:iOS 设备开机即与苹果服务器保持一条长连接,所有第三方推送都经它中转。**App 不需要在运行**——这就是 L3 能补齐 L1/L2 边界的根本原因。你的服务器(中继)→ APNs → 设备,是唯一的合法投递路径;iOS 上没有「App 自己开个端口等推送」这种事(后台 socket 会被冻结)。

### 7.2 鉴权:.p8 token(选它,不用证书)

APNs 两种鉴权:

| 方式 | 原理 | 缺点 |
|---|---|---|
| 证书(推荐弃用) | 每年签名一次的 TLS 客户端证书 | **年度证书护理**:过期即全员断推,运维事故源 |
| **token(.p8)✅** | 一个椭圆曲线私钥,中继用它签名 JWT | 私钥永不过期,可随时撤销重签 |

token 模式工作方式:

1. 中继持有一个 `.p8` 私钥(Apple 后台下载,对应 Key ID `kid` 与 Team ID);
2. 每次推送前构造 JWT:header 含 `alg: ES256, kid`,payload 含 `iss: <Team ID>`,`iat: <当前时间>`;用私钥做 ES256 签名;
3. 该 JWT 放 `Authorization: Bearer` 头;**token 有效期 1 小时,中继做缓存并在过期前刷新**(标准实现:签一个、用满 50 分钟再换);
4. 每个请求还要带 `apns-topic` = App 的 Bundle ID。

### 7.3 一次推送的完整语义(HTTP/2)

APNs 是 HTTP/2 专有端点(`https://api.push.apple.com`,开发环境 `api.sandbox.push.apple.com`),每个推送是一个 POST:

```
POST /3/device/<device-token>          # device token = 设备地址,App 注册时拿到
Authorization: bearer <JWT>            # §7.2
apns-topic: <bundle-id>                # 目标 App
apns-push-type: alert                  # 见下表
apns-priority: 10                      # 10=立即送达;5=省电耐心;1=让系统挑时机
apns-expiration: <unix-epoch>          # 过期即弃;0=设备一上线就送(离线补发的极端形态)
apns-interruption-level: time-sensitive # 需 App 声明 Time Sensitive 能力
aps: { alert: {...}, sound: ... }      # 业务体
```

`apns-push-type` 三值速查(**头与 body 必须一致,错了静默拒**):

| 值 | 用途 | DevPilot 场景 |
|---|---|---|
| `alert` | 常规通知,弹横幅/锁屏 | approval_required、task_complete |
| `background` | 静默唤醒 App(≤30s 执行) | 收件箱后台同步 |
| `liveactivity` | 更新 Live Activity 的 content-state | Agent 状态卡(§9) |

`time-sensitive` 中断级别:横幅穿透专注模式(用户未关该类别时)——审批等待正该用它;需在开发者后台为 App ID 勾选 **Time Sensitive Notifications 能力**,且 Info.plist 配 `NSUserNotificationsUsageDescription` 类声明(实现期核对具体键)。

### 7.4 必须处理的应答与错误

- **HTTP 200**:送达(APNs 受理;不代表用户看到);
- **HTTP 410 + `reason: Unregistered`**:device token 已失效(卸载/重装/系统刷新)→ **中继必须剪枝该 token**,否则后续推送持续 410 浪费配额;
- **429/403/400 类**:频率超限/鉴权错/payload 非法——按错误分类进监控(§13);
- **幂等与重试**:APNs 无投递回执闭环;对 alert 类建议中继记录「已受理」即止,重试只针对连接层失败,避免重复轰炸。

### 7.5 成本

APNs **免费、无限量**(苹果按 App 质量治理而非收费);真实成本是运维义务:token 表护理、410 剪枝、错误监控。另注意:**APNs 推送要求 App 具备通知权限**——App 侧首次配对时引导授权(§11.2)。

---

## 8. 中继服务端架构

### 8.1 部署形态对比

| 方案 | 优势 | 劣势 | 判定 |
|---|---|---|---|
| **单 VPS Go 二进制** ✅ | 与 daemon **共享协议类型库**(同语言,`pkg/wire` 经 go.mod 引用,事件结构零漂移);单租户数据完全自持(对齐 DevPilot 直连隐私哲学);运维面 = 一个 systemd unit | 自己管 TLS/续期/监控 | **推荐** |
| Cloudflare Workers + Durable Objects | 免运维、边缘低延迟;仓库已有 wrangler.jsonc 先例(隐私政策页) | JS/TS 重写协议类型(与 Go daemon 双份维护);DO 计费随连接数走 | 备选,量起来再迁 |

**仓库落位**:relay 实现落 DevPilot-APNS 仓库(2026-10-18 拆分);本节为架构概要,实现级细化(代码骨架/WSS 包封/APNs 客户端/投递语义)见该仓库 `Docs/中继服务端与APNs推送设计.md`。

### 8.2 组件

```
relay(单二进制)
├─ WSS 接入:/ws/daemon 与 /ws/app 两条端点,token 鉴权
├─ 路由表:pairing_token → {daemon 连接, device token 列表}(内存 + 落盘 sqlite)
├─ APNs 客户端:HTTP/2 连接池 + .p8 JWT 缓存(1h)+ 410 剪枝
└─ 管理端点:/pair(配对)、/healthz、/metrics(Prometheus 文本)
```

### 8.3 投递语义

- approval_required:**不落盘补发**——审批有时效,daemon 侧 hook 超时(5min)兜底已把「推送到时人已不在」的后果消掉;推送失败走 APNs expiration 短窗;
- task_complete:**短窗补发**(expiration 24h)——「干完了」晚知道也比不知道好;
- 去重键:event_id,中继对同 token 短窗内去重;
- App 在线时(App 主动连 WSS 或前台)中继可直推 WSS 跳过 APNs(省一次系统横幅),收到 approval 决策回流即此路径。

---

## 9. Live Activity 远程更新

### 9.1 机制

ActivityKit 的 Live Activity 支持两种驱动:App 进程内 `update`(现状,L1)与 **APNs 远程推送**(L3 补的边界)。远程更新要点:

- Live Activity 的 push token **独立于普通 device token**,且**可变**——App 侧监听 `Activity.request(attributes:)` 返回 activity 的 `pushTokenUpdates` 异步序列,每次变更上报中继;
- 推送类型 `apns-push-type: liveactivity`,body 是 `{"event": "update", "content-state": {...}}`(`"event": "end"` + `dismissal-date` 收卡);
- `stale-date`:超时后系统把卡标灰——审批等待卡建议 stale = 10min(与 hook 超时同源);
- **Widget 层零新做**:锁屏与灵动岛 UI 已实现(`Widgets/AgentLiveActivityWidget.swift`,project.yml 已带 `NSSupportsLiveActivities: true`),relay 只需喂对 content-state。

### 9.2 content-state 与 AgentActivityAttributes 一一对应

现有共享结构(`App/Shared/AgentActivityAttributes.swift`):

```swift
// ContentState 五字段
title: String      // 会话标题      ← 事件:会话标题(白名单截断 120)
statusIcon: String // SF Symbol 名 ← 事件类型映射枚举
statusText: String // 运行中/等待审批/已完成 ← 事件类型映射枚举
isAlert: Bool      // 等待审批强调 ← approval_required ⇔ true
updatedAt: Date    //              ← 中继打点时间
// 固定属性 sessionID: String ← 一会话一张卡,daemon 侧会话 id
```

映射是**纯函数**:五事件类型 → {statusIcon, statusText, isAlert} 三元组,daemon 侧生成、中继原样转发。**content-state 即最小载荷**(§4.3 白名单),与端到端加密结论(§6.4)自洽。

### 9.3 边界

- 设备重启后 activity 不在,远程更新自然失败——收件箱仍是兜底事实源;
- 一台设备多会话 = 多张卡,每卡独立 push token,中继 token 表按 (device, sessionID) 存。

---

## 10. 收件箱与审批闭环(App 侧)

### 10.1 收件箱模型

- 数据面:五事件按 (会话 id) 单行合并;本地持久化(CoreData/SwiftData),离线可读;
- 排序:approval_required 置顶 + 强调,其余按 updatedAt;
- 与现有会话页打通:点行跳终端会话(复用 `NotificationService.openSessionNotification` 的深链通路)。

### 10.2 审批交互

- 推送通知 + Live Activity 卡片直达同一审批面板:工具名、命令摘要(即 §4.3 白名单字段,用户看到什么就判什么)、Allow / Deny;
- 决策回流的可靠性:App 在线走 WSS 直达;<30s 内 App 不在线时,轻量拉起(background push 唤醒)补发决策;超时后禁用按钮并标「已回落终端询问」;
- 锁屏 Action 按钮(Allow/Deny 直接在通知上)是 P2 增强:Notification Category 的 action 要么走 extension 要么走 background task,受系统时限约束,风险点在锁屏网络不可用时决策发不出去——v1 只做点进 App 再批。

---

## 11. App 侧接入

### 11.1 新模型,不污染 L2

- 新建 `RelayChannel`(SwiftData):`endpoint: String?`、`pairingState: String?`、`deviceToken: String?`、`registeredAt: Date?`——**全部 Optional**(HostRecord 先例:非可选新增曾致冷启 fatalError);与 `L2Channel` 并列,L2 语义一字节不动;
- Settings 新路由页「云端通道」:`SettingsView.swift` 已有 `.l2` 路由先例(L161),照抄形态(insetGrouped 自绘卡 + 全值式 NavigationLink(value:))。

### 11.2 注册时序

1. 配对成功 → 引导请求通知权限(说明文案:审批等待推送);
2. `UIApplication.registerForRemoteNotifications` 拿 device token → 上报中继;
3. Time Sensitive 能力声明进 project.yml entitlements(xcodegen glob 内);
4. Live Activity push token 上报:现有会话起卡时挂 `pushTokenUpdates` 监听,变更即上报。

### 11.3 通知呈现策略

- approval_required:Time Sensitive + 专属 Notification Category(点按深链进审批);
- task_complete:普通 alert;
- daemon 在线时的 tool_running/finished:**不弹横幅**,只走 Live Activity content-state 更新(§9)——横幅留给人必须知道的事。

---

## 12. 安装与卸载卫生(红线)

- install **幂等**:重复执行结果一致;写入 `~/.claude/settings.json` / `~/.codex/hooks.json` 只增改自家条目(key: `devpilot-hook`),**绝不整文件重写**;
- uninstall **只删自家条目**:读-改-写合并,用户自装的其它 hooks 一字不动;
- 写配置文件走「临时文件 + 原子替换 + 写前备份 `.bak`」;JSON 解析失败即拒绝写入并报错,宁可失败不可毁配置;
- doctor 自检清单:配置可解析 ✓ hooks 在位 ✓ 网关端口可听 ✓ WSS 已连 ✓ 配对有效 ✓ 中继可达 ✓。

---

## 13. 运维与可观测

- 指标:WSS 在线 daemon/App 数、事件上行速率、APNs 受理/410/429 比率、审批决策回流时延 P50/P95;
- 日志:中继只记元数据(时间/token 哈希/事件类型/字节数),**不落 payload**;
- 告警基线:410 比率突增(token 表腐坏)、WSS 断连风暴、JWT 鉴权 403(.p8 泄露/撤销信号);
- token 泄露处置 runbook:pair 轮换 → 中继重启路由 → App 重新配对;`.p8` 泄露:开发者后台撤销 → 重签 → 中继换钥。

---

## 14. 风险与开放问题

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| H1 | hook 超时策略被误配成「超时放行」 | 安全,高 | 默认即「无决策回落终端询问」;daemon 拒绝下发 allow-on-timeout 配置(白名单枚举) |
| H2 | APNs 410 剪枝缺失 → 推送黑洞 | 中 | 剪枝为一级逻辑 + 监控告警 |
| H3 | Live Activity push token 漂移未跟随 | 中 | pushTokenUpdates 监听 + 上报;卡不更新时收件箱兜底 |
| H4 | 中继单点不可用 | 中 | 与 L2/L1 三链路独立,故障面不叠加;VPS 监控 |
| H5 | Windows 服务注册兼容性 | 中 | service 子命令三平台各自验收;失败时降级「serve + 手动自启」文档 |
| H6 | 审批面板误触(锁屏口袋) | 低 | 决策需解锁(系统默认);锁屏 Action 按钮推迟到 P2 的另一理由 |

开放问题:① 中继部署地域/域名(影响延迟与合规);② 多开发机共用一个 App 配对(预期:一 App 可配多 daemon,路由表按 daemon id 区分,D1 定案);③ 收件箱数据同步到 iPad/Mac 端(云同步先例已有,v2 议)。

---

## 15. 里程碑与人周估算(量级判断,非承诺)

| 阶段 | 内容 | 人周 |
|---|---|---|
| D0 | 协议类型库定稿(`pkg/wire` 落 DevPilot-APNS 仓库,daemon go.mod 引用)+ 部署形态拍板 | 0.5 |
| D1 | daemon 核心:网关/hook 适配/配置合并卫生/事件聚合限流(落 DevPilot-Hook)| 2-2.5 |
| D2 | 配对安全 + WSS 双端 + 幂等与离线缓冲 | 1-1.5 |
| D3 | 中继服务端:路由表/APNs 客户端/410 剪枝 + 部署(落 DevPilot-APNS,细化见其 R0-R3)| 2 |
| D4 | App 侧:云端通道页/收件箱/Live Activity 远程更新 | 2-2.5 |
| D5 | 审批闭环端到端 + doctor + 压测(断连重放/限流验证) | 1.5-2 |

**合计 ≈ 8-10 人周,无外部阻塞,可立即动工**。

---

## 来源与依据

- moshi-hook 行为参考:install/pair/serve/doctor/service/uninstall 子命令、`~/.claude/settings.json`/`~/.codex/hooks.json` 合并写入、本地网关 127.0.0.1:24543、云通道最小化(「服务器只收到通知里你看得到的内容」)、五事件类型与单行收件箱(getmoshi.app/docs + DeepSeek/豆包拆解,2026-10-18)
- Claude Code hooks 协议:PreToolUse 同步 JSON permissionDecision、Notification/Stop/PostToolUse 事件、hook 超时行为(code.claude.com/docs hooks 页,2026-10-18 核验)
- APNs:Apple 官方文档(sending notification requests to APNs / .p8 token 鉴权 / apns-push-type 与 interruption-level / Live Activity 远程更新与 pushTokenUpdates / 410 Unregistered)
- 仓库内部事实:`App/LiveActivityService.swift`(「退后台被挂起后停止更新」注释=本设计要补的边界)、`App/Shared/AgentActivityAttributes.swift`(ContentState 五字段)、`Widgets/AgentLiveActivityWidget.swift`、project.yml(`NSSupportsLiveActivities`)、`App/NotificationService.swift`(openSessionNotification 深链)、`Sources/PersistenceKit/L2Channel.swift` 与 `App/Views/L2SettingsView.swift`(并列不动)、`App/Views/SettingsView.swift`(L161 .l2 路由先例)、`Sources/PersistenceKit/HostRecord.swift`(Optional 先例)
