// Package wire 定义 DevPilot 三端（daemon / relay / iOS）共用的线上协议。
//
// 本包是协议的唯一事实源：daemon 通过 go.mod 引用本包，relay 直接使用，
// iOS 侧按同一份 JSON 契约实现 Codable。任何字段变更都必须同步 testdata/vectors。
package wire

import (
	"encoding/json"
	"fmt"
	"time"
)

// Version 是当前协议版本。收到不认识的版本必须拒绝，而不是猜测兼容。
const Version = 1

// Kind 是消息种类。
type Kind string

const (
	KindEvent        Kind = "event"
	KindDecision     Kind = "decision"
	KindAck          Kind = "ack"
	KindHello        Kind = "hello"
	KindHelloAck     Kind = "hello_ack"
	KindPing         Kind = "ping"
	KindPong         Kind = "pong"
	KindResync       Kind = "resync"
	KindResyncResult Kind = "resync_result"
	KindLAToken      Kind = "la_token"
	KindLATokenAck   Kind = "la_token_ack"
)

// EventType 是收件箱五事件 + 会话结束/审批关闭扩展。
type EventType string

const (
	EventSessionStarted   EventType = "session_started"
	EventToolRunning      EventType = "tool_running"
	EventToolFinished     EventType = "tool_finished"
	EventApprovalRequired EventType = "approval_required"
	EventTaskComplete     EventType = "task_complete"
	EventSessionEnded     EventType = "session_ended"
	// EventApprovalClosed 表示一条审批已终结：被批准/拒绝（手机或本地决策）、
	// 等待超时（expired）、或用户在终端侧取消（cancelled，Ctrl-C / Esc）。
	// 携带原 approval_id；relay 与 daemon 都可能发射，event_id 用
	// "close-"+approval_id 确定性去重，两条路径收敛为一条。
	EventApprovalClosed EventType = "approval_closed"
)

// Valid 判断事件类型是否合法。
func (t EventType) Valid() bool {
	switch t {
	case EventSessionStarted, EventToolRunning, EventToolFinished,
		EventApprovalRequired, EventTaskComplete, EventSessionEnded,
		EventApprovalClosed:
		return true
	default:
		return false
	}
}

// CloseReason 是 approval_closed 的 payload.status_text 取值约定。
const (
	CloseReasonApproved  = "approved"   // 手机/本地决策：允许
	CloseReasonDenied    = "denied"     // 手机/本地决策：拒绝
	CloseReasonExpired   = "expired"    // 等待窗耗尽，回落终端原生提示
	CloseReasonCancelled = "cancelled"  // 用户在终端 Ctrl-C / Esc 取消等待
)

// Agent 是 hook 来源。
type Agent string

const (
	AgentClaude Agent = "claude"
	AgentCodex  Agent = "codex"
)

// Valid 判断 agent 是否受支持。
func (a Agent) Valid() bool {
	return a == AgentClaude || a == AgentCodex
}

// DecisionKind 是人工审批结果。
type DecisionKind string

const (
	DecisionAllow DecisionKind = "allow"
	DecisionDeny  DecisionKind = "deny"
)

// Valid 判断决策值是否合法。
func (d DecisionKind) Valid() bool {
	return d == DecisionAllow || d == DecisionDeny
}

// AckStatus 是 relay/daemon 对消息的处理结果。
type AckStatus string

const (
	AckAccepted       AckStatus = "accepted"
	AckAlreadyDecided AckStatus = "already_decided"
	AckExpired        AckStatus = "expired"
	AckUnknown        AckStatus = "unknown"
	AckRejected       AckStatus = "rejected"
)

// Role 是 WSS 客户端角色。
type Role string

const (
	RoleDaemon Role = "daemon"
	RoleApp    Role = "app"
)

// LAEvent 是 Live Activity token 生命周期事件。
type LAEvent string

const (
	LAStart  LAEvent = "start"
	LAUpdate LAEvent = "update"
	LAEnd    LAEvent = "end"
	// LAPushStart 是 REST /v1/la-tokens 专用事件:上报设备的 push-to-start token
	// (云审批 LA 在 App 被杀时由此 token 起卡)。WSS la_token 消息不使用。
	LAPushStart LAEvent = "push_start"
)

