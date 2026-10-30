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
	"github.com/twotwoba/DevPilot-APNS/internal/ratelimit"
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
	return newTestEnvOpts(t, testEnvOpts{Notifier: notifier})
}

func newTestEnvFull(t *testing.T, notifier wss.Notifier, q *quota.Engine) *testEnv {
	return newTestEnvOpts(t, testEnvOpts{Notifier: notifier, Quota: q})
}

type testEnvOpts struct {
	Notifier          wss.Notifier
	Quota             *quota.Engine
	Bans              *ratelimit.BanManager
	TrustedProxyCount int
	MetricsToken      string
}

func newTestEnvOpts(t *testing.T, o testEnvOpts) *testEnv {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := metrics.New()
	hub := wss.New(wss.Config{Store: st, Metrics: m, Notifier: o.Notifier, Quota: o.Quota})
	srv, err := New(Config{
		Store: st, Hub: hub, Metrics: m,
		Bans: o.Bans, TrustedProxyCount: o.TrustedProxyCount, MetricsToken: o.MetricsToken,
	})
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
		"daemon_name":      "Eric's MacBook",
		"expires_at":       time.Now().Add(10 * time.Minute).Unix(),
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register pairing session HTTP %d: %s", resp.StatusCode, body)
	}

	resp, body = e.post(t, "/v1/pair", map[string]any{
		"code": testCode, "apns_token": "fake-token", "device_name": "iPhone",
	}, "")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("pair HTTP %d: %s", resp.StatusCode, body)
	}
	var paired struct {
		DeviceID         string `json:"device_id"`
		DeviceCredential string `json:"device_credential"`
	}
	if err := json.Unmarshal(body, &paired); err != nil {
		t.Fatal(err)
	}
	if paired.DeviceID == "" || paired.DeviceCredential == "" {
		t.Fatalf("pair response missing fields: %s", body)
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
		t.Fatalf("websocket dial failed (HTTP %d): %v", status, err)
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
		t.Fatalf("send failed: %v", err)
	}
}

func recv(t *testing.T, c *websocket.Conn) any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("read failed: %v", err)
	}
	msg, err := wire.Decode(data)
	if err != nil {
		t.Fatalf("decode failed: %v (%s)", err, data)
	}
	return msg
}

