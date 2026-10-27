package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/internal/quota"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

const (
	testDaemonID = "dd_0123456789abcdef0123456789abcdef"
	testCode     = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	testSecret   = "0123456789abcdef0123456789abcdef"
)

type testEnv struct {
	ts       *httptest.Server
	store    *store.Store
	hub      *wss.Hub
	cred     string // daemon 凭据
	deviceID string
	deviceCr string
	client   *http.Client
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	return newTestEnvWithNotifier(t, nil)
}

func newTestEnvWithNotifier(t *testing.T, notifier wss.Notifier) *testEnv {
	return newTestEnvFull(t, notifier, nil)
}

func newTestEnvFull(t *testing.T, notifier wss.Notifier, q *quota.Engine) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := metrics.New()
	hub := wss.New(wss.Config{Store: st, Metrics: m, Notifier: notifier, Quota: q})
	srv, err := New(Config{Store: st, Hub: hub, Metrics: m})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &testEnv{ts: ts, store: st, hub: hub, client: ts.Client()}
}

func (e *testEnv) post(t *testing.T, path string, body any, token string) (*http.Response, []byte) {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, e.ts.URL+path, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

// registerAndPair 完成 daemon 注册 + App 兑换，填充测试上下文。
func (e *testEnv) registerAndPair(t *testing.T) {
	t.Helper()
	e.cred = pairing.DaemonCredential(testDaemonID, testSecret)
	resp, body := e.post(t, "/v1/pairing/sessions", map[string]any{
		"code": testCode, "daemon_id": testDaemonID,
		"daemon_cred_hash": pairing.SecretHash(testSecret),
		"daemon_name":      "Eric 的 MacBook",
		"expires_at":       time.Now().Add(10 * time.Minute).Unix(),
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("注册配对会话 HTTP %d: %s", resp.StatusCode, body)
	}

	resp, body = e.post(t, "/v1/pair", map[string]any{
		"code": testCode, "apns_token": "fake-token", "device_name": "iPhone",
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("配对 HTTP %d: %s", resp.StatusCode, body)
	}
	var paired struct {
		DeviceID         string `json:"device_id"`
		DeviceCredential string `json:"device_credential"`
	}
	if err := json.Unmarshal(body, &paired); err != nil {
		t.Fatal(err)
	}
	if paired.DeviceID == "" || paired.DeviceCredential == "" {
		t.Fatalf("配对响应缺少字段: %s", body)
	}
	e.deviceID = paired.DeviceID
	e.deviceCr = paired.DeviceCredential
}

func wsAddr(ts *httptest.Server, path string) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + path
}

func dial(t *testing.T, ts *httptest.Server, path, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, wsAddr(ts, path), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + token}},
	})
	if err != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		t.Fatalf("WebSocket 连接失败（HTTP %d）: %v", status, err)
	}
	t.Cleanup(func() { c.CloseNow() })
	return c
}

func send(t *testing.T, c *websocket.Conn, v any) {
	t.Helper()
	data, err := wire.Encode(v)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, data); err != nil {
		t.Fatalf("发送失败: %v", err)
	}
}

func recv(t *testing.T, c *websocket.Conn) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	msg, err := wire.Decode(data)
	if err != nil {
		t.Fatalf("解码失败: %v (%s)", err, data)
	}
	return msg
}

func hello(t *testing.T, c *websocket.Conn, h wire.Hello) {
	t.Helper()
	send(t, c, h)
	if _, ok := recv(t, c).(*wire.HelloAck); !ok {
		t.Fatal("缺少 hello_ack")
	}
}