// Payload 是云通道允许传输的摘要白名单。
//
// 隐私红线：摘要字段（title~is_alert）仍然只允许出现「通知里用户看得到的
// 内容」，完整输出、文件内容、任意工具产出一律不得进入。v0.3.0 新增的主机
// 路由字段（hostname~cwd）仅用于 App 端匹配 HostRecord / 直进 tmux 会话 /
// 一键补建主机预填：cwd 不进入通知文案、只在 App 内展示；除 $TMUX/$USER
// 这两个明确定义的值外，其他环境变量仍不得进入本结构。
type Payload struct {
	// Title 是会话标题（截断到 MaxTitleRunes）。
	Title string `json:"title,omitempty"`
	// SessionTitle 是会话标题的显式字段，供 Live Activity 使用。
	SessionTitle string `json:"session_title,omitempty"`
	// ToolName 是工具名（Bash/Edit/Read...）。
	ToolName string `json:"tool_name,omitempty"`
	// CommandSummary 是工具调用的可读摘要（截断到 MaxCommandRunes）。
	CommandSummary string `json:"command_summary,omitempty"`
	// ReplySummary 是 Agent 回复摘要（截断到 MaxReplyRunes）。
	ReplySummary string `json:"reply_summary,omitempty"`
	// PromptSummary 是用户 prompt 摘要（截断到 MaxPromptRunes）。
	PromptSummary string `json:"prompt_summary,omitempty"`
	// InputDigest 是 tool_input 规范化后的短哈希，用于展示与防篡改。
	InputDigest string `json:"input_digest,omitempty"`
	// Options 是选项型审批（AskUserQuestion）的候选标签，顺序保留；
	// App 审批卡渲染成选项钮，点选后经 Decision.Choice 回传。仅当
	// tool_input 能解析出唯一一问的选项时才携带（多问/解析失败不设键，
	// App 回落二值 Approve/Deny）。属摘要白名单：只有选项 label 这种
	// 「通知里用户看得到的内容」允许进入。v0.4.3 增量字段。
	Options []string `json:"options,omitempty"`
	// StatusText / StatusIcon 供 Live Activity 直接使用。
	StatusText string `json:"status_text,omitempty"`
	StatusIcon string `json:"status_icon,omitempty"`
	// IsAlert 为 true 时 Live Activity 需要强调。
	IsAlert bool `json:"is_alert,omitempty"`
	// Hostname 是 daemon 上报的主机名（os.Hostname），App 用于匹配 HostRecord。
	Hostname string `json:"hostname,omitempty"`
	// HostIP 是 daemon 尽力探测的内网 IPv4（默认路由接口首个非环回地址），
	// App 用于主机匹配与一键补建预填。
	HostIP string `json:"host_ip,omitempty"`
	// TmuxSession 是审批发生时所在 tmux 会话名（tmux display-message -p '#S'），
	// App 用于直进/attach 对应会话。
	TmuxSession string `json:"tmux_session,omitempty"`
	// TmuxWindow 是审批发生时所在 tmux 窗口序号（#{window_index}，数字串），
	// 仅 App 详情抽屉展示；不参与路由，不进 APNs 手搓 map（deliverer 不加）。
	// v0.3.1 增量字段。
	TmuxWindow string `json:"tmux_window,omitempty"`
	// User 是 hook 进程的 $USER，一键补建主机时预填用户名。
	User string `json:"user,omitempty"`
	// Cwd 是会话完整工作目录：仅用于 App 端会话/项目匹配，不进入通知文案。
	Cwd string `json:"cwd,omitempty"`
}

// Envelope 是 kind=event 的消息体。
type Envelope struct {
	V          int             `json:"v"`
	Kind       Kind            `json:"kind"`
	MsgID      string          `json:"msg_id"`
	EventID    string          `json:"event_id"`
	ApprovalID string          `json:"approval_id,omitempty"`
	Type       EventType       `json:"type"`
	Agent      Agent           `json:"agent"`
	DaemonID   string          `json:"daemon_id"`
	SessionID  string          `json:"session_id"`
	Seq        int64           `json:"seq,omitempty"`
	// ExpiresAt 是本轮审批等待窗的截止时刻（Unix 秒，daemon 盖章 = ts + 等待窗）。
	// 仅 approval_required 携带；relay 据此派生 pending TTL / APNs 过期 /
	// LA stale-date，缺失时回落旧常量。其余事件类型不得设置。
	ExpiresAt int64           `json:"expires_at,omitempty"`
	Payload   Payload         `json:"payload"`
	TS        int64           `json:"ts"`
	Raw       json.RawMessage `json:"-"`
}

