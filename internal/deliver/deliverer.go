// Package deliver 实现 relay 的 APNs 投递语义。
//
// 规则（与设计文档 §4.5 一致,v0.3.0 增补）：
//   - approval_required：始终发 alert（priority 10、time-sensitive、过期=等待窗），
//     同时驱动两类 Live Activity（本地会话进度卡 + 云审批卡 push-to-start）；
//   - approval_closed：发同 collapse-id 的替换推送（Approved/Denied/Missed/
//     Answered at terminal），让已投递横幅「变脸」；APNs 无法撤回已投递 alert，
//     替换是平台上限，锁屏干净退场靠云审批卡的 end 推送；
//   - task_complete：始终发 alert（priority 5、24h 过期）与 Live Activity end；
//   - tool_* / session_started / session_ended：不单独发 alert；
//   - Live Activity 按 (device, session) 1 秒节流、latest-wins；
//   - 审批范围推送（approval alert + 云审批 LA）默认豁免 UTC 日配额；
//   - 410 表示 token 失效，立即剪枝，不做重试。
package deliver

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/apns"
	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/quota"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// 投递超时与节流参数。
const (
	pushTimeout      = 10 * time.Second
	defaultThrottle  = time.Second
	throttleIdle     = 60 * time.Second
	taskExpiry       = 24 * time.Hour
	approvalExpiry   = wire.APNSApprovalExpirySeconds
	liveActivityTTL  = wire.LiveActivityStaleSeconds
	closeExpiry      = 60 // close 替换推送的短过期:只服务「变脸」,过期即无声消失
	approvalPushKind = "alert"
	laPushKind       = "liveactivity"
	cloudLAPushKind  = "liveactivity:cloud"
	// legacyApprovalCategory 是 v0.2 及之前的审批 category(拼写笔误)。
	legacyApprovalCategory = "DEVPLOT_APPROVAL"
)

// Pusher 是 APNs 客户端的窄接口，便于测试注入。
type Pusher interface {
	Send(ctx context.Context, req apns.Request) (apns.Result, error)
	TopicFor() string
	LiveActivityTopic() string
}

// Config 是 Deliverer 依赖。
type Config struct {
	Store    *store.Store
	Pusher   Pusher
	Metrics  *metrics.Metrics
	Logger   *slog.Logger
	Quota    *quota.Engine
	Now      func() time.Time
	Throttle time.Duration
	// APNSCategory 是审批通知 category;空值回落旧串(拼写修正的分期切换见 config)。
	APNSCategory string
	// QuotaExemptApprovalPushes 时审批范围推送豁免日配额(见 push())。
	QuotaExemptApprovalPushes bool
}

// Deliverer 实现 wss.Notifier。
type Deliverer struct {
	cfg      Config
	log      *slog.Logger
	now      func() time.Time
	throttle *Throttler
}

// New 构造 Deliverer。
func New(cfg Config) *Deliverer {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	if cfg.Throttle <= 0 {
		cfg.Throttle = defaultThrottle
	}
	return &Deliverer{
		cfg:      cfg,
		log:      cfg.Logger,
		now:      cfg.Now,
		throttle: NewThrottler(cfg.Throttle, throttleIdle),
	}
}

// Notify 处理一条事件对单台设备的投递。由 Hub 每设备起一个 goroutine 调用。
func (d *Deliverer) Notify(ctx context.Context, device store.Device, env wire.Envelope) {
	if d.cfg.Pusher == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if device.APNSToken != "" {
		d.sendAlert(d.tierOf(env.DaemonID), device, env)
	}
	// 没有 alert token 的设备仍可能注册了 Live Activity token。
	d.sendLiveActivity(d.tierOf(env.DaemonID), device, env)
	// 云审批卡(新版 App):push-to-start 起卡 / update / end 收场。
	d.sendCloudActivity(d.tierOf(env.DaemonID), device, env)
}

// tierOf 读取 daemon 档位；记录不存在（已注销级联）按 free——
// 该 daemon 本就不该再推送，free 恰是最严档。
func (d *Deliverer) tierOf(daemonID string) store.Tier {
	daemon, err := d.cfg.Store.Daemon(daemonID)
	if err != nil {
		return store.TierFree
	}
	return daemon.Tier
}