func hello(t *testing.T, c *websocket.Conn, h wire.Hello) {
	t.Helper()
	send(t, c, h)
	if _, ok := recv(t, c).(*wire.HelloAck); !ok {
		t.Fatal("missing hello_ack")
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
		t.Fatalf("app received event = %+v", live)
	}

	// 手机允许。决策落地后手机先收到合成的 approval_closed(撤卡/横幅变脸),
	// 再收到 ack;daemon 收到转发决策。
	send(t, app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_d1",
		ApprovalID: approvalID, Decision: wire.DecisionAllow, TS: time.Now().Unix(),
	})
	d, ok := recv(t, daemon).(*wire.Decision)
	if !ok || d.ApprovalID != approvalID || d.Decision != wire.DecisionAllow {
		t.Fatalf("daemon received decision = %+v", d)
	}
	closed, ok := recv(t, app).(*wire.Envelope)
	if !ok || closed.Type != wire.EventApprovalClosed || closed.EventID != "close-"+approvalID ||
		closed.Payload.StatusText != wire.CloseReasonApproved {
		t.Fatalf("synthesized close = %+v", closed)
	}
	ack, ok = recv(t, app).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("decision ack = %+v", ack)
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
			t.Fatalf("decision not marked delivered: %+v", items)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// resync 返回历史事件，pending 为空。
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs1"})
	rs, ok := recv(t, app).(*wire.ResyncResult)
	if !ok {
		t.Fatal("missing resync_result")
	}
	if len(rs.Pending) != 0 {
		t.Fatalf("decided approval still in pending: %+v", rs.Pending)
	}
	if len(rs.Events) != 2 || rs.Events[0].EventID != "ev_1" ||
		rs.Events[1].Type != wire.EventApprovalClosed || rs.Events[1].EventID != "close-"+approvalID {
		t.Fatalf("resync events = %+v", rs.Events)
	}

	// 重复 event_id 不重复入箱。
	send(t, daemon, env1)
	ack, ok = recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("duplicate event ack = %+v", ack)
	}
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs2"})
	rs, _ = recv(t, app).(*wire.ResyncResult)
	if len(rs.Events) != 2 {
		t.Fatalf("duplicate event entered inbox: %d", len(rs.Events))
	}

	// 重复决策 first-wins。
	send(t, app, wire.Decision{
		V: wire.Version, Kind: wire.KindDecision, MsgID: "m_d2",
		ApprovalID: approvalID, Decision: wire.DecisionDeny, TS: time.Now().Unix(),
	})
	ack, ok = recv(t, app).(*wire.Ack)
	if !ok || ack.Status != wire.AckAlreadyDecided {
		t.Fatalf("duplicate decision ack = %+v", ack)
	}
	p, err := env.store.GetPending(approvalID)
	if err != nil || p.Status != "approved" {
		t.Fatalf("decision was overwritten: %+v err=%v", p, err)
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
		t.Fatal("notifier not called")
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
	closed, ok := recv(t, app).(*wire.Envelope)
	if !ok || closed.Type != wire.EventApprovalClosed || closed.Payload.StatusText != wire.CloseReasonDenied {
		t.Fatalf("synthesized close = %+v", closed)
	}
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
		t.Fatalf("replayed decision = %+v", d)
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
			t.Fatalf("decision not marked delivered: %+v", items)
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
		t.Fatal("unregistered daemon should not connect")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("HTTP status = %+v", resp)
	}
}

func TestPairingCodeOneTime(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)
	resp, body := env.post(t, "/v1/pair", map[string]any{"code": testCode}, "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("reuse code HTTP %d: %s", resp.StatusCode, body)
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
		t.Fatalf("first registration HTTP %d: %s", resp.StatusCode, b)
	}
	// 无旧凭据不能轮换。
	body["code"] = "ZZZZZZZZZZZZZZZZZZZZZZZZZZ"
	body["daemon_cred_hash"] = pairing.SecretHash("newnewnewnewnewnewnewnewnewnewnewnew")
	if resp, b := env.post(t, "/v1/pairing/sessions", body, ""); resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated rotation should 401, got %d: %s", resp.StatusCode, b)
	}
	// 持有旧凭据可以轮换。
	oldCred := pairing.DaemonCredential(testDaemonID, seedSecret)
	if resp, b := env.post(t, "/v1/pairing/sessions", body, oldCred); resp.StatusCode != http.StatusCreated {
		t.Fatalf("credential rotation HTTP %d: %s", resp.StatusCode, b)
	}
	if _, ok := env.store.VerifyDaemon(testDaemonID, "newnewnewnewnewnewnewnewnewnewnewnew"); !ok {
		t.Fatal("new credential not active")
	}
	if _, ok := env.store.VerifyDaemon(testDaemonID, seedSecret); ok {
		t.Fatal("old credential still valid")
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
		t.Fatalf("end LA token HTTP %d", resp.StatusCode)
	}
	tokens, _ = env.store.LATokensForDaemon(testDaemonID)
	if len(tokens) != 0 {
		t.Fatal("LA token not deleted")
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
			t.Fatalf("event %d should be accepted: %+v", i, ack)
		}
	}
	send(t, daemon, mk(3, wire.EventToolRunning))
	ack, ok := recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckRejected {
		t.Fatalf("event 3 should be rejected by rate limit: %+v", ack)
	}
	// 审批类豁免速率闸。
	send(t, daemon, mk(4, wire.EventApprovalRequired))
	ack, ok = recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("approval event should be exempt and accepted: %+v", ack)
	}
	// 被拒事件不落库:收件箱只有 2 条遥测 + 1 条审批。
	events, _, err := env.store.InboxSince(testDaemonID, 0, 0, 10)
	if err != nil || len(events) != 3 {
		t.Fatalf("inbox should have 3 events: %d err=%v", len(events), err)
	}
}

