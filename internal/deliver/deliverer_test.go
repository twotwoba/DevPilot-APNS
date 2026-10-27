package deliver

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/apns"
	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/internal/quota"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

const (
	testDaemon = "dd_0123456789abcdef0123456789abcdef"
	testCode   = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	testNow    = int64(1_760_000_000)
	alertTopic = "com.twotwoba.devpilot"
	laTopic    = "com.twotwoba.devpilot.push-type.liveactivity"
)

type fakePusher struct {
	mu      sync.Mutex
	calls   []apns.Request
	respond func(apns.Request) (apns.Result, error)
}

func (f *fakePusher) Send(_ context.Context, req apns.Request) (apns.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, req)
	respond := f.respond
	f.mu.Unlock()
	if respond != nil {
		return respond(req)
	}
	return apns.Result{Status: 200}, nil
}

func (f *fakePusher) TopicFor() string          { return alertTopic }
func (f *fakePusher) LiveActivityTopic() string { return laTopic }
func (f *fakePusher) snapshot() []apns.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]apns.Request, len(f.calls))
	copy(out, f.calls)
	return out
}

// setupStore 创建 daemon + device，并返回设备记录。
func setupStore(t *testing.T, apnsToken string) (*store.Store, store.Device) {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.RegisterPairingSession(store.PairingSession{
		CodeHash: pairing.CodeHash(testCode), DaemonID: testDaemon, DaemonName: "mac",
		CredHash:  pairing.SecretHash("0123456789abcdef0123456789abcdef"),
		ExpiresAt: testNow + 900, CreatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	dev := store.Device{
		ID: "dv_0123456789abcdef", CredHash: pairing.SecretHash(strings.Repeat("s", 32)),
		APNSToken: apnsToken, Name: "iPhone",
	}
	if _, _, err := s.Pair(pairing.CodeHash(testCode), dev, store.TierFree, testNow); err != nil {
		t.Fatal(err)
	}
	got, err := s.Device(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	return s, got
}

func newDeliverer(t *testing.T, s *store.Store, p Pusher, clock *testClock, throttle time.Duration) (*Deliverer, *metrics.Metrics) {
	return newDelivererWithQuota(t, s, p, clock, throttle, nil)
}

func newDelivererWithQuota(t *testing.T, s *store.Store, p Pusher, clock *testClock, throttle time.Duration, eng *quota.Engine) (*Deliverer, *metrics.Metrics) {
	t.Helper()
	m := metrics.New()
	d := New(Config{
		Store: s, Pusher: p, Metrics: m, Quota: eng,
		Now: clock.Now, Throttle: throttle,
	})
	return d, m
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func event(typ wire.EventType, approvalID string) wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_1", EventID: "ev_1",
		ApprovalID: approvalID, Type: typ, Agent: wire.AgentClaude,
		DaemonID: testDaemon, SessionID: "sess_1", TS: testNow,
		Payload: wire.Payload{
			SessionTitle: "重构认证模块", ToolName: "Bash",
			CommandSummary: "rm -rf build", ReplySummary: "已完成重构",
		},
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待超时: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestApprovalAlert(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	d, m := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	req := calls[0]
	if req.DeviceToken != "apns-token" || req.Topic != alertTopic {
		t.Fatalf("req = %+v", req)
	}
	if req.PushType != "alert" || req.Priority != 10 || req.Expiration != testNow+120 {
		t.Fatalf("推送参数不符: %+v", req)
	}
	if req.CollapseID != "apv-apv_1" {
		t.Fatalf("collapse = %q", req.CollapseID)
	}
	payload, ok := req.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload 类型 = %T", req.Payload)
	}
	aps := payload["aps"].(map[string]any)
	if aps["interruption-level"] != "time-sensitive" || aps["thread-id"] != "approval" {
		t.Fatalf("aps = %+v", aps)
	}
	alert := aps["alert"].(map[string]string)
	if alert["title"] != "重构认证模块" || !strings.Contains(alert["body"], "Bash") {
		t.Fatalf("alert = %+v", alert)
	}
	if payload["approval_id"] != "apv_1" || payload["daemon_id"] != testDaemon {
		t.Fatalf("自定义字段 = %+v", payload)
	}
	if !strings.Contains(m.Render(), `devpilot_apns_pushes_total{kind="alert",status="200"} 1`) {
		t.Fatalf("metrics 缺少推送计数:\n%s", m.Render())
	}
}

func TestTaskCompleteAlertPriority(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	d.Notify(context.Background(), dev, event(wire.EventTaskComplete, ""))
	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if calls[0].Priority != 5 || calls[0].Expiration != testNow+86400 {
		t.Fatalf("参数 = %+v", calls[0])
	}
	if calls[0].CollapseID != "task-sess_1" {
		t.Fatalf("collapse = %q", calls[0].CollapseID)
	}
	payload := calls[0].Payload.(map[string]any)
	alert := payload["aps"].(map[string]any)["alert"].(map[string]string)
	if alert["body"] != "已完成重构" {
		t.Fatalf("body = %q", alert["body"])
	}
}

func TestToolEventSendsNoAlert(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	time.Sleep(50 * time.Millisecond)
	if len(f.snapshot()) != 0 {
		t.Fatal("tool_running 不应发 alert")
	}
}

func TestLiveActivityThrottleLatestWins(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	if err := s.PutLAToken(store.LAToken{
		DeviceID: dev.ID, DaemonID: testDaemon, SessionID: "sess_1",
		PushToken: "la-token", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{}
	clock := &testClock{t: time.Unix(testNow, 0)}
	d, _ := newDeliverer(t, s, f, clock, 100*time.Millisecond)

	d.Notify(context.Background(), dev, event(wire.EventSessionStarted, ""))
	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	d.Notify(context.Background(), dev, event(wire.EventSessionEnded, ""))

	waitFor(t, "至少一次 Live Activity 推送", func() bool { return len(f.snapshot()) >= 1 })
	time.Sleep(300 * time.Millisecond)

	calls := f.snapshot()
	if len(calls) > 2 {
		t.Fatalf("1s 内推送次数过多: %d", len(calls))
	}
	last := calls[len(calls)-1]
	if last.PushType != "liveactivity" || last.Topic != laTopic || last.DeviceToken != "la-token" {
		t.Fatalf("last = %+v", last)
	}
	push, ok := last.Payload.(wire.LiveActivityPush)
	if !ok {
		t.Fatalf("payload 类型 = %T", last.Payload)
	}
	if push.APS.Event != "end" || push.APS.ContentState.StatusText != "已结束" {
		t.Fatalf("最新事件被旧事件覆盖: %+v", push.APS)
	}
	if push.APS.ContentState.UpdatedAt != testNow || push.APS.Timestamp != testNow {
		t.Fatalf("时间戳应为 relay 发送时刻: %+v", push.APS)
	}
}

func TestLiveActivityPruneOn410(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	if err := s.PutLAToken(store.LAToken{
		DeviceID: dev.ID, DaemonID: testDaemon, SessionID: "sess_1",
		PushToken: "dead-token", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{respond: func(req apns.Request) (apns.Result, error) {
		if req.PushType == "liveactivity" {
			return apns.Result{Status: 410, Reason: "Unregistered", Prune: true}, nil
		}
		return apns.Result{Status: 200}, nil
	}}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, 10*time.Millisecond)

	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	waitFor(t, "LA token 被剪枝", func() bool {
		tokens, err := s.LATokensForDaemon(testDaemon)
		return err == nil && len(tokens) == 0
	})
}

func TestAlertPruneClearsDeviceToken(t *testing.T) {
	s, dev := setupStore(t, "dead-token")
	f := &fakePusher{respond: func(apns.Request) (apns.Result, error) {
		return apns.Result{Status: 410, Reason: "Unregistered", Prune: true}, nil
	}}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	got, err := s.Device(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.APNSToken != "" {
		t.Fatalf("失效 token 未清空: %q", got.APNSToken)
	}
}

func TestDeviceWithoutAlertTokenStillGetsLiveActivity(t *testing.T) {
	s, dev := setupStore(t, "")
	if err := s.PutLAToken(store.LAToken{
		DeviceID: dev.ID, DaemonID: testDaemon, SessionID: "sess_1",
		PushToken: "la-token", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, 10*time.Millisecond)

	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	waitFor(t, "仅 LA 推送", func() bool { return len(f.snapshot()) == 1 })
	req := f.snapshot()[0]
	if req.PushType != "liveactivity" || req.DeviceToken != "la-token" {
		t.Fatalf("req = %+v", req)
	}
}

// TestDailyQuotaBlocksAlert 日配额打满后 alert 不再发,APNs 零调用。
func TestDailyQuotaBlocksAlert(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	clock := &testClock{t: time.Unix(testNow, 0)}
	eng := quota.New(quota.Config{QuotaDailyFree: 1, Now: clock.Now})
	d, m := newDelivererWithQuota(t, s, f, clock, time.Second, eng)

	// alert 路径同步执行:第一条发出,第二条被日配额拦截。
	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_2"))
	if calls := f.snapshot(); len(calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(calls))
	}
	if !strings.Contains(m.Render(), `devpilot_quota_rejected_total{kind="push_alert"} 1`) {
		t.Fatalf("metrics 缺少配额拒绝计数:\n%s", m.Render())
	}
	// 被拒推送不触碰 token(无 prune 副作用)。
	got, err := s.Device(dev.ID)
	if err != nil || got.APNSToken != "apns-token" {
		t.Fatalf("token 不应被改动: %+v %v", got, err)
	}
}

// TestDailyQuotaBlocksLiveActivity 日配额同样约束 LA 推送(最频繁的成本来源)。
func TestDailyQuotaBlocksLiveActivity(t *testing.T) {
	s, dev := setupStore(t, "")
	if err := s.PutLAToken(store.LAToken{
		DeviceID: dev.ID, DaemonID: testDaemon, SessionID: "sess_1",
		PushToken: "la-token", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{}
	clock := &testClock{t: time.Unix(testNow, 0)}
	eng := quota.New(quota.Config{QuotaDailyFree: 1, Now: clock.Now})
	d, m := newDelivererWithQuota(t, s, f, clock, 10*time.Millisecond, eng)

	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	waitFor(t, "第一次 LA 推送", func() bool { return len(f.snapshot()) == 1 })
	d.Notify(context.Background(), dev, event(wire.EventToolFinished, ""))
	time.Sleep(100 * time.Millisecond)
	if calls := f.snapshot(); len(calls) != 1 {
		t.Fatalf("日配额打满后 LA 推送应被拦截: %d", len(calls))
	}
	if !strings.Contains(m.Render(), `devpilot_quota_rejected_total{kind="push_liveactivity"} 1`) {
		t.Fatalf("metrics 缺少 LA 配额拒绝计数:\n%s", m.Render())
	}
}

func TestThrottlerLatestWins(t *testing.T) {
	th := NewThrottler(20*time.Millisecond, time.Second)
	var mu sync.Mutex
	var ran []int
	for i := range 3 {
		idx := i
		th.Do("k", func() {
			mu.Lock()
			ran = append(ran, idx)
			mu.Unlock()
		})
	}
	waitFor(t, "执行至少一次", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(ran) > 2 {
		t.Fatalf("执行次数 = %v", ran)
	}
	if ran[len(ran)-1] != 2 {
		t.Fatalf("最新动作未胜出: %v", ran)
	}
}
