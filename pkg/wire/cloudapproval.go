package wire

// 云审批 Live Activity（CloudApprovalAttributes）推送契约。
//
// 与 liveactivity.go（AgentActivityAttributes，本地 SSH 会话进度卡）并存：
// 本结构面向「随时可批」的审批卡——锁屏/灵动岛常驻、needsYou 态带
// Approve/Deny 按钮与倒计时，决策后变形为终态并由 end 推送收场。
//
// 注意：字段名使用 camelCase，以匹配 Swift 端 CloudApprovalAttributes.ContentState
// 的默认 Codable 键名；时间一律 Unix epoch 秒（整数），理由同 liveactivity.go 头注。

// 云审批卡状态机取值。iOS 端按 state 渲染按钮/倒计时/终态文案。
const (
	CloudStateWorking   = "working"   // 会话运行中（非审批等待）
	CloudStateNeedsYou  = "needsYou"  // 等待用户审批：显示 Approve/Deny + 倒计时
	CloudStateApproved  = "approved"  // 已批准（终态）
	CloudStateDenied    = "denied"    // 已拒绝（终态）
	CloudStateExpired   = "expired"   // 等待超时（终态，收件箱转 Missed）
	CloudStateCancelled = "cancelled" // 终端侧取消（终态）
	CloudStateSent      = "sent"      // App Intent 本地已发决策、等 relay 确认
)

// CloudApprovalAttributesType 是 push-to-start 推送必须携带的 attributes-type，
// 值须与 Xcode 端 ActivityAttributes 类型名逐字一致（含模块无关的短名）。
const CloudApprovalAttributesType = "CloudApprovalAttributes"

// CloudApprovalState 与 iOS 端 CloudApprovalAttributes.ContentState 一一对应。
type CloudApprovalState struct {
	Title          string `json:"title"`
	ToolName       string `json:"toolName,omitempty"`
	CommandSummary string `json:"commandSummary,omitempty"`
	HostName       string `json:"hostName,omitempty"`
	ApprovalID     string `json:"approvalId,omitempty"`
	State          string `json:"state"`
	// ExpiresAt 是倒计时终点（Unix 秒）；仅 needsYou 态有意义。
	ExpiresAt int64 `json:"expiresAt,omitempty"`
	UpdatedAt int64 `json:"updatedAt"`
}

// CloudApprovalAPS 是 APNs liveactivity 推送的 aps 体（云审批卡）。
type CloudApprovalAPS struct {
	Timestamp      int64              `json:"timestamp"`
	Event          string             `json:"event"` // start|update|end
	ContentState   CloudApprovalState `json:"content-state"`
	Attributes     map[string]any     `json:"attributes,omitempty"`      // 仅 push-to-start 的 start 事件需要
	AttributesType string             `json:"attributes-type,omitempty"` // 同上，= CloudApprovalAttributesType
	StaleDate      int64              `json:"stale-date,omitempty"`
	DismissalDate  int64              `json:"dismissal-date,omitempty"`
}

// CloudApprovalPush 是完整的云审批 LA 推送体。
type CloudApprovalPush struct {
	APS CloudApprovalAPS `json:"aps"`
}

// CloudStateFor 把事件纯函数式映射为云审批卡状态。
func CloudStateFor(env Envelope) string {
	switch env.Type {
	case EventApprovalRequired:
		return CloudStateNeedsYou
	case EventApprovalClosed:
		// 终态映射：StatusText 携带 CloseReason 取值。
		switch env.Payload.StatusText {
		case CloseReasonApproved:
			return CloudStateApproved
		case CloseReasonDenied:
			return CloudStateDenied
		case CloseReasonExpired:
			return CloudStateExpired
		case CloseReasonCancelled:
			return CloudStateCancelled
		default:
			return CloudStateApproved
		}
	default:
		// tool_running / task_complete 等非审批事件到达时卡片回到进行中形态；
		// 终局由 deliverer 用 end 推送收场，状态值本身不再是重点。
		return CloudStateWorking
	}
}

// CloudApprovalStateFor 把事件映射为完整 ContentState。
func CloudApprovalStateFor(env Envelope, now int64) CloudApprovalState {
	title := env.Payload.SessionTitle
	if title == "" {
		title = env.Payload.Title
	}
	if title == "" {
		title = env.Payload.Hostname
	}
	if title == "" {
		title = Truncate(env.SessionID, 8)
	}
	var expires int64
	if CloudStateFor(env) == CloudStateNeedsYou {
		expires = env.ExpiresAt
	}
	return CloudApprovalState{
		Title:          Truncate(title, MaxTitleRunes),
		ToolName:       env.Payload.ToolName,
		CommandSummary: Truncate(env.Payload.CommandSummary, MaxCommandRunes),
		HostName:       env.Payload.Hostname,
		ApprovalID:     env.ApprovalID,
		State:          CloudStateFor(env),
		ExpiresAt:      expires,
		// 统一用发送时刻打点，避免 daemon 与 relay 时钟差导致卡片时间倒退。
		UpdatedAt: now,
	}
}

// CloudApprovalPushFor 生成一次云审批卡推送体。
//
// isStart 为 true 时生成 push-to-start 的 start 事件（带 attributes 与
// attributes-type，App 被杀也能起卡）；否则按事件类型生成 update / end。
func CloudApprovalPushFor(env Envelope, now int64, isStart bool) CloudApprovalPush {
	event := "update"
	var dismissal int64
	switch env.Type {
	case EventApprovalClosed, EventTaskComplete, EventSessionEnded:
		event = "end"
		dismissal = now + 5 // 终态短暂停留后由系统收卡（Moshi 式变形余量）
	}

	stale := int64(0)
	if event == "start" || event == "update" {
		if env.Type == EventApprovalRequired && env.ExpiresAt > now {
			// 等待窗走完 + 60s 宽限后卡片标旧（终局 close 推送会先到）。
			stale = env.ExpiresAt + 60
		} else {
			stale = now + LiveActivityStaleSeconds
		}
	}

	aps := CloudApprovalAPS{
		Timestamp:     now,
		Event:         event,
		ContentState:  CloudApprovalStateFor(env, now),
		StaleDate:     stale,
		DismissalDate: dismissal,
	}
	if isStart {
		aps.Event = "start"
		aps.Attributes = map[string]any{
			"sessionId": env.SessionID,
			"daemonId":  env.DaemonID,
		}
		aps.AttributesType = CloudApprovalAttributesType
	}
	return CloudApprovalPush{APS: aps}
}