// TestDeviceLifecycle token 重报与设备自助解绑。
func TestDeviceLifecycle(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	resp, body := env.post(t, "/v1/devices/token", map[string]any{"apns_token": "new-token-xyz"}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("token re-report HTTP %d: %s", resp.StatusCode, body)
	}
	dev, err := env.store.Device(env.deviceID)
	if err != nil || dev.APNSToken != "new-token-xyz" {
		t.Fatalf("token not updated: %+v %v", dev, err)
	}
	// 无凭据 401;空 token 400。
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{"apns_token": "x"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credential should 401: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{"apns_token": " "}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank token should 400: %d", resp.StatusCode)
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
		t.Fatalf("unbind HTTP %d", resp2.StatusCode)
	}
	if _, err := env.store.Device(env.deviceID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("device should be deleted: %v", err)
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
		t.Fatalf("revoke HTTP %d", resp.StatusCode)
	}
	if _, err := env.store.Daemon(testDaemonID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("daemon should be cascade-revoked: %v", err)
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
		t.Fatalf("device credential should be invalid after revoke: %d", resp2.StatusCode)
	}
	// daemon 凭据同样失效(WS 401)。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, wsResp, err := websocket.Dial(ctx, wsAddr(env.ts, "/ws/daemon"), &websocket.DialOptions{
		HTTPHeader: http.Header{"Authorization": {"Bearer " + env.cred}},
	})
	if err == nil {
		t.Fatal("websocket should be rejected after revoke")
	}
	if wsResp == nil || wsResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("websocket should 401 after revoke: %+v", wsResp)
	}
}

// recvClosed 断言连接已被服务端断开（读取出错，而非超时后仍能收到消息）。
func recvClosed(t *testing.T, c *websocket.Conn) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, _, err := c.Read(ctx); err == nil {
		t.Fatal("connection should have been closed by server")
	}
}

// TestDeleteDeviceKicksLiveConn 设备解绑后，其在线 WSS 连接应被立即断开，
// 不残留还能收事件的僵尸连接。
func TestDeleteDeviceKicksLiveConn(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_kick",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: time.Now().Unix(),
	})

	resp, body := env.doXFF(t, http.MethodDelete, "/v1/device", nil, env.deviceCr, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unbind HTTP %d: %s", resp.StatusCode, body)
	}
	recvClosed(t, app)
}

// TestFanOutSkipsZombieConn 设备行已删但连接仍在的竞态窗口里（绕过
// handler 直接删行模拟 kick 未触达），事件扇出必须跳过该连接并顺手
// 断开，而不是继续向已注销设备投递。
func TestFanOutSkipsZombieConn(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_zd",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_za",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: time.Now().Unix(),
	})

	// 竞态窗口：store 里设备已删，但连接注册表尚未摘除。
	if err := env.store.DeleteDevice(env.deviceID); err != nil {
		t.Fatal(err)
	}
	send(t, daemon, wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_evz", EventID: "ev_z",
		ApprovalID: "apv_" + strings.Repeat("z", 26), Type: wire.EventApprovalRequired,
		Agent: wire.AgentClaude, DaemonID: testDaemonID, SessionID: "sess_z",
		TS: time.Now().Unix(),
	})
	ack, ok := recv(t, daemon).(*wire.Ack)
	if !ok || ack.Status != wire.AckAccepted {
		t.Fatalf("event ack = %+v", ack)
	}
	// App 侧：收不到事件，且连接被复核逻辑断开。
	recvClosed(t, app)
}