// sendAlert 只对审批、审批关闭与任务完成发通知。
func (d *Deliverer) sendAlert(tier store.Tier, device store.Device, env wire.Envelope) {
	now := d.now().Unix()
	var (
		payload    map[string]any
		expiration int64
		priority   int
		collapseID string
	)
	category := d.cfg.APNSCategory
	if category == "" {
		category = legacyApprovalCategory
	}
	switch env.Type {
	case wire.EventApprovalRequired:
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "Approval Required"
		}
		body := approvalBody(env)
		payload = map[string]any{
			"aps": map[string]any{
				"alert":              map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": body},
				"sound":              "default",
				"interruption-level": "time-sensitive",
				"thread-id":          "approval",
				"category":           category,
			},
			"type":         string(env.Type),
			"event_id":     env.EventID,
			"approval_id":  env.ApprovalID,
			"daemon_id":    env.DaemonID,
			"session_id":   env.SessionID,
			"agent":        string(env.Agent),
			"hostname":     env.Payload.Hostname,
			"tmux_session": env.Payload.TmuxSession,
			"cwd":          env.Payload.Cwd,
			"expires_at":   env.ExpiresAt,
			"ts":           env.TS,
		}
		// APNs 过期对齐等待窗:窗内设备上线仍能收到;旧 daemon 无 expires_at 回落旧值。
		expiration = now + approvalExpiry
		if env.ExpiresAt > now {
			expiration = env.ExpiresAt
		}
		priority = 10
		collapseID = "apv-" + env.ApprovalID
	case wire.EventApprovalClosed:
		// 替换推送:同 collapse-id 让已投递的审批横幅「变脸」为终态。
		// 静默(无声音、无打断级别)——用户已在别处处理,不再打扰。
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "Approval Resolved"
		}
		payload = map[string]any{
			"aps": map[string]any{
				"alert":     map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": closedBody(env)},
				"thread-id": "approval",
				"category":  category,
			},
			"type":        string(env.Type),
			"event_id":    env.EventID,
			"approval_id": env.ApprovalID,
			"daemon_id":   env.DaemonID,
			"session_id":  env.SessionID,
			"agent":       string(env.Agent),
			"ts":          env.TS,
		}
		expiration = now + closeExpiry
		priority = 10
		collapseID = "apv-" + env.ApprovalID
	case wire.EventTaskComplete:
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "Task Complete"
		}
		body := wire.Summarize(env.Payload.ReplySummary, wire.MaxReplyRunes)
		if body == "" {
			body = "Agent completed the task"
		}
		payload = map[string]any{
			"aps": map[string]any{
				"alert":     map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": body},
				"sound":     "default",
				"thread-id": "task",
			},
			"type":       string(env.Type),
			"event_id":   env.EventID,
			"daemon_id":  env.DaemonID,
			"session_id": env.SessionID,
			"agent":      string(env.Agent),
			"ts":         env.TS,
		}
		expiration = now + int64(taskExpiry.Seconds())
		priority = 5
		collapseID = "task-" + env.SessionID
	default:
		return
	}

	res := d.push(tier, approvalPushKind, device.ID, env, apns.Request{
		DeviceToken: device.APNSToken,
		Topic:       d.cfg.Pusher.TopicFor(),
		PushType:    "alert",
		Priority:    priority,
		Expiration:  expiration,
		CollapseID:  collapseID,
		Payload:     payload,
	})
	if res.Prune {
		// device token 已失效：清空，等待 App 下次启动重新上报。
		if err := d.cfg.Store.ClearDeviceAPNSToken(device.ID); err != nil {
			d.log.Warn("failed to clear stale device token", "device_id", device.ID, "err", err)
		}
	}
}

// approvalBody 生成审批通知正文，只使用摘要白名单字段。
func approvalBody(env wire.Envelope) string {
	tool := env.Payload.ToolName
	if tool == "" {
		tool = "a tool"
	}
	agent := string(env.Agent)
	if agent == "" {
		agent = "Agent"
	}
	body := fmt.Sprintf("%s requests to run %s", agent, tool)
	if summary := env.Payload.CommandSummary; summary != "" {
		body += ": " + summary
	}
	return wire.Truncate(body, 200)
}

// closedBody 生成审批关闭替换推送的正文,按 close reason 映射终态文案。
func closedBody(env wire.Envelope) string {
	switch env.Payload.StatusText {
	case wire.CloseReasonApproved:
		return "Approved"
	case wire.CloseReasonDenied:
		return "Denied"
	case wire.CloseReasonExpired:
		return "Missed approval — open the session to continue"
	case wire.CloseReasonCancelled:
		return "Answered at terminal"
	default:
		return "Resolved"
	}
}