func TestPairAndApprovalFlow(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_d",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})

	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_a",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: time.Now().Unix(),
	})

	approvalID := "apv_" + strings.Repeat("a", 26)
	env1 := wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_ev1", EventID: "ev_1",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_1", TS: time.Now().Unix(),
		Payload: wire.Payload{ToolName: "Bash", CommandSummary: "rm -rf build"},
	}
	send(t, daemon, env1)
	ack, ok := recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted || ack.EventID != "ev_1" {
		t.Fatalf("event ack = %+v", ack)
	}
	live, ok := recv(t, app).(*wire.Envelope)
	if !ok || live.EventID != "ev_1" || live.Seq == 0 {
		t.Fatalf("App 收到的事件 = %+v", live)
	}

	// 手机允许。
	send(t, app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_d1",
		ApprovalID: approvalID, Decision: wire.DecisionAllow, TS: time.Now().Unix(),
	})
	d, ok := recv(t, daemon).(*wire.Decision)
	if !ok || d.ApprovalID != approvalID || d.Decision != wire.DecisionAllow {
		t.Fatalf("daemon 收到的决策 = %+v", d)
	}
	ack, ok = recv(t, app).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("决策 ack = %+v", ack)
	}

	// daemon 确认收到决策后不再补发。
	send(t, daemon, wire.Ack{
		V: wire.Version, Kind: wire.KindAck, MsgID: "m_ack", ApprovalID: approvalID,
		Status: wire.AckAccepted, TS: time.Now().Unix(),
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		items, err := env.store.UndeliveredDecisions(testDaemonID, time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("决策未标记送达: %+v", items)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// resync 返回历史事件，pending 为空。
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs1"})
	rs, ok := recv(t, app).(*wire.ResyncResult)
	if !ok {
		t.Fatal("缺少 resync_result")
	}
	if len(rs.Pending) != 0 {
		t.Fatalf("已决策审批仍出现在 pending: %+v", rs.Pending)
	}
	if len(rs.Events) != 1 || rs.Events[0].EventID != "ev_1" {
		t.Fatalf("resync events = %+v", rs.Events)
	}

	// 重复 event_id 不重复入箱。
	send(t, daemon, env1)
	ack, ok = recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("重复事件 ack = %+v", ack)
	}
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs2"})
	rs, _ = recv(t, app).(*wire.ResyncResult)
	if len(rs.Events) != 1 {
		t.Fatalf("重复事件进入了收件箱: %d", len(rs.Events))
	}

	// 重复决策 first-wins。
	send(t, app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_d2",
		ApprovalID: approvalID, Decision: wire.DecisionDeny, TS: time.Now().Unix(),
	})
	ack, ok = recv(t, app).(*wire.Ack)
	if !ok || ack.Status != wire.AckAlreadyDecided {
		t.Fatalf("重复决策 ack = %+v", ack)
	}
	p, err := env.store.GetPending(approvalID)
	if err != nil || p.Status != "approved" {
		t.Fatalf("决策被覆盖: %+v err=%v", p, err)
	}
}

// recordingNotifier 记录 Hub 交给 APNs 投递层的事件。
type recordingNotifier struct {
	ch chan notifyCall
}

type notifyCall struct {
	device store.Device
	env    wire.Envelope
}

func newRecordingNotifier() *recordingNotifier {
	return &recordingNotifier{ch: make(chan notifyCall, 8)}
}

func (n *recordingNotifier) Notify(_ context.Context, device store.Device, env wire.Envelope) {
	n.ch <- notifyCall{device: device, env: env}
}

func TestApprovalFansOutToNotifier(t *testing.T) {
	n := newRecordingNotifier()
	env := newTestEnvWithNotifier(t, n)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_d",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})

	approvalID := "apv_" + strings.Repeat("b", 26)
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_ev_n", EventID: "ev_n",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_1", TS: time.Now().Unix(),
		Payload: wire.Payload{ToolName: "Bash", CommandSummary: "rm -rf build"},
	})

	select {
	case got := <-n.ch:
		if got.device.ID != env.deviceID || got.device.APNSToken != "fake-token" {
			t.Fatalf("device = %+v", got.device)
		}
		if got.env.EventID != "ev_n" || got.env.ApprovalID != approvalID {
			t.Fatalf("env = %+v", got.env)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Notifier 未被调用")
	}
}