// TestDeleteDaemonKicksLiveConns daemon 吊销后，其自身与名下设备的
// 在线连接都应被立即断开。
func TestDeleteDaemonKicksLiveConns(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_dd",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_da",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: time.Now().Unix(),
	})

	resp, body := env.doXFF(t, http.MethodDelete, "/v1/daemon", nil, env.cred, "")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke HTTP %d: %s", resp.StatusCode, body)
	}
	recvClosed(t, daemon)
	recvClosed(t, app)
}

// TestDeviceTokenUpdatesTier token 端点顺带刷新 daemon 档位:升级/降级
// 落库并踢 daemon 连接(重连后配额按新档生效);缺 tier 字段旧版兼容不动档;
// 非法档 400 且不落库。
func TestDeviceTokenUpdatesTier(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_t1",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})

	resp, body := env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "tok-1", "tier": "pro",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("upgrade HTTP %d: %s", resp.StatusCode, body)
	}
	d, err := env.store.Daemon(testDaemonID)
	if err != nil || d.Tier != store.TierPro {
		t.Fatalf("tier not upgraded: %+v err=%v", d, err)
	}
	// 配额按建连时缓存的 tier 结算,改档必须踢连接促重连。
	recvClosed(t, daemon)

	// 降级对称可逆。
	daemon = dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_t2",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "tok-2", "tier": "free",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("downgrade HTTP %d", resp.StatusCode)
	}
	if d, err = env.store.Daemon(testDaemonID); err != nil || d.Tier != store.TierFree {
		t.Fatalf("tier not downgraded: %+v err=%v", d, err)
	}
	recvClosed(t, daemon)

	// 非法档 400 且不落库、不踢连接;缺 tier 字段照常 204。
	daemon = dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_t3",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{
		"apns_token": "tok-3", "tier": "vip",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid tier should 400: %d", resp.StatusCode)
	}
	if d, err = env.store.Daemon(testDaemonID); err != nil || d.Tier != store.TierFree {
		t.Fatalf("invalid tier should not persist: %+v err=%v", d, err)
	}
	resp, _ = env.post(t, "/v1/devices/token", map[string]any{"apns_token": "tok-4"}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("missing tier should 204: %d", resp.StatusCode)
	}
	if d, err = env.store.Daemon(testDaemonID); err != nil || d.Tier != store.TierFree {
		t.Fatalf("missing tier should not change tier: %+v err=%v", d, err)
	}
	// 连接未被踢(合法性校验失败不该殃及在线会话):Ping 应有 Pong 应答。
	send(t, daemon, wire.Ping{V: wire.Version, Kind: wire.KindPing, TS: time.Now().Unix()})
	if _, ok := recv(t, daemon).(*wire.Pong); !ok {
		t.Fatal("report without tier should not close connection")
	}
}

// doXFF 带可选 Bearer 与 X-Forwarded-For 的通用请求(限流/封禁测试用)。
func (e *testEnv) doXFF(t *testing.T, method, path string, body any, token, xff string) (*http.Response, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(data)
	}
	req, err := http.NewRequest(method, e.ts.URL+path, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if xff != "" {
		req.Header.Set("X-Forwarded-For", xff)
	}
	resp, err := e.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp, out
}

func TestRealIP(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		xff     string
		trusted int
		want    string
	}{
		{"direct public ignores XFF", "203.0.113.9", "1.2.3.4", 1, "203.0.113.9"},
		{"zero trusted ignores XFF", "10.0.0.2", "1.2.3.4", 0, "10.0.0.2"},
		{"single proxy takes rightmost", "127.0.0.1", "1.2.3.4", 1, "1.2.3.4"},
		{"spoofed left entries ignored", "127.0.0.1", "6.6.6.6, 1.2.3.4", 1, "1.2.3.4"},
		{"two-hop chain takes second from right", "127.0.0.1", "5.5.5.5, 1.2.3.4, 10.0.0.9", 2, "1.2.3.4"},
		{"garbage XFF falls back", "127.0.0.1", "garbage", 1, "127.0.0.1"},
		{"portless RemoteAddr passthrough", "203.0.113.9", "", 0, "203.0.113.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := realIP(c.remote, c.xff, c.trusted); got != c.want {
				t.Fatalf("realIP(%q,%q,%d) = %q, want %q", c.remote, c.xff, c.trusted, got, c.want)
			}
		})
	}
}

