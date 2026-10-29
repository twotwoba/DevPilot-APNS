package store

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

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
	return pairDeviceTier(t, s, code, TierFree)
}

func pairDeviceTier(t *testing.T, s *Store, code string, tier Tier) (Daemon, Device) {
	t.Helper()
	secret, err := pairing.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	d := Device{ID: pairing.NewDeviceID(), CredHash: pairing.SecretHash(secret), Name: "iPhone"}
	daemon, device, err := s.Pair(pairing.CodeHash(code), d, tier, testNow)
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
	if _, _, err := s.Pair(pairing.CodeHash(code), device, TierFree, testNow); err != ErrCodeInvalid {
		t.Fatalf("重复使用配对码 err = %v", err)
	}
}

func TestPairingExpired(t *testing.T) {
	s := newStore(t)
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	seedPairing(t, s, code, "dd_0123456789abcdef0123456789abcdef")
	if _, _, err := s.Pair(pairing.CodeHash(code), Device{ID: "dv_x", CredHash: "h"}, TierFree, testNow+901); err != ErrCodeExpired {
		t.Fatalf("err = %v", err)
	}
}

func TestPairReplacesOldDevice(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	_, first := pairDevice(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")

	// 旧设备的 LA token 也必须在重配对时级联清除。
	if err := s.PutLAToken(LAToken{DeviceID: first.ID, DaemonID: daemonID,
		SessionID: "sess_1", PushToken: "tok", UpdatedAt: testNow}); err != nil {
		t.Fatal(err)
	}

	seedPairing(t, s, "ZZZZZZZZZZZZZZZZZZZZZZZZZZ", daemonID)
	_, second := pairDevice(t, s, "ZZZZZZZZZZZZZZZZZZZZZZZZZZ")

	if _, err := s.Device(first.ID); err != ErrNotFound {
		t.Fatalf("旧设备未删除: %v", err)
	}
	if toks, err := s.LATokensForDaemon(daemonID); err != nil || len(toks) != 0 {
		t.Fatalf("旧设备 LA token 应级联清除: %+v err=%v", toks, err)
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

func TestParseTier(t *testing.T) {
	if tier, ok := ParseTier(""); !ok || tier != TierFree {
		t.Fatalf("空值应为 free: %q %v", tier, ok)
	}
	if tier, ok := ParseTier(" PRO "); !ok || tier != TierPro {
		t.Fatalf("大小写与空白应容忍: %q %v", tier, ok)
	}
	if _, ok := ParseTier("enterprise"); ok {
		t.Fatal("未识别档位应拒绝")
	}
}

// TestMigrateV1ToV2 构造 v1 形态的库(版本=1、Daemon 无新字段),
// 验证 Open 时回填 Tier 与 LastSeenAt,且二次打开幂等。
func TestMigrateV1ToV2(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	createdAt := int64(1_700_000_000)
	if err := db.Update(func(tx *bolt.Tx) error {
		for _, name := range []string{"meta", "daemons", "devices", "pending", "inbox", "event_ids", "la_tokens", "pairing_codes"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], 1)
		if err := tx.Bucket([]byte("meta")).Put([]byte("schema_version"), buf[:]); err != nil {
			return err
		}
		old := fmt.Appendf(nil, `{"id":"dd_old000000","name":"legacy","cred_hash":"h","created_at":%d,"last_seq":3}`, createdAt)
		return tx.Bucket([]byte("daemons")).Put([]byte("dd_old000000"), old)
	}); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Daemon("dd_old000000")
	if err != nil {
		t.Fatal(err)
	}
	if d.Tier != TierFree || d.LastSeenAt != createdAt {
		t.Fatalf("迁移回填失败: tier=%q last_seen=%d want free/%d", d.Tier, d.LastSeenAt, createdAt)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// 二次打开幂等。
	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if d, err = s2.Daemon("dd_old000000"); err != nil || d.LastSeenAt != createdAt {
		t.Fatalf("二次打开应幂等: %+v %v", d, err)
	}
}

func TestPairWritesTierAndTouchDaemon(t *testing.T) {
	s := newStore(t)
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	seedPairing(t, s, code, "dd_0123456789abcdef0123456789abcdef")
	daemon, _ := pairDeviceTier(t, s, code, TierPro)
	if daemon.Tier != TierPro {
		t.Fatalf("档位应写入 daemon: %q", daemon.Tier)
	}
	d, err := s.Daemon(daemon.ID)
	if err != nil || d.Tier != TierPro {
		t.Fatalf("档位未落盘: %+v %v", d, err)
	}
	if err := s.TouchDaemon(daemon.ID, testNow+100); err != nil {
		t.Fatal(err)
	}
	if d, _ = s.Daemon(daemon.ID); d.LastSeenAt != testNow+100 {
		t.Fatalf("LastSeenAt 未刷新: %d", d.LastSeenAt)
	}
}

func TestUpdateDeviceAPNSToken(t *testing.T) {
	s := newStore(t)
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	seedPairing(t, s, code, "dd_0123456789abcdef0123456789abcdef")
	_, device := pairDevice(t, s, code)
	newToken := strings.Repeat("a", 64)
	if err := s.UpdateDeviceAPNSToken(device.ID, newToken, testNow+50); err != nil {
		t.Fatal(err)
	}
	d, err := s.Device(device.ID)
	if err != nil || d.APNSToken != newToken || d.LastSeenAt != testNow+50 {
		t.Fatalf("token 未更新: %+v %v", d, err)
	}
}

func TestDeleteDeviceRemovesLATokens(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	seedPairing(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", daemonID)
	_, device := pairDevice(t, s, "ABCDEFGHIJKLMNOPQRSTUVWXYZ")
	if err := s.PutLAToken(LAToken{
		DeviceID: device.ID, DaemonID: daemonID, SessionID: "sess_1",
		PushToken: "tok", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteDevice(device.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Device(device.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("device 应已删除: %v", err)
	}
	if toks, _ := s.LATokensForDaemon(daemonID); len(toks) != 0 {
		t.Fatalf("LA token 应级联删除: %d", len(toks))
	}
}

func TestDeleteDaemonCascade(t *testing.T) {
	s := newStore(t)
	daemonID := "dd_0123456789abcdef0123456789abcdef"
	code := "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
	seedPairing(t, s, code, daemonID)
	_, device := pairDevice(t, s, code)
	if _, _, err := s.AppendInbox(daemonID, approvalEvent("ev_1", "apv_1"), testNow); err != nil {
		t.Fatal(err)
	}
	event := approvalEvent("ev_1", "apv_1")
	if err := s.PutPending(Pending{
		ApprovalID: "apv_1", DaemonID: daemonID, Event: event, Status: "pending",
		CreatedAt: testNow, ExpiresAt: testNow + 300,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.PutLAToken(LAToken{
		DeviceID: device.ID, DaemonID: daemonID, SessionID: "sess_1",
		PushToken: "tok", UpdatedAt: testNow,
	}); err != nil {
		t.Fatal(err)
	}

	if err := s.DeleteDaemonCascade(daemonID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Daemon(daemonID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("daemon 应已删除: %v", err)
	}
	if _, err := s.Device(device.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("device 应级联删除: %v", err)
	}
	if toks, _ := s.LATokensForDaemon(daemonID); len(toks) != 0 {
		t.Fatalf("LA token 应级联删除: %d", len(toks))
	}
	if events, _, _ := s.InboxSince(daemonID, 0, 0, 10); len(events) != 0 {
		t.Fatalf("收件箱应级联删除: %+v", events)
	}
	if items, _ := s.PendingForDaemon(daemonID, testNow); len(items) != 0 {
		t.Fatalf("pending 应级联删除: %+v", items)
	}
	if _, err := s.GetPending("apv_1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pending 记录应级联删除: %v", err)
	}
}

func TestCleanupIdentities(t *testing.T) {
	s := newStore(t)
	staleID := "dd_stale0000000000000000000000000"
	freshID := "dd_fresh0000000000000000000000000"
	seedPairing(t, s, "ABCDEFGHIJKLMNOP", staleID)
	seedPairing(t, s, "QRSTUVWXYZ234567", freshID)
	_, staleDev := pairDevice(t, s, "ABCDEFGHIJKLMNOP")

	day := int64(86400)
	fresh := testNow + 31*day
	if err := s.TouchDaemon(freshID, fresh); err != nil {
		t.Fatal(err)
	}

	// 30 天 TTL:stale 距建号 31 天被清,fresh 刚续期保留;device 维度禁用。
	removedDaemons, removedDevices, err := s.CleanupIdentities(fresh, 30*24*time.Hour, 0)
	if err != nil {
		t.Fatal(err)
	}
	if removedDaemons != 1 || removedDevices != 0 {
		t.Fatalf("removed=(%d,%d) want (1,0)", removedDaemons, removedDevices)
	}
	if _, err := s.Daemon(staleID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超期 daemon 应被清理: %v", err)
	}
	if _, err := s.Device(staleDev.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("超期 daemon 的 device 应级联删除: %v", err)
	}
	if _, err := s.Daemon(freshID); err != nil {
		t.Fatalf("活跃 daemon 不应被清理: %v", err)
	}

	// TTL=0 全禁用,零操作。
	if n, m, err := s.CleanupIdentities(fresh+10*day, 0, 0); err != nil || n != 0 || m != 0 {
		t.Fatalf("禁用时应零操作: (%d,%d) %v", n, m, err)
	}
}