// Decision 是 App 上行的人工审批结果。
type Decision struct {
	V          int          `json:"v"`
	Kind       Kind         `json:"kind"`
	MsgID      string       `json:"msg_id"`
	ApprovalID string       `json:"approval_id"`
	Decision   DecisionKind `json:"decision"`
	// Choice 是选项型审批所选的候选标签（对应 Payload.Options 中的项）。
	// 语义上总是伴随 deny：daemon 侧以「deny + 理由携带答案」喂回 agent
	// （Claude Code 的 deny 理由就是模型的输入）。relay 只透传/落库补发，
	// 不解释语义；旧 relay 不认识该键（json 忽略未知键）会静默丢弃——
	// 三端必须同步升级后选项钮才有端到端效果。v0.4.3 增量字段。
	Choice string `json:"choice,omitempty"`
	TS     int64  `json:"ts"`
}

// Ack 是对事件或决策的确认。
type Ack struct {
	V          int       `json:"v"`
	Kind       Kind      `json:"kind"`
	MsgID      string    `json:"msg_id"`
	RefMsgID   string    `json:"ref_msg_id,omitempty"`
	EventID    string    `json:"event_id,omitempty"`
	ApprovalID string    `json:"approval_id,omitempty"`
	Status     AckStatus `json:"status"`
	TS         int64     `json:"ts"`
}

// Hello 是 WSS 建连后的第一条消息。
type Hello struct {
	V          int    `json:"v"`
	Kind       Kind   `json:"kind"`
	MsgID      string `json:"msg_id"`
	Role       Role   `json:"role"`
	DaemonID   string `json:"daemon_id,omitempty"`
	DeviceID   string `json:"device_id,omitempty"`
	LastAckSeq int64  `json:"last_ack_seq,omitempty"`
	Resync     bool   `json:"resync,omitempty"`
	// Cursor 是 App 上次对账的最新事件 seq（resync_result.next_cursor 回传值）。
	// resync 对账按此回放「之后」的最新窗口；空 = 全新档，回最新 resyncLimit 条。
	// 旧版 App 不发此键（json 忽略未知键），对账退化为零游标 = 旧行为，加性演进
	Cursor string `json:"cursor,omitempty"`
	TS     int64  `json:"ts"`
}

// HelloAck 是服务端对 Hello 的确认。
type HelloAck struct {
	V        int    `json:"v"`
	Kind     Kind   `json:"kind"`
	MsgID    string `json:"msg_id"`
	RefMsgID string `json:"ref_msg_id"`
	ServerTS int64  `json:"server_ts"`
}

// Ping / Pong 用于应用层心跳（WSS 控制帧之外的可观测心跳）。
type Ping struct {
	V    int   `json:"v"`
	Kind Kind  `json:"kind"`
	TS   int64 `json:"ts"`
}

// Pong 是 Ping 的应答。
type Pong struct {
	V       int   `json:"v"`
	Kind    Kind  `json:"kind"`
	RefTime int64 `json:"ref_time"`
	TS      int64 `json:"ts"`
}

// Resync 是 App 重连后的对账请求。
type Resync struct {
	V      int    `json:"v"`
	Kind   Kind   `json:"kind"`
	MsgID  string `json:"msg_id"`
	Since  int64  `json:"since,omitempty"`
	Cursor string `json:"cursor,omitempty"`
	TS     int64  `json:"ts"`
}

// ResyncResult 是 relay 返回的未完成审批与收件箱增量。
type ResyncResult struct {
	V          int        `json:"v"`
	Kind       Kind       `json:"kind"`
	MsgID      string     `json:"msg_id"`
	RefMsgID   string     `json:"ref_msg_id"`
	Pending    []Envelope `json:"pending,omitempty"`
	Events     []Envelope `json:"events,omitempty"`
	NextCursor string     `json:"next_cursor,omitempty"`
	TS         int64      `json:"ts"`
}

// LAToken 是 App 上报 Live Activity push token。
type LAToken struct {
	V         int     `json:"v"`
	Kind      Kind    `json:"kind"`
	MsgID     string  `json:"msg_id"`
	DaemonID  string  `json:"daemon_id"`
	SessionID string  `json:"session_id"`
	PushToken string  `json:"push_token"`
	Event     LAEvent `json:"event"`
	TS        int64   `json:"ts"`
}