// TestBanMiddleware fail2ban 闭环:失败累计→封禁→403;环回豁免;他人不受牵连。
func TestBanMiddleware(t *testing.T) {
	bans := ratelimit.NewBanManager(ratelimit.BanConfig{Threshold: 3, Window: time.Minute, TTL: time.Hour})
	env := newTestEnvOpts(t, testEnvOpts{Bans: bans, TrustedProxyCount: 1})

	// 以 XFF 标记客户端 203.0.113.7,连打 3 次非法配对(400 计失败)。
	for range 3 {
		resp, _ := env.doXFF(t, http.MethodPost, "/v1/pairing/sessions",
			map[string]any{"code": "BAD"}, "", "203.0.113.7")
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("invalid pairing should 400: %d", resp.StatusCode)
		}
	}
	// 已封禁:任何端点 403(封禁检查在失败豁免之前)。
	resp, _ := env.doXFF(t, http.MethodGet, "/healthz", nil, "", "203.0.113.7")
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("banned should 403: %d", resp.StatusCode)
	}
	if bans.Active() != 1 {
		t.Fatalf("Active = %d", bans.Active())
	}
	// 环回直连豁免(容器 HEALTHCHECK 形态)。
	resp, _ = env.doXFF(t, http.MethodGet, "/healthz", nil, "", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback should be exempt: %d", resp.StatusCode)
	}
	// 其他 IP 不受牵连。
	resp, _ = env.doXFF(t, http.MethodGet, "/healthz", nil, "", "198.51.100.5")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unbanned IP should not be affected: %d", resp.StatusCode)
	}
}

func TestMetricsToken(t *testing.T) {
	env := newTestEnvOpts(t, testEnvOpts{MetricsToken: "s3cret"})
	resp, _ := env.doXFF(t, http.MethodGet, "/metrics", nil, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token should 401: %d", resp.StatusCode)
	}
	resp, body := env.doXFF(t, http.MethodGet, "/metrics", nil, "s3cret", "")
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "devpilot_events_total") {
		t.Fatalf("with token should 200: %d %s", resp.StatusCode, body)
	}
}

