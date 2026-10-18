# DevPilot-APNS 中继服务端设计(relay + APNs 推送)

> 状态:评审稿 v1(2026-10-18)|性质:决策 + 设计文档,**本期不含实现**
> 上游总设计:[DevPilot-Hook守护进程与推送中继设计.md](DevPilot-Hook守护进程与推送中继设计.md)(daemon+relay 通盘;§7 APNs 原理详解、§8 中继架构为本文概要源,本文是其**实现级细化**)
> 关联:[Agent基建总纲与路线图.md](Agent基建总纲与路线图.md)

---

## 1. 定位与范围

DevPilot-APNS 是部署在**云服务器 VPS** 上的推送中继(relay):左手接开发机上的 hook daemon(WSS),右手接 Apple APNs(HTTP/2),按配对 token 把 Agent 事件路由到用户的 iPhone,并把审批决策路由回 daemon。

**它存在的理由**:iOS 后台机制下,App 被杀/锁屏后任何进程内链路(L1)与自建边缘链路(L2)都不可靠;只有 APNs 能保证到达。中继是 daemon 与 APNs 之间唯一合法的桥。

**做**:WSS 接入(daemon/App 两类客户端)、配对路由表、APNs 客户端(.p8 JWT/连接池/410 剪枝)、Live Activity 远程更新转发、投递语义。
**不做**:不读业务内容(只路由,见 §2 安全);不多租户(单租户自部署);不存 payload(日志只记元数据);不做 Web 控制台。

## 2. 三项目关系(两 Go 一 iOS,一条链路)

| | DevPilot | DevPilot-Hook | **DevPilot-APNS(本仓库)** |
|---|---|---|---|
| 形态 | iOS App(Swift/SwiftUI)| Go 常驻 daemon(用户开发机)| **Go relay(云 VPS)** |
| 链路角色 | WSS 客户端(App 端):收推送、上行审批决策 | WSS 客户端(daemon 端):上行事件、收决策 | **中枢:鉴权、路由表、APNs 推送** |
| 代码边界 | 终端/SSH/mosh、收件箱 UI、审批面板、Live Activity、RelayChannel 模型、device token 注册 | hook 适配(Claude Code/Codex)、本地网关 127.0.0.1:24543、事件聚合限流、离线缓冲 | relay 全部 + **`pkg/wire` 协议类型库(三端线上协议的单一事实源)** |
| 协议依赖 | 按 JSON 契约实现(Swift Codable 对照) | `go.mod` 引用本仓库 `pkg/wire` | 定义并持有 `pkg/wire` |

```
Claude Code/Codex hooks
   → ① DevPilot-Hook daemon(本地网关 127.0.0.1:24543,事件包封 pkg/wire)
   → ② DevPilot-APNS relay(本仓库:鉴权 → 查路由表 → 选通道)
   → ③ Apple APNs(HTTP/2)/ 或 App WSS 在线直推
   → ④ DevPilot iOS(通知/Live Activity/收件箱)
审批回流:④ decision → ② relay 路由回该 daemon → ① daemon 应答 hook → Agent 继续
```

**线上协议是三端唯一契约**:`pkg/wire`(Go)定义包封与消息结构;iOS 侧按同一 JSON 契约写 Codable;三端各自持有契约测试向量(同一组样例消息必须同解),协议漂移在 CI 层暴露而不是线上。

## 3. 为什么 Go(选型论证)

用户直觉正确,Go 是本组件的合适选择:

1. **APNs 强制 HTTP/2**:`net/http` 打开 `ForceAttemptHTTP2` 即原生 h2(ALPN 自动协商),连接池与多路复用由 Transport 托管,无需第三方 h2 客户端;
2. **并发模型贴合推送 fan-out**:一次审批可能同时打 N 台设备的 alert + M 张 Live Activity 卡,goroutine per push + 信号量限流是教科书场景;channel 天然适合「WSS 收包 → 投递 worker 池」的流水线;
3. **单二进制部署**:VPS 上 scp + systemd unit 即上线;`GOOS=linux GOARCH=amd64` 交叉编译零依赖(选纯 Go 依赖链,sqlite 用 modernc 驱动或改用 bbolt,免 cgo);
4. **与 daemon 同语言**:`pkg/wire` 一份类型两边编译,零漂移;
5. **密码学全齐**:ES256(.p8 JWT 签名)= 标准库 `crypto/ecdsa`(P-256)+ `crypto/sha256`,无需额外依赖。

