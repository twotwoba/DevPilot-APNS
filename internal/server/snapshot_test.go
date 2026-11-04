package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"

	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
)

// TestSnapshotDisabledWithoutToken METRICS_TOKEN 未设时端点整体禁用:
// 快照触发磁盘写,不允许匿名(或裸奔部署)触达。
func TestSnapshotDisabledWithoutToken(t *testing.T) {
	env := newTestEnv(t) // MetricsToken 空
	resp, body := env.post(t, "/admin/snapshot", nil, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("no token configured should 503: %d %s", resp.StatusCode, body)
	}
}

// TestSnapshotAuthRequired token 已设时缺/错 Bearer 必须 401。
func TestSnapshotAuthRequired(t *testing.T) {
	env := newTestEnvOpts(t, testEnvOpts{MetricsToken: "s3cret"})
	resp, _ := env.post(t, "/admin/snapshot", nil, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing token should 401: %d", resp.StatusCode)
	}
	resp, _ = env.post(t, "/admin/snapshot", nil, "wrong-token")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token should 401: %d", resp.StatusCode)
	}
}

// TestSnapshotWritesValidDatabase 正确 token → 201 + 数据库同级 snapshots/
// 落盘 + 重开快照文件可读(meta bucket 在),响应带 path/bytes/duration_ms。
func TestSnapshotWritesValidDatabase(t *testing.T) {
	env := newTestEnvOpts(t, testEnvOpts{MetricsToken: "s3cret"})
	env.registerAndPair(t) // 库里先有点状态

	resp, body := env.post(t, "/admin/snapshot", nil, "s3cret")
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("valid token should 201: %d %s", resp.StatusCode, body)
	}
	var out struct {
		Path       string `json:"path"`
		Bytes      int64  `json:"bytes"`
		DurationMS int64  `json:"duration_ms"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if out.Path == "" || out.Bytes <= 0 || out.DurationMS < 0 {
		t.Fatalf("response missing fields: %s", body)
	}
	if filepath.Base(filepath.Dir(out.Path)) != "snapshots" || !strings.HasPrefix(filepath.Base(out.Path), "relay-") {
		t.Fatalf("snapshot path should live in <dbdir>/snapshots/relay-*.db: %s", out.Path)
	}
	// 独立进程视角重开:证明是合法 bbolt 文件且数据可读(备份的唯一验收标准)。
	db, err := bolt.Open(out.Path, 0o400, &bolt.Options{Timeout: 2 * time.Second, ReadOnly: true})
	if err != nil {
		t.Fatalf("snapshot file is not a valid bbolt db: %v", err)
	}
	defer db.Close()
	err = db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("meta")) == nil {
			return errors.New("snapshot: meta bucket missing")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("snapshot missing meta bucket: %v", err)
	}
}

// TestSnapshotConcurrent409 防重入:锁被持有时并发触发直接 409,不排队。
func TestSnapshotConcurrent409(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	m := metrics.New()
	hub := wss.New(wss.Config{Store: st, Metrics: m})
	srv, err := New(Config{Store: st, Hub: hub, Metrics: m, MetricsToken: "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	srv.snapshotMu.Lock() // 模拟另一个快照进行中
	defer srv.snapshotMu.Unlock()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/admin/snapshot", nil)
	req.Header.Set("Authorization", "Bearer s3cret")
	srv.handleAdminSnapshot(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent snapshot should 409: %d", rec.Code)
	}
}