// TestDecisionREST REST 决策通道(LA 按钮/App Intent 与通知按钮的回落路径):
// 认证、参数校验、first-wins、unknown、daemon 转发与 close 合成均与 WSS 共路。
func TestDecisionREST(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	now := time.Now().Unix()
	approvalID := "apv_" + strings.Repeat("r", 26)
	ev := wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_rest", EventID: "ev_rest",
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: testDaemonID, SessionID: "sess_1", TS: now,
		Payload: wire.Payload{ToolName: "Bash"},
	}
	if _, _, err := env.store.AppendInbox(testDaemonID, ev, now); err != nil {
		t.Fatal(err)
	}
	if err := env.store.PutPending(store.Pending{
		ApprovalID: approvalID, DaemonID: testDaemonID, Event: ev, Status: "pending",
		CreatedAt: now, ExpiresAt: now + 1800,
	}); err != nil {
		t.Fatal(err)
	}

	// 无凭据 401;非法 decision 400;缺 approval_id 400。
	resp, _ := env.post(t, "/v1/decisions", map[string]any{"approval_id": approvalID, "decision": "allow"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credential should 401: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/decisions", map[string]any{"approval_id": approvalID, "decision": "maybe"}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid decision should 400: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/decisions", map[string]any{"decision": "allow"}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing approval_id should 400: %d", resp.StatusCode)
	}

	// daemon 在线:决策应转发,close 应合成入箱。
	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_rd",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: now,
	})
	resp, body := env.post(t, "/v1/decisions",
		map[string]any{"approval_id": approvalID, "decision": "allow"}, env.deviceCr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("decide HTTP %d: %s", resp.StatusCode, body)
	}
	var out struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Status != string(wire.AckAccepted) {
		t.Fatalf("status = %q (%s)", out.Status, body)
	}
	d, ok := recv(t, daemon).(*wire.Decision)
	if !ok || d.ApprovalID != approvalID || d.Decision != wire.DecisionAllow {
		t.Fatalf("daemon received decision = %+v", d)
	}

	// close 事件进收件箱:App 离线也能经 resync 恢复终态。
	app := dial(t, env.ts, "/ws/app", env.deviceCr)
	hello(t, app, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_ra",
		Role: wire.RoleApp, DeviceID: env.deviceID, TS: now,
	})
	send(t, app, wire.Resync{V: wire.Version, Kind: wire.KindResync, MsgID: "m_rs"})
	rs, ok := recv(t, app).(*wire.ResyncResult)
	if !ok {
		t.Fatal("missing resync_result")
	}
	sawClose := false
	for _, e := range rs.Events {
		if e.Type == wire.EventApprovalClosed {
			sawClose = e.EventID == "close-"+approvalID && e.Payload.StatusText == wire.CloseReasonApproved
		}
	}
	if !sawClose {
		t.Fatalf("approval_closed missing from inbox: %+v", rs.Events)
	}

	// first-wins 与 unknown。
	resp, body = env.post(t, "/v1/decisions",
		map[string]any{"approval_id": approvalID, "decision": "deny"}, env.deviceCr)
	if err := json.Unmarshal(body, &out); err != nil || out.Status != string(wire.AckAlreadyDecided) {
		t.Fatalf("duplicate decision status = %q (%s)", out.Status, body)
	}
	resp, body = env.post(t, "/v1/decisions",
		map[string]any{"approval_id": "apv_nope", "decision": "deny"}, env.deviceCr)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("unknown decision HTTP %d: %s", resp.StatusCode, body)
	}
	if err := json.Unmarshal(body, &out); err != nil || out.Status != string(wire.AckUnknown) {
		t.Fatalf("unknown status = %q (%s)", out.Status, body)
	}
}

// TestLATokenPushStartREST push_start 上报设备级云审批 LA token:
// 允许空 session_id(App 未起任何 activity);缺 push_token 400;未知 event 400。
func TestLATokenPushStartREST(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	resp, body := env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "event": "push_start", "push_token": "pts-token",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("push_start HTTP %d: %s", resp.StatusCode, body)
	}
	dev, err := env.store.Device(env.deviceID)
	if err != nil || dev.PushToStartToken != "pts-token" {
		t.Fatalf("device = %+v err=%v", dev, err)
	}

	resp, _ = env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "event": "push_start",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("push_start without token should 400: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/la-tokens", map[string]any{
		"daemon_id": testDaemonID, "session_id": "sess_1", "event": "bogus", "push_token": "x",
	}, env.deviceCr)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown event should 400: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/v1/la-tokens", map[string]any{"event": "push_start", "push_token": "x"}, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credential should 401: %d", resp.StatusCode)
	}
}

// TestDaemonWSRecordsObservedIP daemon WSS 建连即记录观测源 IP
// (v3 增量字段,NAT 后公网地址仅作路由提示)。
func TestDaemonWSRecordsObservedIP(t *testing.T) {
	env := newTestEnv(t)
	env.registerAndPair(t)

	daemon := dial(t, env.ts, "/ws/daemon", env.cred)
	hello(t, daemon, wire.Hello{
		V: wire.Version, Kind: wire.KindHello, MsgID: "m_hello_ip",
		Role: wire.RoleDaemon, DaemonID: testDaemonID, TS: time.Now().Unix(),
	})
	d, err := env.store.Daemon(testDaemonID)
	if err != nil || d.LastIP == "" {
		t.Fatalf("daemon last_ip not recorded: %+v err=%v", d, err)
	}
}