否决项:Swift(VPS 服务端生态弱)、Node/TS(与 daemon 双份协议类型)、Rust(交付速度不划算)。

## 4. 部署形态

- 单 VPS、单二进制、单 systemd unit(`devpilot-apns.service`);
- TLS:Let's Encrypt `autocert`(golang.org/x/crypto/acme/autocert)自动续期,443 对外;
- 存储:bbolt 单文件(纯 Go、零 schema)默认;数据规模 = 路由表(设备×token),KV 足够,需要复杂查询再迁 sqlite(modernc.org/sqlite);
- 配置:环境变量 + flags(`APNS_KEY_PATH`/`KEY_ID`/`TEAM_ID`/`BUNDLE_ID`/`LISTEN_ADDR`/`DOMAIN`);
- 备份:bbolt 单文件快照即全量(路由表可重建,丢失后果 = 重新配对,故备份策略从宽)。

## 5. 代码骨架

```
devpilot-apns/
├─ cmd/relay/main.go        # 装配:配置/日志/HTTP 服务/wire 包
├─ internal/
│  ├─ wss/                  # WSS 接入:daemon 与 App 两类端点,鉴权,心跳
│  ├─ route/                # 路由表:pairing→device→token;bbolt 存储
│  ├─ apns/                 # APNs 客户端:JWT 缓存/h2 连接/并发闸/错误分类/410 剪枝
│  ├─ pairing/              # 配对码校验、凭据派生、轮换
│  └─ deliver/              # 投递语义:选通道(App WSS 在线直推 vs APNs)、去重、限流
├─ pkg/wire/                # 协议类型库(单一事实源;DevPilot-Hook go.mod 引用)
└─ Docs/
```

WSS 库:gorilla/websocket(生态最广)默认;依赖策略:全部纯 Go,保交叉编译。

## 6. WSS 协议与包封(实现级)

### 6.1 包封(`pkg/wire.Envelope`)

```json
{
  "v": 1,
  "kind": "event | decision | ack | ping",
  "event_id": "uuid —— 幂等键,中继按此去重",
  "type": "session_started | tool_running | tool_finished | approval_required | task_complete",
  "agent": "claude | codex",
  "session_id": "daemon 侧会话 id(收件箱单行合并键)",
  "payload": { "title": "...", "command_summary": "...", "reply_summary": "..." },
  "ts": 1760770000
}
```

- payload 字段 = 上游总设计 §4.3 白名单(标题 120/命令 256/回复 80/prompt 200 截断),**中继不做二次校验白名单**(信任 daemon 是自家代码,但日志不落 payload);
- `decision` 消息:`{kind:"decision", approval_id, decision:"allow"|"deny", ts}`;
- 心跳:WSS ping/pong 30s;90s 无 pong 判死,踢出路由。

### 6.2 端点

| 端点 | 鉴权 | 用途 |
|---|---|---|
| `POST /v1/pair` | 一次性配对码 | App 注册:换 App 凭据 + 登记 APNs device token |
| `GET /ws/daemon` | daemon 凭据(Bearer) | daemon 长连接:上行事件/收决策 |
| `GET /ws/app` | App 凭据(Bearer) | App 长连接:收事件直推/上行决策 |
| `GET /healthz`、`GET /metrics` | — | 存活 / Prometheus 文本 |

### 6.3 路由表(bbolt buckets)

```
pairings   key=sha256(配对码)   value={created_at, expires_at, daemon_cred_hash}   # 一次性,用后即焚
devices    key=sha256(配对token) value={apns_token, app_cred_hash, registered_at, last_seen}
la_tokens  key=device_hash+"/"+session_id  value={push_token, updated_at}          # Live Activity 卡,可变
```

## 7. APNs 客户端(核心组件)

### 7.1 鉴权与 JWT 缓存

