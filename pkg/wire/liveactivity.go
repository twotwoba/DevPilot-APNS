package wire

// ContentState 与 iOS 端 AgentActivityAttributes.ContentState 一一对应。
//
// 注意：字段名使用 camelCase，以匹配 Swift 默认 Codable 的键名。
// UpdatedAt 是 Unix epoch 秒（整数）——ActivityKit 由系统解码 content-state，
// 不会使用 App 设置的 ISO8601 策略，因此 iOS 侧必须自定义 Codable 解码。
type ContentState struct {
	Title      string `json:"title"`
	StatusIcon string `json:"statusIcon"`
	StatusText string `json:"statusText"`
	IsAlert    bool   `json:"isAlert"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// LiveActivityAPS 是 APNs liveactivity 推送的 aps 体。
type LiveActivityAPS struct {
	Timestamp     int64        `json:"timestamp"`
	Event         string       `json:"event"`
	ContentState  ContentState `json:"content-state"`
	StaleDate     int64        `json:"stale-date,omitempty"`
	DismissalDate int64        `json:"dismissal-date,omitempty"`
}

// LiveActivityPush 是完整的 Live Activity 推送体。
type LiveActivityPush struct {
	APS LiveActivityAPS `json:"aps"`
}

// ContentStateFor 把事件纯函数式映射为 Live Activity 五字段。
func ContentStateFor(env Envelope) ContentState {
	title := env.Payload.SessionTitle
	if title == "" {
		title = env.Payload.Title
	}
	if title == "" {
		title = Truncate(env.SessionID, 8)
	}
	title = Truncate(title, MaxTitleRunes)

	icon, text, alert := statusFor(env.Type)
	return ContentState{
		Title:      title,
		StatusIcon: icon,
		StatusText: text,
		IsAlert:    alert,
		UpdatedAt:  env.TS,
	}
}

func statusFor(t EventType) (icon, text string, alert bool) {
	switch t {
	case EventSessionStarted:
		return "play.circle.fill", "Running", false
	case EventToolRunning:
		return "gearshape.fill", "Running", false
	case EventToolFinished:
		return "checkmark.circle", "Running", false
	case EventApprovalRequired:
		return "exclamationmark.triangle.fill", "Waiting for approval", true
	case EventTaskComplete:
		return "checkmark.seal.fill", "Completed", false
	case EventSessionEnded:
		return "xmark.circle", "Ended", false
	default:
		return "circle", "Running", false
	}
}

// LiveActivityPushFor 生成一次 update/end 推送体。
func LiveActivityPushFor(env Envelope, now int64) LiveActivityPush {
	event := "update"
	var dismissal int64
	if env.Type == EventSessionEnded || env.Type == EventTaskComplete {
		event = "end"
		dismissal = now + LiveActivityStaleSeconds
	}
	cs := ContentStateFor(env)
	// 统一用发送时刻打点，避免 daemon 与 relay 时钟差导致卡片时间倒退。
	cs.UpdatedAt = now
	return LiveActivityPush{APS: LiveActivityAPS{
		Timestamp:     now,
		Event:         event,
		ContentState:  cs,
		StaleDate:     now + LiveActivityStaleSeconds,
		DismissalDate: dismissal,
	}}
}