// sendLiveActivity 把事件转发到该设备当前会话的 LA token。
func (d *Deliverer) sendLiveActivity(tier store.Tier, device store.Device, env wire.Envelope) {
	tokens, err := d.cfg.Store.LATokensForDaemon(env.DaemonID)
	if err != nil {
		d.log.Warn("failed to list Live Activity tokens", "daemon_id", env.DaemonID, "err", err)
		return
	}
	if len(tokens) == 0 {
		return
	}
	now := d.now().Unix()
	push := wire.LiveActivityPushFor(env, now)
	priority := 5
	if env.Type == wire.EventApprovalRequired {
		priority = 10
	}
	for _, tok := range tokens {
		if tok.DeviceID != device.ID || tok.SessionID != env.SessionID || tok.PushToken == "" {
			continue
		}
		token, session := tok.PushToken, tok.SessionID
		key := device.ID + "/" + session
		d.throttle.Do(key, func() {
			res := d.push(tier, laPushKind, device.ID, env, apns.Request{
				DeviceToken: token,
				Topic:       d.cfg.Pusher.LiveActivityTopic(),
				PushType:    "liveactivity",
				Priority:    priority,
				Expiration:  now + liveActivityTTL,
				Payload:     push,
			})
			if res.Prune {
				// token 已失效：剪枝，后续事件不再尝试。
				if err := d.cfg.Store.DeleteLAToken(device.ID, session); err != nil {
					d.log.Warn("failed to delete stale LA token", "device_id", device.ID, "session_id", session, "err", err)
				}
			}
		})
	}
}

// push 发送一条 APNs 请求并记录指标。
func (d *Deliverer) push(tier store.Tier, kind, deviceID string, env wire.Envelope, req apns.Request) apns.Result {
	// 日配额闸在 APNs 发送之前：这是 APNs 成本唯一漏斗。被拒只计数，
	// 不触碰 token；实时 WSS/inbox/resync 不受影响，App 前台仍可审批（fail-safe）。
	// 审批范围推送默认豁免:LA 更新+替换推送会把免费档 10 条/日烧穿,审批不能被配额哑掉。
	if d.cfg.Quota != nil &&
		!(d.cfg.QuotaExemptApprovalPushes && approvalScoped(env)) &&
		!d.cfg.Quota.AllowPush(tier, env.DaemonID) {
		d.cfg.Metrics.IncQuotaRejected("push_" + kind)
		d.log.Info("push blocked by daily quota", "kind", kind, "daemon_id", env.DaemonID,
			"device_id", deviceID, "event_id", env.EventID)
		return apns.Result{}
	}
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	res, err := d.cfg.Pusher.Send(ctx, req)
	status := strconv.Itoa(res.Status)
	switch {
	case err != nil:
		status = "error"
		d.log.Warn("APNs push failed", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "err", err)
	case res.Skipped:
		status = "skipped"
		d.log.Debug("APNs push skipped (topic backoff)", "kind", kind, "device_id", deviceID, "event_id", env.EventID)
	case !res.Accepted():
		level := slog.LevelWarn
		if res.Status == 400 || res.Status == 403 {
			level = slog.LevelError
		}
		d.log.Log(context.Background(), level, "APNs rejected push", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "prune", res.Prune)
	}
	d.cfg.Metrics.IncPush(kind, status)
	return res
}

