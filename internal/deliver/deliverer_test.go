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
			SessionTitle: "Refactor auth module", ToolName: "Bash",
			CommandSummary: "rm -rf build", ReplySummary: "Refactor done",
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
			t.Fatalf("timed out waiting: %s", what)
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
		t.Fatalf("push params mismatch: %+v", req)
	}
	if req.CollapseID != "apv-apv_1" {
		t.Fatalf("collapse = %q", req.CollapseID)
	}
	payload, ok := req.Payload.(map[string]any)
	if !ok {
		t.Fatalf("payload type = %T", req.Payload)
	}
	aps := payload["aps"].(map[string]any)
	if aps["interruption-level"] != "time-sensitive" || aps["thread-id"] != "approval" {
		t.Fatalf("aps = %+v", aps)
	}
	alert := aps["alert"].(map[string]string)
	if alert["title"] != "Refactor auth module" || !strings.Contains(alert["body"], "Bash") {
		t.Fatalf("alert = %+v", alert)
	}
	if payload["approval_id"] != "apv_1" || payload["daemon_id"] != testDaemon {
		t.Fatalf("custom fields = %+v", payload)
	}
	if !strings.Contains(m.Render(), `devpilot_apns_pushes_total{kind="alert",status="200"} 1`) {
		t.Fatalf("metrics missing push counter:\n%s", m.Render())
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
		t.Fatalf("params = %+v", calls[0])
	}
	if calls[0].CollapseID != "task-sess_1" {
		t.Fatalf("collapse = %q", calls[0].CollapseID)
	}
	payload := calls[0].Payload.(map[string]any)
	alert := payload["aps"].(map[string]any)["alert"].(map[string]string)
	if alert["body"] != "Refactor done" {
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
		t.Fatal("tool_running should not send alert")
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

	waitFor(t, "at least one Live Activity push", func() bool { return len(f.snapshot()) >= 1 })
	time.Sleep(300 * time.Millisecond)

	calls := f.snapshot()
	// 只统计会话进度卡(legacy LA)的推送:云审批卡走独立节流键,session_ended
	// 会另发一条 end,不属本测试的节流断言范围。
	var legacy []apns.Request
	for _, c := range calls {
		if _, ok := c.Payload.(wire.LiveActivityPush); ok {
			legacy = append(legacy, c)
		}
	}
	if len(legacy) > 2 {
		t.Fatalf("too many pushes within 1s: %d", len(legacy))
	}
	last := legacy[len(legacy)-1]
	if last.PushType != "liveactivity" || last.Topic != laTopic || last.DeviceToken != "la-token" {
		t.Fatalf("last = %+v", last)
	}
	push, ok := last.Payload.(wire.LiveActivityPush)
	if !ok {
		t.Fatalf("payload type = %T", last.Payload)
	}
	if push.APS.Event != "end" || push.APS.ContentState.StatusText == "" {
		t.Fatalf("latest event overwritten by stale event: %+v", push.APS)
	}
	if push.APS.ContentState.UpdatedAt != testNow || push.APS.Timestamp != testNow {
		t.Fatalf("timestamps should be relay send time: %+v", push.APS)
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
	waitFor(t, "LA token pruned", func() bool {
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
		t.Fatalf("stale token not cleared: %q", got.APNSToken)
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
	waitFor(t, "only LA push", func() bool { return len(f.snapshot()) == 1 })
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
		t.Fatalf("metrics missing quota rejection counter:\n%s", m.Render())
	}
	// 被拒推送不触碰 token(无 prune 副作用)。
	got, err := s.Device(dev.ID)
	if err != nil || got.APNSToken != "apns-token" {
		t.Fatalf("token should not be modified: %+v %v", got, err)
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
	waitFor(t, "first LA push", func() bool { return len(f.snapshot()) == 1 })
	d.Notify(context.Background(), dev, event(wire.EventToolFinished, ""))
	time.Sleep(100 * time.Millisecond)
	if calls := f.snapshot(); len(calls) != 1 {
		t.Fatalf("LA push should be blocked after daily quota is exhausted: %d", len(calls))
	}
	if !strings.Contains(m.Render(), `devpilot_quota_rejected_total{kind="push_liveactivity"} 1`) {
		t.Fatalf("metrics missing LA quota rejection counter:\n%s", m.Render())
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
	waitFor(t, "executed at least once", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(ran) >= 1
	})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if len(ran) > 2 {
		t.Fatalf("execution count = %v", ran)
	}
	if ran[len(ran)-1] != 2 {
		t.Fatalf("latest action did not win: %v", ran)
	}
}

// TestApprovalClosedReplacementAlert close 替换推送:同 collapse-id 让已投递
// 横幅「变脸」,无声、无打断级别、60s 短过期,正文按 close reason 映射。
func TestApprovalClosedReplacementAlert(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	cases := []struct {
		reason string
		body   string
	}{
		{wire.CloseReasonApproved, "Approved"},
		{wire.CloseReasonDenied, "Denied"},
		{wire.CloseReasonExpired, "Missed approval — open the session to continue"},
		{wire.CloseReasonCancelled, "Answered at terminal"},
	}
	for i, c := range cases {
		env := event(wire.EventApprovalClosed, "apv_1")
		env.Payload.StatusText = c.reason
		d.Notify(context.Background(), dev, env)
		calls := f.snapshot()
		if len(calls) != i+1 {
			t.Fatalf("calls = %d after %s", len(calls), c.reason)
		}
		req := calls[i]
		if req.PushType != "alert" || req.Priority != 10 || req.Expiration != testNow+closeExpiry {
			t.Fatalf("params = %+v", req)
		}
		if req.CollapseID != "apv-apv_1" {
			t.Fatalf("collapse = %q (must replace the original approval banner)", req.CollapseID)
		}
		aps := req.Payload.(map[string]any)["aps"].(map[string]any)
		if _, ok := aps["sound"]; ok {
			t.Fatal("replacement push must be silent")
		}
		if _, ok := aps["interruption-level"]; ok {
			t.Fatal("replacement push must not interrupt")
		}
		if aps["thread-id"] != "approval" {
			t.Fatalf("aps = %+v", aps)
		}
		if got := aps["alert"].(map[string]string)["body"]; got != c.body {
			t.Fatalf("body = %q, want %q", got, c.body)
		}
	}
}

// TestApprovalAlertEnrichment expires_at/主机富集键透传,APNs 过期对齐等待窗;
// category 配置化:配置后用新串,未配置回落旧串(存量 App 分期切换)。
func TestApprovalAlertEnrichment(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	clock := &testClock{t: time.Unix(testNow, 0)}
	d := New(Config{
		Store: s, Pusher: f, Metrics: metrics.New(), Now: clock.Now, Throttle: time.Second,
		APNSCategory: "DEVPILOT_APPROVAL",
	})

	env := event(wire.EventApprovalRequired, "apv_1")
	env.ExpiresAt = testNow + 1800
	env.Payload.Hostname = "Erics-MacBook.local"
	env.Payload.TmuxSession = "main"
	env.Payload.Cwd = "/Users/eric/Desktop/DevPilot"
	d.Notify(context.Background(), dev, env)

	calls := f.snapshot()
	if len(calls) != 1 {
		t.Fatalf("calls = %d", len(calls))
	}
	if req := calls[0]; req.Expiration != testNow+1800 {
		t.Fatalf("expiration = %d, want wait-window end", req.Expiration)
	}
	payload := calls[0].Payload.(map[string]any)
	if payload["hostname"] != "Erics-MacBook.local" ||
		payload["tmux_session"] != "main" ||
		payload["cwd"] != "/Users/eric/Desktop/DevPilot" ||
		payload["expires_at"] != int64(testNow+1800) {
		t.Fatalf("enrichment keys = %+v", payload)
	}
	if aps := payload["aps"].(map[string]any); aps["category"] != "DEVPILOT_APPROVAL" {
		t.Fatalf("category = %v", aps["category"])
	}

	// 未配置 category → 回落旧串(存量 App 只注册旧 category)。
	f2 := &fakePusher{}
	d2 := New(Config{Store: s, Pusher: f2, Metrics: metrics.New(), Now: clock.Now, Throttle: time.Second})
	d2.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_2"))
	aps2 := f2.snapshot()[0].Payload.(map[string]any)["aps"].(map[string]any)
	if aps2["category"] != legacyApprovalCategory {
		t.Fatalf("fallback category = %v", aps2["category"])
	}
}

// TestApprovalPushesExemptFromDailyQuota 审批范围推送豁免日配额(默认开):
// 免费档 1 条/日也能连发审批 alert 与 close 替换,审批不因配额哑火。
func TestApprovalPushesExemptFromDailyQuota(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	f := &fakePusher{}
	clock := &testClock{t: time.Unix(testNow, 0)}
	eng := quota.New(quota.Config{QuotaDailyFree: 1, Now: clock.Now})
	m := metrics.New()
	d := New(Config{Store: s, Pusher: f, Metrics: m, Quota: eng, Now: clock.Now,
		Throttle: time.Second, QuotaExemptApprovalPushes: true})

	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_2"))
	closeEnv := event(wire.EventApprovalClosed, "apv_1")
	closeEnv.Payload.StatusText = wire.CloseReasonApproved
	d.Notify(context.Background(), dev, closeEnv)
	if calls := f.snapshot(); len(calls) != 3 {
		t.Fatalf("approval-scope pushes should bypass daily quota: %d", len(calls))
	}
	// HELP 头对零值指标也会出现,须匹配样本行。
	if strings.Contains(m.Render(), "devpilot_quota_rejected_total{") {
		t.Fatalf("unexpected quota rejection:\n%s", m.Render())
	}
}

// TestTelemetryLAStillQuotaCounted 豁免只覆盖审批范围:遥测 LA 照常计数扣配额。
func TestTelemetryLAStillQuotaCounted(t *testing.T) {
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
	m := metrics.New()
	d := New(Config{Store: s, Pusher: f, Metrics: m, Quota: eng, Now: clock.Now,
		Throttle: 10 * time.Millisecond, QuotaExemptApprovalPushes: true})

	d.Notify(context.Background(), dev, event(wire.EventToolRunning, ""))
	waitFor(t, "first LA push", func() bool { return len(f.snapshot()) == 1 })
	d.Notify(context.Background(), dev, event(wire.EventToolFinished, ""))
	time.Sleep(100 * time.Millisecond)
	if calls := f.snapshot(); len(calls) != 1 {
		t.Fatalf("telemetry LA should still consume quota: %d", len(calls))
	}
	if !strings.Contains(m.Render(), `devpilot_quota_rejected_total{kind="push_liveactivity"} 1`) {
		t.Fatalf("metrics missing LA quota rejection:\n%s", m.Render())
	}
}

// TestCloudApprovalPushToStart 设备级 push-to-start:无会话 token 时
// approval_required 由 relay 发 start 起卡(attributes-type 必带、stale 对齐
// 等待窗);close 无卡可收,只发通知替换,不碰设备级 token。
func TestCloudApprovalPushToStart(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	if err := s.SetPushToStartToken(dev.ID, "pts-token", testNow); err != nil {
		t.Fatal(err)
	}
	// Notify 拿到的是事件时刻的设备快照(生产路径 notify() 现查 store),重取。
	dev, err := s.Device(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	env := event(wire.EventApprovalRequired, "apv_1")
	env.ExpiresAt = testNow + 1800
	env.Payload.Hostname = "Erics-MacBook.local"
	d.Notify(context.Background(), dev, env)

	calls := f.snapshot()
	var cloud apns.Request
	sawLA := false
	for _, c := range calls {
		if c.PushType == "liveactivity" {
			sawLA = true
			cloud = c
		}
	}
	if !sawLA {
		t.Fatalf("push-to-start LA push missing: %+v", calls)
	}
	if cloud.DeviceToken != "pts-token" || cloud.Topic != laTopic || cloud.Priority != 10 {
		t.Fatalf("cloud push params = %+v", cloud)
	}
	push, ok := cloud.Payload.(wire.CloudApprovalPush)
	if !ok {
		t.Fatalf("payload type = %T", cloud.Payload)
	}
	if push.APS.Event != "start" || push.APS.AttributesType != wire.CloudApprovalAttributesType {
		t.Fatalf("aps = %+v", push.APS)
	}
	if push.APS.Attributes["sessionId"] != "sess_1" || push.APS.Attributes["daemonId"] != testDaemon {
		t.Fatalf("attributes = %+v", push.APS.Attributes)
	}
	st := push.APS.ContentState
	if st.State != wire.CloudStateNeedsYou || st.ExpiresAt != testNow+1800 ||
		st.HostName != "Erics-MacBook.local" || st.ApprovalID != "apv_1" {
		t.Fatalf("content-state = %+v", st)
	}
	if push.APS.StaleDate != testNow+1800+60 {
		t.Fatalf("stale-date = %d", push.APS.StaleDate)
	}

	// close:只新增替换 alert,不再向设备级 token 发 LA。
	before := len(f.snapshot())
	closeEnv := event(wire.EventApprovalClosed, "apv_1")
	closeEnv.Payload.StatusText = wire.CloseReasonApproved
	d.Notify(context.Background(), dev, closeEnv)
	if calls = f.snapshot(); len(calls) != before+1 {
		t.Fatalf("close should only add the replacement alert: %d -> %d", before, len(calls))
	}
	if added := calls[len(calls)-1]; added.PushType != "alert" {
		t.Fatalf("added push = %+v", added)
	}
}

// TestPushToStartPruneOn410 push-to-start token 失效(APNs 410)时立即剪枝,
// 等待 App 下次启动重报。
func TestPushToStartPruneOn410(t *testing.T) {
	s, dev := setupStore(t, "apns-token")
	if err := s.SetPushToStartToken(dev.ID, "pts-token", testNow); err != nil {
		t.Fatal(err)
	}
	dev, err := s.Device(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{respond: func(req apns.Request) (apns.Result, error) {
		if req.PushType == "liveactivity" {
			return apns.Result{Status: 410, Reason: "Unregistered", Prune: true}, nil
		}
		return apns.Result{Status: 200}, nil
	}}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, time.Second)

	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	got, err := s.Device(dev.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.PushToStartToken != "" {
		t.Fatalf("stale push-to-start token not cleared: %q", got.PushToStartToken)
	}
}

// TestCloudApprovalSessionTokenPreferred 会话级 token(App 前台起卡)优先于
// push-to-start:approval_required 走 update(无 start 属性),close 走
// end(dismissal-date = now+5)。
func TestCloudApprovalSessionTokenPreferred(t *testing.T) {
	s, dev := setupStore(t, "")
	if err := s.SetPushToStartToken(dev.ID, "pts-token", testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLAToken(store.LAToken{
		DeviceID: dev.ID, DaemonID: testDaemon, SessionID: "sess_1",
		PushToken: "la-token", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	f := &fakePusher{}
	d, _ := newDeliverer(t, s, f, &testClock{t: time.Unix(testNow, 0)}, 10*time.Millisecond)

	d.Notify(context.Background(), dev, event(wire.EventApprovalRequired, "apv_1"))
	waitFor(t, "cloud update push", func() bool { return len(f.snapshot()) >= 2 })

	time.Sleep(50 * time.Millisecond) // 越过节流间隔,让 close 的 end 推送放行
	closeEnv := event(wire.EventApprovalClosed, "apv_1")
	closeEnv.Payload.StatusText = wire.CloseReasonDenied
	d.Notify(context.Background(), dev, closeEnv)
	waitFor(t, "cloud end push", func() bool {
		for _, c := range f.snapshot() {
			if p, ok := c.Payload.(wire.CloudApprovalPush); ok && p.APS.Event == "end" {
				return true
			}
		}
		return false
	})

	var update, end wire.CloudApprovalPush
	var endReq apns.Request
	for _, c := range f.snapshot() {
		p, ok := c.Payload.(wire.CloudApprovalPush)
		if !ok {
			continue
		}
		switch p.APS.Event {
		case "update":
			update = p
		case "end":
			end = p
			endReq = c
		}
	}
	if update.APS.AttributesType != "" || update.APS.Attributes != nil {
		t.Fatalf("session-token update must not carry start attributes: %+v", update.APS)
	}
	if update.APS.ContentState.State != wire.CloudStateNeedsYou {
		t.Fatalf("update state = %+v", update.APS.ContentState)
	}
	if end.APS.ContentState.State != wire.CloudStateDenied {
		t.Fatalf("end state = %+v", end.APS.ContentState)
	}
	if end.APS.DismissalDate != testNow+5 {
		t.Fatalf("dismissal-date = %d", end.APS.DismissalDate)
	}
	if endReq.DeviceToken != "la-token" {
		t.Fatalf("end push should target the session token: %+v", endReq)
	}
}
