package store

import (
	"path/filepath"
	"testing"

	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

var testNow int64 = 1_760_000_000

func seedPairing(t *testing.T, s *Store, code, daemonID string) string {
	t.Helper()
	secret := "0123456789abcdef0123456789abcdef"
	hash := pairing.SecretHash(secret)
	if err := s.RegisterPairingSession(PairingSession{
		CodeHash: pairing.CodeHash(code), DaemonID: daemonID, DaemonName: "mac",
		CredHash: hash, ExpiresAt: testNow + 900, CreatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	return pairing.DaemonCredential(daemonID, secret)
}

func pairDevice(t *testing.T, s *Store, code string) (Daemon, Device) {
	t.Helper()
	secret, err := pairing.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	d := Device{ID: pairing.NewDeviceID(), CredHash: pairing.SecretHash(secret), Name: "iPhone"}
	daemon, device, err := s.Pair(pairing.CodeHash(code), d, testNow)
	if err != nil {
		t.Fatal(err)
	}
	return daemon, device
}

func TestPairingFlow(t *testing.T) {
	s := newStore(t)
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	cred := seedPairing(t, s, code, "dd_0123456789abcdef0123456789abcdef")

	daemon, device := pairDevice(t, s, code)
	if daemon.ID != "dd_0123456789abcdef0123456789abcdef" || daemon.Name != "mac" {
		t.Fatalf("daemon = %+v", daemon)
	}
	credID, credSecret, ok := pairing.ParseCredential(cred, pairing.DaemonPrefix, pairing.DaemonIDPrefix)
	if !ok {
		t.Fatalf("测试凭据无法解析: %s", cred)
	}
	if _, ok := s.VerifyDaemon(credID, credSecret); !ok {
		t.Fatal("daemon 凭据校验失败")
	}
	if _, ok := s.VerifyDevice(device.ID, "wrongwrongwrongwrongwrongwrongwrong"); ok {
		t.Fatal("错误 device secret 不应通过")
	}

	// 配对码一次性。
	if _, _, err := s.Pair(pairing.CodeHash(code), device, testNow); err != ErrCodeInvalid {
		t.Fatalf("重复使用配对码 err = %v", err)
	}
}

func TestPairingExpired(t *testing.T) {
	s := newStore(t)
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	seedPairing(t, s, code, "dd_0123456789abcdef0123456789abcdef")
	if _, _, err := s.Pair(pairing.CodeHash(code), Device{ID: "dv_x", CredHash: "h"}, testNow+901); err != ErrCodeExpired {
		t.Fatalf("err = %v", err)
	}
}

func TestPairReplacesOldDevice(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	_, first := pairDevice(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")

	seedPairing(t, s, "ZZZZZZZZZZZZZZZZZZZZZZZZZZ", daemonID)
	_, second := pairDevice(t, s, "ZZZZZZZZZZZZZZZZZZZZZZZZZZ")

	if _, err := s.Device(first.ID); err != ErrNotFound {
		t.Fatalf("旧设备未删除: %v", err)
	}
	devices, err := s.DevicesForDaemon(daemonID)
	if err != nil || len(devices) != 1 || devices[0].ID != second.ID {
		t.Fatalf("devices = %+v err=%v", devices, err)
	}
}

func approvalEvent(id, approvalID string) wire.Envelope {
	return wire.Envelope{
		V: wire.Version, Kind: wire.KindEvent, MsgID: "m_" + id, EventID: id,
		ApprovalID: approvalID, Type: wire.EventApprovalRequired, Agent: wire.AgentClaude,
		DaemonID: "dd_0123456789abcdef0123456789abcdef", SessionID: "sess_1",
		Payload: wire.Payload{ToolName: "Bash"}, TS: testNow,
	}
}

func TestInboxSeqAndDedup(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)

	env := approvalEvent("ev_1", "apv_1")
	first, dup, err := s.AppendInbox(daemonID, env, testNow)
	if err != nil || dup || first.Seq != 1 {
		t.Fatalf("first seq=%d dup=%v err=%v", first.Seq, dup, err)
	}
	again, dup, err := s.AppendInbox(daemonID, env, testNow)
	if err != nil || !dup || again.Seq != 1 {
		t.Fatalf("again seq=%d dup=%v err=%v", again.Seq, dup, err)
	}

	second, dup, err := s.AppendInbox(daemonID, approvalEvent("ev_2", "apv_2"), testNow)
	if err != nil || dup || second.Seq != 2 {
		t.Fatalf("second seq=%d dup=%v err=%v", second.Seq, dup, err)
	}

	events, next, err := s.InboxSince(daemonID, 0, 0, 1)
	if err != nil || len(events) != 1 || events[0].Seq != 1 || next != 1 {
		t.Fatalf("events=%+v next=%d err=%v", events, next, err)
	}
	events, next, err = s.InboxSince(daemonID, next, 0, 10)
	if err != nil || len(events) != 1 || events[0].Seq != 2 || next != 0 {
		t.Fatalf("第二页 events=%+v next=%d err=%v", events, next, err)
	}
}

func TestInboxTTLCleanup(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	if _, _, err := s.AppendInbox(daemonID, approvalEvent("ev_1", "apv_1"), testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Cleanup(testNow + int64(InboxTTL.Seconds()) + 1); err != nil {
		t.Fatal(err)
	}
	events, _, err := s.InboxSince(daemonID, 0, 0, 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("过期事件未清理: %+v err=%v", events, err)
	}
}

func TestDecisionFirstWinsAndExpiry(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	approvalID := "apv_1"
	event := approvalEvent("ev_1", approvalID)
	event.DaemonID = daemonID
	if _, _, err := s.AppendInbox(daemonID, event, testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.PutPending(Pending{
		ApprovalID: approvalID, DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 300,
	}); err != nil {
		t.Fatal(err)
	}

	status, p, err := s.ApplyDecision(approvalID, daemonID, "approved", testNow+1)
	if err != nil || status != wire.AckAccepted || p.Decision != "approved" {
		t.Fatalf("status=%v p=%+v err=%v", status, p, err)
	}
	status, _, err = s.ApplyDecision(approvalID, daemonID, "denied", testNow+2)
	if err != nil || status != wire.AckAlreadyDecided {
		t.Fatalf("重复决策 status=%v err=%v", status, err)
	}
	if status, _, _ := s.ApplyDecision(approvalID, "dd_other", "deny", testNow); status != wire.AckUnknown {
		t.Fatalf("跨 daemon 决策 status=%v", status)
	}
	if status, _, _ := s.ApplyDecision("apv_missing", daemonID, "deny", testNow); status != wire.AckUnknown {
		t.Fatalf("未知审批 status=%v", status)
	}

	// 过期审批：决策返回 expired。
	if err := s.PutPending(Pending{
		ApprovalID: "apv_old", DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 10,
	}); err != nil {
		t.Fatal(err)
	}
	status, _, err = s.ApplyDecision("apv_old", daemonID, "approved", testNow+11)
	if err != nil || status != wire.AckExpired {
		t.Fatalf("过期 status=%v err=%v", status, err)
	}
}

func TestUndeliveredDecisions(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	event := approvalEvent("ev_1", "apv_1")
	event.DaemonID = daemonID
	_ = s.PutPending(Pending{
		ApprovalID: "apv_1", DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 300,
	})
	if _, _, err := s.ApplyDecision("apv_1", daemonID, "approved", testNow+1); err != nil {
		t.Fatal(err)
	}
	items, err := s.UndeliveredDecisions(daemonID, testNow+2)
	if err != nil || len(items) != 1 || items[0].Decision != "approved" {
		t.Fatalf("items=%+v err=%v", items, err)
	}
	if err := s.MarkDecisionDelivered("apv_1"); err != nil {
		t.Fatal(err)
	}
	items, _ = s.UndeliveredDecisions(daemonID, testNow+3)
	if len(items) != 0 {
		t.Fatalf("已送达仍返回: %+v", items)
	}
}

func TestRestartPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	if _, _, err := s.AppendInbox(daemonID, approvalEvent("ev_1", "apv_1"), testNow); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.Daemon(daemonID); err != nil {
		t.Fatalf("重启后 daemon 丢失: %v", err)
	}
	events, _, err := s2.InboxSince(daemonID, 0, 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("重启后事件丢失: %+v err=%v", events, err)
	}
}
