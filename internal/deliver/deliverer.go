// Package deliver 实现 relay 的 APNs 投递语义。
//
// 规则（与设计文档 §4.5 一致）：
//   - approval_required：始终发 alert（priority 10、time-sensitive、120s 过期），
//     同时驱动 Live Activity；
//   - task_complete：始终发 alert（priority 5、24h 过期）与 Live Activity end；
//   - tool_* / session_started / session_ended：不单独发 alert；
//   - Live Activity 按 (device, session) 1 秒节流、latest-wins；
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
	approvalPushKind = "alert"
	laPushKind       = "liveactivity"
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
	Now      func() time.Time
	Throttle time.Duration
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
		d.sendAlert(device, env)
	}
	// 没有 alert token 的设备仍可能注册了 Live Activity token。
	d.sendLiveActivity(device, env)
}

// sendAlert 只对审批与任务完成发通知。
func (d *Deliverer) sendAlert(device store.Device, env wire.Envelope) {
	now := d.now().Unix()
	var (
		payload    map[string]any
		expiration int64
		priority   int
		collapseID string
	)
	switch env.Type {
	case wire.EventApprovalRequired:
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "需要审批"
		}
		body := approvalBody(env)
		payload = map[string]any{
			"aps": map[string]any{
				"alert":              map[string]string{"title": wire.Truncate(title, wire.MaxTitleRunes), "body": body},
				"sound":              "default",
				"interruption-level": "time-sensitive",
				"thread-id":          "approval",
				"category":           "DEVPLOT_APPROVAL",
			},
			"type":        string(env.Type),
			"event_id":    env.EventID,
			"approval_id": env.ApprovalID,
			"daemon_id":   env.DaemonID,
			"session_id":  env.SessionID,
			"agent":       string(env.Agent),
			"ts":          env.TS,
		}
		expiration = now + approvalExpiry
		priority = 10
		collapseID = "apv-" + env.ApprovalID
	case wire.EventTaskComplete:
		title := env.Payload.SessionTitle
		if title == "" {
			title = env.Payload.Title
		}
		if title == "" {
			title = "任务完成"
		}
		body := wire.Summarize(env.Payload.ReplySummary, wire.MaxReplyRunes)
		if body == "" {
			body = "Agent 已完成任务"
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

	res := d.push(approvalPushKind, device.ID, env, apns.Request{
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
			d.log.Warn("清空失效 device token 失败", "device_id", device.ID, "err", err)
		}
	}
}

// approvalBody 生成审批通知正文，只使用摘要白名单字段。
func approvalBody(env wire.Envelope) string {
	tool := env.Payload.ToolName
	if tool == "" {
		tool = "工具调用"
	}
	agent := string(env.Agent)
	if agent == "" {
		agent = "Agent"
	}
	body := fmt.Sprintf("%s 请求执行 %s", agent, tool)
	if summary := env.Payload.CommandSummary; summary != "" {
		body += "：" + summary
	}
	return wire.Truncate(body, 200)
}

// sendLiveActivity 把事件转发到该设备当前会话的 LA token。
func (d *Deliverer) sendLiveActivity(device store.Device, env wire.Envelope) {
	tokens, err := d.cfg.Store.LATokensForDaemon(env.DaemonID)
	if err != nil {
		d.log.Warn("读取 Live Activity token 失败", "daemon_id", env.DaemonID, "err", err)
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
			res := d.push(laPushKind, device.ID, env, apns.Request{
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
					d.log.Warn("删除失效 LA token 失败", "device_id", device.ID, "session_id", session, "err", err)
				}
			}
		})
	}
}

// push 发送一条 APNs 请求并记录指标。
func (d *Deliverer) push(kind, deviceID string, env wire.Envelope, req apns.Request) apns.Result {
	ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
	defer cancel()
	res, err := d.cfg.Pusher.Send(ctx, req)
	status := strconv.Itoa(res.Status)
	switch {
	case err != nil:
		status = "error"
		d.log.Warn("APNs 推送失败", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "err", err)
	case res.Skipped:
		status = "skipped"
		d.log.Debug("APNs 推送跳过（topic 退避）", "kind", kind, "device_id", deviceID, "event_id", env.EventID)
	case !res.Accepted():
		level := slog.LevelWarn
		if res.Status == 400 || res.Status == 403 {
			level = slog.LevelError
		}
		d.log.Log(context.Background(), level, "APNs 拒绝推送", "kind", kind, "device_id", deviceID,
			"event_id", env.EventID, "status", res.Status, "reason", res.Reason, "prune", res.Prune)
	}
	d.cfg.Metrics.IncPush(kind, status)
	return res
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