func TestDecisionReplayedWhenDaemonOffline(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	now := time.Now().Unix()
	approvalID := "apv_" + strings.Repeat("c", 26)
	ev := wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_offline", EventID: "ev_offline",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_1", TS: now,
		Payload: wire.Payload{ToolName: "Bash"},
	}
	if _, _, err := env.store.AppendInbox(testDaemonID, ev, now); err != nil {
		t.Fatal(err)
	}
	if err := env.store.PutPending(store.Pending{
		ApprovalID: approvalID, DaemonID: testDaemonID, Event: ev, Status: "pending",
		CreatedAt: now, ExpiresAt: now + 300,
	}); err != nil {
		t.Fatal(err)
	}

	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_a",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: now,
	})
	send(t, app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_offline_d",
		ApprovalID: approvalID, Decision: wire.DecisionDeny, TS: now,
	})
	ack, ok := recv(t, app).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("ack = %+v", ack)
	}

	// daemon 此时不在线；重连后必须收到未送达的决策。
	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_d2",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: now,
	})
	d, ok := recv(t, daemon).(*wire.Decision)
	if !ok || d.ApprovalID != approvalID || d.Decision != wire.DecisionDeny {
		t.Fatalf("补发的决策 = %+v", d)
	}

	// daemon ack 之后不再重放。
	send(t, daemon, wire.Ack{
		V: wire.Version, Kind: wire.KindAck, MsgID: "m_ack2", ApprovalID: approvalID,
		Status: wire.AckAccepted, TS: now,
	})
	deadline := time.Now().Add(2 * time.Second)
	for {
		items, err := env.store.UndeliveredDecisions(testDaemonID, now+1)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("决策未被标记送达: %+v", items)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUnpairedDaemonRejected(t *testing.T) {
	env := newTestEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, resp, err := websocket.Dial(ctx, wsAddr(env.ts, "/ws/daemon"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer dpd_dd_ffffffffffffffffffffffffffffffff_" + strings.Repeat("a", 32)}},
	})
	if err == nil {
		t.Fatal("未注册 daemon 不应连上")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP 状态 = %+v", resp)
	}
}

func TestPairingCodeOneTime(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)
	resp, body := env.post(t, "/v1/pair", map[string]any{"code": testCode}, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("重复兑换 HTTP %d: %s", resp.StatusCode, body)
	}
}

func TestPairingRotationRequiresAuth(t *testing.T) {
	env := newTestEnv(t)
	seedSecret := strings.Repeat("c", 32)
	body := map[string]any{
		"code": "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "daemon_id": testDaemonID,
		"daemon_cred_hash": pairing.SecretHash(seedSecret), "daemon_name": "mac",
	}
	if resp, b := env.post(t, "/v1/pairing/sessions", body, ""); resp.StatusCode != http.StatusCreated {
		t.Fatalf("首次注册 HTTP %d: %s", resp.StatusCode, b)
	}
	// 无旧凭据不能轮换。
	body["code"] = "ZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	body["daemon_cred_hash"] = pairing.SecretHash("newnewnewnewnewnewnewnewnewnewnewnew")
	if resp, b := env.post(t, "/v1/pairing/sessions", body, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("未鉴权轮换应 401，实际 %d: %s", resp.StatusCode, b)
	}
	// 持有旧凭据可以轮换。
	oldCred := pairing.DaemonCredential(testDaemonID, seedSecret)
	if resp, b := env.post(t, "/v1/pairing/sessions", body, oldCred); resp.StatusCode != http.StatusCreated {
		t.Fatalf("凭据轮换 HTTP %d: %s", resp.StatusCode, b)
	}
	if _, ok := env.store.VerifyDaemon(testDaemonID, "newnewnewnewnewnewnewnewnewnewnewnew"); !ok {
		t.Fatal("新凭据未生效")
	}
	if _, ok := env.store.VerifyDaemon(testDaemonID, seedSecret); ok {
		t.Fatal("旧凭据仍然有效")
	}
}

func TestHealthAndMetrics(t *testing.T) {
	env := newTestEnv(t)
	for path, want := range map[string]int{
		"/healthz": http.StatusOK,
		"/readyz":  http.StatusOK,
		"/metrics": http.StatusOK,
	} {
		resp, err := env.client.Get(env.ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != want {
			t.Fatalf("%s = %d", path, resp.StatusCode)
		}
	}
}

func TestLATokenREST(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)
	resp, body := env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "session_id": "sess_1",
		"push_token": "la-token", "event": "start",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("LA token HTTP %d: %s", resp.StatusCode, body)
	}
	tokens, err := env.store.LATokensForDaemon(testDaemonID)
	if err != nil || len(tokens) != 1 || tokens[0].PushToken != "la-token" {
		t.Fatalf("tokens = %+v err=%v", tokens, err)
	}
	resp, _ = env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "session_id": "sess_1", "event": "end",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("结束 LA token HTTP %d", resp.StatusCode)
	}
	tokens, _ = env.store.LATokensForDaemon(testDaemonID)
	if len(tokens) != 0 {
		t.Fatal("LA token 未删除")
	}
}