// sendCloudActivity 驱动云审批 LA(CloudApprovalAttributes,新版 App):
//   - approval_required:优先会话级 token(App 前台起卡时上报)发 update;
//     没有则用设备级 push-to-start token 发 start——App 被杀也能起卡(iOS 17.2+)。
//   - approval_closed / task_complete / session_ended:会话级 token 发 end 收场。
//   - 其余遥测事件不驱动云审批卡(回到 working 形态由 update 事件承担,这里不扩)。
//   - 强退的 App:push-to-start 被 iOS 拒绝,自然回落通知按钮层(App 侧契约)。
func (d *Deliverer) sendCloudActivity(tier store.Tier, device store.Device, env wire.Envelope) {
	isEnd := env.Type == wire.EventApprovalClosed ||
		env.Type == wire.EventTaskComplete || env.Type == wire.EventSessionEnded
	if env.Type != wire.EventApprovalRequired && !isEnd {
		return
	}
	tokens, err := d.cfg.Store.LATokensForDaemon(env.DaemonID)
	if err != nil {
		d.log.Warn("failed to list Live Activity tokens", "daemon_id", env.DaemonID, "err", err)
		return
	}
	var sessionToken string
	for _, tok := range tokens {
		if tok.DeviceID == device.ID && tok.SessionID == env.SessionID && tok.PushToken != "" {
			sessionToken = tok.PushToken
			break
		}
	}
	now := d.now().Unix()
	priority := 5
	if env.Type == wire.EventApprovalRequired {
		priority = 10
	}

	// 路径一:会话级 token(App 前台起的卡)——update/end。
	if sessionToken != "" {
		token, session := sessionToken, env.SessionID
		key := "cloud/" + device.ID + "/" + session
		push := wire.CloudApprovalPushFor(env, now, false)
		d.throttle.Do(key, func() {
			res := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
				DeviceToken: token,
				Topic:       d.cfg.Pusher.LiveActivityTopic(),
				PushType:    "liveactivity",
				Priority:    priority,
				Expiration:  now + liveActivityTTL,
				Payload:     push,
			})
			if res.Prune && isEnd {
				// 终局推送才发现 token 失效:顺手剪枝。
				if err := d.cfg.Store.DeleteLAToken(device.ID, session); err != nil {
					d.log.Warn("failed to delete stale LA token", "device_id", device.ID, "session_id", session, "err", err)
				}
			}
		})
		return
	}

	// 路径二:设备级 push-to-start token(App 被杀时由 relay 起卡)。
	// 只对 approval_required 起卡;终局事件无卡可收(410 剪枝 token)。
	if env.Type == wire.EventApprovalRequired && device.PushToStartToken != "" {
		push := wire.CloudApprovalPushFor(env, now, true)
		res := d.push(tier, cloudLAPushKind, device.ID, env, apns.Request{
			DeviceToken: device.PushToStartToken,
			Topic:       d.cfg.Pusher.LiveActivityTopic(),
			PushType:    "liveactivity",
			Priority:    priority,
			Expiration:  now + liveActivityTTL,
			Payload:     push,
		})
		if res.Prune {
			if err := d.cfg.Store.SetPushToStartToken(device.ID, "", now); err != nil {
				d.log.Warn("failed to clear stale push-to-start token", "device_id", device.ID, "err", err)
			}
		}
	}
}

// approvalScoped 判定审批范围事件(alert 替换 + LA 驱动都豁免日配额的集合)。
func approvalScoped(env wire.Envelope) bool {
	return env.Type == wire.EventApprovalRequired || env.Type == wire.EventApprovalClosed
}

// ---------- 节流 ----------

// Throttler 对同一 key 的投递做 latest-wins 节流。
//
// 第一次调用立即执行；随后每个 interval 至多执行一次，期间到达的
// 多个动作只保留最后一个。空闲 idle 后协程自行退出，避免泄漏。
type Throttler struct {
	interval time.Duration
	idle     time.Duration
	mu       sync.Mutex
	states   map[string]*throttleState
}

type throttleState struct {
	ch chan func()
}

// NewThrottler 创建节流器。
func NewThrottler(interval, idle time.Duration) *Throttler {
	return &Throttler{interval: interval, idle: idle, states: make(map[string]*throttleState)}
}

// Do 提交一个动作；同 key 的待执行动作会被最新调用替换。
func (t *Throttler) Do(key string, fn func()) {
	t.mu.Lock()
	st := t.states[key]
	if st == nil {
		st = &throttleState{ch: make(chan func(), 1)}
		t.states[key] = st
		go t.loop(key, st)
	}
	t.mu.Unlock()

	// 非阻塞写入；槽位已满时丢弃旧动作，保证 latest-wins。
	select {
	case st.ch <- fn:
		return
	default:
	}
	select {
	case <-st.ch:
	default:
	}
	select {
	case st.ch <- fn:
	default:
	}
}

func (t *Throttler) loop(key string, st *throttleState) {
	idle := time.NewTimer(t.idle)
	defer idle.Stop()
	for {
		select {
		case fn := <-st.ch:
			fn()
			// 节流间隔内到达的新动作会覆盖缓冲槽位，下一轮只发最新。
			select {
			case <-time.After(t.interval):
			case <-idle.C:
				t.forget(key, st)
				return
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(t.idle)
		case <-idle.C:
			t.forget(key, st)
			return
		}
	}
}

func (t *Throttler) forget(key string, st *throttleState) {
	t.mu.Lock()
	if t.states[key] == st {
		delete(t.states, key)
	}
	t.mu.Unlock()
}
