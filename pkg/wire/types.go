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

// EventType 是收件箱五事件 + 会话结束扩展。
type EventType string

const (
	EventSessionStarted   EventType = "session_started"
	EventToolRunning      EventType = "tool_running"
	EventToolFinished     EventType = "tool_finished"
	EventApprovalRequired EventType = "approval_required"
	EventTaskComplete     EventType = "task_complete"
	EventSessionEnded     EventType = "session_ended"
)

// Valid 判断事件类型是否合法。
func (t EventType) Valid() bool {
	switch t {
	case EventSessionStarted, EventToolRunning, EventToolFinished,
		EventApprovalRequired, EventTaskComplete, EventSessionEnded:
		return true
	default:
		return false
	}
}

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
)

// Payload 是云通道允许传输的摘要白名单。
//
// 隐私红线：这里只允许出现「通知里用户看得到的内容」。完整输出、文件内容、
// 环境变量、cwd 之外的路径一律不得进入本结构。
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
	// StatusText / StatusIcon 供 Live Activity 直接使用。
	StatusText string `json:"status_text,omitempty"`
	StatusIcon string `json:"status_icon,omitempty"`
	// IsAlert 为 true 时 Live Activity 需要强调。
	IsAlert bool `json:"is_alert,omitempty"`
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
	Payload    Payload         `json:"payload"`
	TS         int64           `json:"ts"`
	Raw        json.RawMessage `json:"-"`
}

// Decision 是 App 上行的人工审批结果。
type Decision struct {
	V          int          `json:"v"`
	Kind       Kind         `json:"kind"`
	MsgID      string       `json:"msg_id"`
	ApprovalID string       `json:"approval_id"`
	Decision   DecisionKind `json:"decision"`
	TS         int64        `json:"ts"`
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
	TS         int64  `json:"ts"`
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
		return fmt.Errorf("wire: 不支持的协议版本 %d（期望 %d）", e.V, Version)
	}
	if e.Kind != KindEvent {
		return fmt.Errorf("wire: envelope kind 必须是 event，实际 %q", e.Kind)
	}
	if e.MsgID == "" || e.EventID == "" {
		return fmt.Errorf("wire: msg_id 与 event_id 不能为空")
	}
	if !e.Type.Valid() {
		return fmt.Errorf("wire: 未知事件类型 %q", e.Type)
	}
	if !e.Agent.Valid() {
		return fmt.Errorf("wire: 未知 agent %q", e.Agent)
	}
	if e.DaemonID == "" || e.SessionID == "" {
		return fmt.Errorf("wire: daemon_id 与 session_id 不能为空")
	}
	if e.TS <= 0 {
		return fmt.Errorf("wire: ts 必须为正的 Unix 秒")
	}
	if e.Type == EventApprovalRequired && e.ApprovalID == "" {
		return fmt.Errorf("wire: approval_required 必须携带 approval_id")
	}
	return nil
}

// Validate 校验审批决策。
func (d *Decision) Validate() error {
	if d.V != Version {
		return fmt.Errorf("wire: 不支持的协议版本 %d", d.V)
	}
	if d.Kind != KindDecision {
		return fmt.Errorf("wire: decision kind 必须是 decision")
	}
	if d.MsgID == "" || d.ApprovalID == "" {
		return fmt.Errorf("wire: msg_id 与 approval_id 不能为空")
	}
	if !d.Decision.Valid() {
		return fmt.Errorf("wire: 未知决策 %q", d.Decision)
	}
	if d.TS <= 0 {
		return fmt.Errorf("wire: ts 必须为正的 Unix 秒")
	}
	return nil
}

// Validate 校验 Hello。
func (h *Hello) Validate() error {
	if h.V != Version {
		return fmt.Errorf("wire: 不支持的协议版本 %d", h.V)
	}
	if h.Kind != KindHello {
		return fmt.Errorf("wire: hello kind 必须是 hello")
	}
	if h.Role != RoleDaemon && h.Role != RoleApp {
		return fmt.Errorf("wire: 未知角色 %q", h.Role)
	}
	if h.Role == RoleDaemon && h.DaemonID == "" {
		return fmt.Errorf("wire: daemon 角色必须携带 daemon_id")
	}
	if h.Role == RoleApp && h.DeviceID == "" {
		return fmt.Errorf("wire: app 角色必须携带 device_id")
	}
	return nil
}

// Validate 校验重同步请求。
func (r *Resync) Validate() error {
	if r.V != Version || r.Kind != KindResync {
		return fmt.Errorf("wire: 非法的 resync 消息")
	}
	if r.MsgID == "" {
		return fmt.Errorf("wire: resync 缺少 msg_id")
	}
	return nil
}

// Validate 校验 Live Activity token 上报。
func (l *LAToken) Validate() error {
	if l.V != Version || l.Kind != KindLAToken {
		return fmt.Errorf("wire: 非法的 la_token 消息")
	}
	if l.MsgID == "" || l.DaemonID == "" || l.SessionID == "" || l.PushToken == "" {
		return fmt.Errorf("wire: la_token 字段不完整")
	}
	switch l.Event {
	case LAStart, LAUpdate, LAEnd:
		return nil
	default:
		return fmt.Errorf("wire: 未知 la event %q", l.Event)
	}
}

// NowUnix 返回协议统一使用的 Unix 秒。
func NowUnix() int64 { return time.Now().Unix() }