// TestEventRateLimitAck 端到端验证速率闸:超限回 AckRejected 且不落库,审批类豁免。
func TestEventRateLimitAck(t *testing.T) {
	eng := quota.New(quota.Config{
		RateEventFree: 2, RateEventPro: 2, RateEventWindow: time.Minute,
		QuotaDailyFree: 100, QuotaDailyPro: 100,
		ExemptApprovals: true,
	})
	env := newTestEnvFull(t, nil, eng)
	env.registerAndPair(t) // 未传 tier → free 档

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_rl_hello",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})

	mk := func(i int, typ wire.EventType) wire.Envelope {
		return wire.Envelope{
			V: wire.Version, Kind: wire.KindEvent,
			MsgID: fmt.Sprintf("m_rl_%d", i), EventID: fmt.Sprintf("ev_rl_%d", i),
			ApprovalID: fmt.Sprintf("apv_rl_%d", i), Type: typ, Agent: wire.AgentClaude,
			DaemonID: testDaemonID, SessionID: "sess_1", TS: time.Now().Unix(),
			Payload: wire.Payload{ToolName: "Bash"},
		}
	}
	for i := 1; i <= 2; i++ {
		send(t, daemon, mk(i, wire.EventToolRunning))
		ack, ok := recv(t, daemon).(*wire.Ack)
		if !ok || ack.Status != wire.AckAccepted {
			t.Fatalf("事件 %d 应放行: %+v", i, ack)
		}
	}
	send(t, daemon, mk(3, wire.EventToolRunning))
	ack, ok := recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckRejected {
		t.Fatalf("第 3 个事件应被速率闸拒绝: %+v", ack)
	}
	// 审批类豁免速率闸。
	send(t, daemon, mk(4, wire.EventApprovalRequired))
	ack, ok = recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("审批事件应豁免放行: %+v", ack)
	}
	// 被拒事件不落库:收件箱只有 2 条遥测 + 1 条审批。
	events, _, err := env.store.InboxSince(testDaemonID, 0, 0, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("收件箱应有 3 条: %d err=%v", len(events), err)
	}
}

// TestDeviceLifecycle token 重报与设备自助解绑。
func TestDeviceLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	resp, body := env.post(t, "/v1/devices/token", map[string]any{"apns_token": "new-token-xyz"}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("token 重报 HTTP %d: %s", resp.StatusCode, body)
	}
	dev, err := env.store.Device(env.deviceID)
	if err != nil || dev.APNSToken != "new-token-xyz" {
		t.Fatalf("token 未更新: %+v %v", dev, err)
	}
	// 无凭据 401;空 token 400。
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{"apns_token": "x"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("无凭据应 401: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{"apns_token": " "}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("空 token 应 400: %d", resp.StatusCode)
	}

	// 自助解绑:设备与 LA token 级联删除。
	req, err := http.NewRequest(http.MethodDelete, env.ts.URL+"/v1/device", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.deviceCr)
	resp2, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNoContent {
		t.Fatalf("解绑 HTTP %d", resp2.StatusCode)
	}
	if _, err := env.store.Device(env.deviceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("设备应已删除: %v", err)
	}
}

// TestDeleteDaemonRevokesAll daemon 吊销:级联注销,daemon/device 凭据全部失效。
func TestDeleteDaemonRevokesAll(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	req, err := http.NewRequest(http.MethodDelete, env.ts.URL+"/v1/daemon", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+env.cred)
	resp, err := env.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("吊销 HTTP %d", resp.StatusCode)
	}
	if _, err := env.store.Daemon(testDaemonID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("daemon 应已级联注销: %v", err)
	}
	// 设备凭据随之失效。
	req2, err := http.NewRequest(http.MethodGet, env.ts.URL+"/v1/pending", nil)
	if err != nil {
		t.Fatal(err)
	}
	req2.Header.Set("Authorization", "Bearer "+env.deviceCr)
	resp2, err := env.client.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnauthorized {
		t.Fatalf("吊销后设备凭据应失效: %d", resp2.StatusCode)
	}
	// daemon 凭据同样失效(WS 401)。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, wsResp, err := websocket.Dial(ctx, wsAddr(env.ts, "/ws/daemon"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + env.cred}},
	})
	if err == nil {
		t.Fatal("吊销后 WS 应拒绝")
	}
	if wsResp == nil || wsResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("吊销后 WS 应 401: %+v", wsResp)
	}
}