- `.p8` 私钥文件(0600,systemd 凭据挂载或环境变量路径);启动时解析一次;
- JWT = header `{alg:ES256, kid}` + payload `{iss:TEAM_ID, iat}`,`crypto/ecdsa` P-256 签名;
- **缓存 50 分钟**(有效期 1h,留 10min 余量),`sync.Mutex` 保护,过期前惰性刷新——绝不为每条推送现签。

### 7.2 连接与并发(响应"Go 处理 APNs 并发"的关切)

- 单例 `http.Client{Transport: &http.Transport{ForceAttemptHTTP2: true}}`:h2 多路复用,连接数由 Transport 自动管理(APNs 侧按 provider 限流,观测 429 并退避,不主动开多 client);
- 并发闸:信号量限制在途推送数(默认 64),`context` 超时 10s/条;
- 一次事件的扇出(多设备/多 LA 卡)用 `errgroup` 并发,单条失败不阻塞整批;
- 退避:429 指数退避(0.5s→1s→2s,封顶 30s),按 (topic) 全局退避而非单 token。

### 7.3 响应错误分类(逐类必处理)

| APNs 应答 | 含义 | 处置 |
|---|---|---|
| 200 | 受理 | 记「已受理」即止(APNs 无投递回执闭环,不重试) |
| **410 Unregistered** | device token 失效(卸载/重装/系统刷新) | **立即从路由表剪枝** + 指标告警(剪枝缺失 = 推送黑洞) |
| 429 | 频率超限 | 指数退避;持续 429 = 告警 |
| 403 | 鉴权失败 | **告警级**:`.p8` 被撤销/`kid`/`TEAM_ID` 配错;不重试 |
| 400 | payload 非法 | 常量级 bug,告警并丢弃;不重试 |
| 5xx | APNs 侧故障 | 限次重试(≤2),连接层失败也在此列 |

### 7.4 推送构造

- alert 类:头三件套 `apns-push-type: alert` / `apns-priority: 10` / `apns-interruption-level: time-sensitive`(仅 approval_required);`apns-expiration` 按事件类型(§8);
- body 即 `payload` 白名单字段的 `aps.alert` 投影——**通知里显示什么,服务器就只有什么**;
- background 类预留(收件箱后台同步),v1 不启用。

## 8. 投递语义

| 事件 | APNs expiration | 落盘补发 | 说明 |
|---|---|---|---|
| approval_required | **+300s**(与 hook 超时同源) | 否 | 审批有时效;超时后 daemon 侧已回落终端询问,迟到的推送只会误导 |
| task_complete | +24h | 短窗(内存队列) | 「干完了」晚知道也比不知道好 |
| tool_running/finished | 不打 APNs | — | 只走 Live Activity content-state(§9);App WSS 在线则直推 WSS |
| session_started | 不打 APNs | — | 同上,仅收件箱数据 |

去重:`event_id` 短窗 LRU(10min);App WSS 在线时**跳过 APNs 直推**(省系统横幅,审批决策回流即此路径)。

## 9. Live Activity 远程更新转发

- relay 是**纯转发**:content-state 五字段(`title/statusIcon/statusText/isAlert/updatedAt`)由 daemon 按 `pkg/wire` 生成,relay 原样装进 `liveactivity` 推送;
- 推送体:`{"aps":{"timestamp":..., "event":"update"|"end", "content-state":{...五字段...}, "stale-date": ts+600}}`(stale 10min 与 hook 超时同源;`end` 附 `dismissal-date`);
- token 表:`la_tokens` 按 (device, sessionID) 存;App 端监听 `pushTokenUpdates` 变更即上报,relay 覆盖旧值;
- **content-state 对齐 `AgentActivityAttributes`**(DevPilot 仓库 App/Shared),Widget 层零新做;
- 边界:content-state 由系统解码渲染,结构上不可端到端加密——白名单即防线(上游总设计 §6.4)。

## 10. 配对与安全