// LATokenAck 是 relay 对 token 上报的确认。
type LATokenAck struct {
	V      int       `json:"v"`
	Kind   Kind      `json:"kind"`
	MsgID  string    `json:"msg_id"`
	RefID  string    `json:"ref_msg_id"`
	Status AckStatus `json:"status"`
	TS     int64     `json:"ts"`
}

// ---------- 校验 ----------

// Validate 校验事件包封。
func (e *Envelope) Validate() error {
	if e.V != Version {
		return fmt.Errorf("wire: unsupported protocol version %d (want %d)", e.V, Version)
	}
	if e.Kind != KindEvent {
		return fmt.Errorf("wire: envelope kind must be event, got %q", e.Kind)
	}
	if e.MsgID == "" || e.EventID == "" {
		return fmt.Errorf("wire: msg_id and event_id must not be empty")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("wire: unknown event type %q", e.Type)
	}
	if !e.Agent.Valid() {
		return fmt.Errorf("wire: unknown agent %q", e.Agent)
	}
	if e.DaemonID == "" || e.SessionID == "" {
		return fmt.Errorf("wire: daemon_id and session_id must not be empty")
	}
	if e.TS <= 0 {
		return fmt.Errorf("wire: ts must be a positive unix timestamp")
	}
	if e.Type == EventApprovalRequired && e.ApprovalID == "" {
		return fmt.Errorf("wire: approval_required requires approval_id")
	}
	if e.Type == EventApprovalClosed && e.ApprovalID == "" {
		return fmt.Errorf("wire: approval_closed requires approval_id")
	}
	return nil
}

// Validate 校验审批决策。
func (d *Decision) Validate() error {
	if d.V != Version {
		return fmt.Errorf("wire: unsupported protocol version %d", d.V)
	}
	if d.Kind != KindDecision {
		return fmt.Errorf("wire: decision kind must be decision")
	}
	if d.MsgID == "" || d.ApprovalID == "" {
		return fmt.Errorf("wire: msg_id and approval_id must not be empty")
	}
	if !d.Decision.Valid() {
		return fmt.Errorf("wire: unknown decision %q", d.Decision)
	}
	// Choice 是选项标签回传：超帽即畸形（App 侧已按 MaxOptionRunes 截断），
	// 拒绝而非截断——不认识的形状必须拒绝，这是本包的纪律。
	if len([]rune(d.Choice)) > MaxOptionRunes {
		return fmt.Errorf("wire: choice exceeds %d runes", MaxOptionRunes)
	}
	if d.TS <= 0 {
		return fmt.Errorf("wire: ts must be a positive unix timestamp")
	}
	return nil
}

// Validate 校验 Hello。
func (h *Hello) Validate() error {
	if h.V != Version {
		return fmt.Errorf("wire: unsupported protocol version %d", h.V)
	}
	if h.Kind != KindHello {
		return fmt.Errorf("wire: hello kind must be hello")
	}
	if h.Role != RoleDaemon && h.Role != RoleApp {
		return fmt.Errorf("wire: unknown role %q", h.Role)
	}
	if h.Role == RoleDaemon && h.DaemonID == "" {
		return fmt.Errorf("wire: daemon role requires daemon_id")
	}
	if h.Role == RoleApp && h.DeviceID == "" {
		return fmt.Errorf("wire: app role requires device_id")
	}
	return nil
}

// Validate 校验重同步请求。
func (r *Resync) Validate() error {
	if r.V != Version || r.Kind != KindResync {
		return fmt.Errorf("wire: invalid resync message")
	}
	if r.MsgID == "" {
		return fmt.Errorf("wire: resync missing msg_id")
	}
	return nil
}

// Validate 校验 Live Activity token 上报。
func (l *LAToken) Validate() error {
	if l.V != Version || l.Kind != KindLAToken {
		return fmt.Errorf("wire: invalid la_token message")
	}
	if l.MsgID == "" || l.DaemonID == "" || l.SessionID == "" || l.PushToken == "" {
		return fmt.Errorf("wire: la_token is missing required fields")
	}
	switch l.Event {
	case LAStart, LAUpdate, LAEnd:
		return nil
	default:
		return fmt.Errorf("wire: unknown la event %q", l.Event)
	}
}

// NowUnix 返回协议统一使用的 Unix 秒。
func NowUnix() int64 { return time.Now().Unix() }