- 配对码:daemon `pair` 生成,高熵、一次性、短时效(15min);App 提交 `/v1/pair` 后焚毁,双端各持由配对 token 派生的长期凭据(只存哈希,bbolt 里不落明文);
- 全链路 TLS 强制;WSS 升级时校验 Bearer 凭据并绑定连接↔pairing;
- 轮换:daemon 重新 `pair` 即轮换,旧 pairing 失效、旧连接被踢;App 端 401 → 提示重新配对;
- 单租户泄露 runbook:换配对 token + 重启路由(bbolt 清 `pairings`/`devices`)+ 双端重新配对;`.p8` 泄露:开发者后台撤销 → 重签 → 换钥重启;
- 中继**只路由不读内容**:payload 对它是不透明 JSON(白名单使「读」也无所得)。

## 11. 可观测与运维

- `/metrics`:WSS 在线数(daemon/App 分列)、事件上行速率、APNs 受理/410/429/403 计数、审批决策回流时延 P50/P95、去重命中数;
- 日志:结构化,只记元数据(时间/token 哈希前 8 位/事件类型/字节数),**不落 payload**;
- 告警基线:410 比率突增(token 表腐坏)、403 出现(凭据事故)、WSS 断连风暴、429 持续;
- systemd:`Restart=always` + 看门狗;bbolt 文件入每日快照(可选)。

## 12. 风险清单

| # | 风险 | 等级 | 缓解 |
|---|---|---|---|
| A1 | 410 剪枝缺失 → 推送黑洞 | 高 | 剪枝一级逻辑 + 410 率告警(§7.3) |
| A2 | `.p8` 泄露/撤销 → 全员断推 | 高 | 403 告警;runbook(§10);钥文件 0600 |
| A3 | relay 单点故障 | 中 | 与 L1/L2 三链路独立;systemd 自拉起;监控 |
| A4 | APNs 429 限流风暴(多设备扇出) | 中 | 并发闸 + 全局退避(§7.2) |
| A5 | `pkg/wire` 三端漂移 | 中 | 契约测试向量三仓同源(§2);go.mod 语义化版本 |
| A6 | token 表腐坏/丢失 → 需重新配对 | 低 | bbolt 快照;丢失后果可接受(重新配对 1 分钟) |

## 13. 里程碑与人周估算(量级判断,非承诺)

| 阶段 | 内容 | 人周 |
|---|---|---|
| R0 | `pkg/wire` 协议包 + 契约测试向量(三仓同源基准) | 0.5 |
| R1 | WSS 接入(daemon/App 端点)+ 路由表 + 配对 | 1-1.5 |
| R2 | APNs 客户端(JWT 缓存/h2 并发闸/错误分类/410 剪枝)+ 投递语义 | 1.5 |
| R3 | 部署(VPS/systemd/TLS/监控)+ 与 daemon、App 联调压测(断连重放/去重验证) | 0.75-1 |

**合计 ≈ 3.75-4.5 人周**;总账与总纲一致(原 D0/D3/半 D5 划入本仓库,其余留在 DevPilot-Hook)。本仓库与 DevPilot-Hook 均无外部阻塞,可并行开工,R0 先行(hook 侧等 `pkg/wire` 定稿再接)。

## 14. 验收(relay 侧)

1. App 被杀 + 锁屏,daemon 上行 approval_required → APNs 受理 ≤1s(P95,服务端侧);
2. App WSS 在线时,审批 decision 回流至 daemon ≤200ms(P95);
3. 同 `event_id` 重放去重;relay 重启后路由表完整恢复(bbolt 持久化);
4. 卸载 App 后再推:410 → token 剪枝 + 告警触发;
5. `apns-push-type` 与 body 不一致类错误为 0(上线前用 sandbox 端点矩阵验证 alert/liveactivity/background 三型)。

---

## 来源与依据

- 上游总设计:DevPilot 仓库 `Docs/DevPilot-Hook守护进程与推送中继设计.md`(§4 事件/§6 配对/§7 APNs 原理详解/§8 中继架构/§13 运维——本文为其 relay 部分的实现级展开)
- Apple:APNs HTTP/2 协议与应答码、.p8 token 鉴权、Live Activity 远程更新(同上游引用)
- Go 生态事实:net/http 原生 HTTP/2(ForceAttemptHTTP2)、crypto/ecdsa ES256、gorilla/websocket、bbolt/modernc sqlite 纯 Go、autocert
- 三仓库拆分决策:2026-10-18 用户拍板(daemon 与 relay 分仓)
