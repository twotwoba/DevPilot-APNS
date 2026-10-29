package apns

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// captured 是一次假 APNs 请求的快照。
type captured struct {
	auth       string
	topic      string
	pushType   string
	priority   string
	expiration string
	collapseID string
	body       []byte
	protoMajor int
}

// fakeAPNs 启动一个启用 HTTP/2 的 TLS 假服务器，并记录请求。
func fakeAPNs(t *testing.T, handle func(w http.ResponseWriter, r *http.Request, n int)) (*httptest.Server, func() []captured) {
	t.Helper()
	var (
		mu     sync.Mutex
		got    []captured
		serial int
	)
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		serial++
		n := serial
		got = append(got, captured{
			auth:       r.Header.Get("authorization"),
			topic:      r.Header.Get("apns-topic"),
			pushType:   r.Header.Get("apns-push-type"),
			priority:   r.Header.Get("apns-priority"),
			expiration: r.Header.Get("apns-expiration"),
			collapseID: r.Header.Get("apns-collapse-id"),
			body:       body,
			protoMajor: r.ProtoMajor,
		})
		mu.Unlock()
		if handle != nil {
			handle(w, r, n)
			return
		}
		w.Header().Set("apns-id", "test-id")
		w.WriteHeader(http.StatusOK)
	}))
	ts.EnableHTTP2 = true
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts, func() []captured {
		mu.Lock()
		defer mu.Unlock()
		out := make([]captured, len(got))
		copy(out, got)
		return out
	}
}

// testKey 生成一份 PKCS8 编码的 P-256 私钥文件（与 Apple .p8 格式一致）。
func testKey(t *testing.T) (string, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "apns.p8")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, key
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestClient(t *testing.T, ts *httptest.Server, clock *fakeClock) (*Client, *ecdsa.PrivateKey) {
	t.Helper()
	keyPath, key := testKey(t)
	c, err := New(Config{
		KeyPath: keyPath, KeyID: "KEY123", TeamID: "TEAM123",
		BundleID:          "com.twotwoba.devpilot",
		LiveActivityTopic: "com.twotwoba.devpilot.push-type.liveactivity",
		Environment:       "sandbox",
		Endpoint:          ts.URL,
		HTTPClient:        ts.Client(),
		RetryBase:         time.Millisecond,
		BackoffBase:       time.Millisecond,
		Now:               clock.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	return c, key
}

func TestSendSuccessHeadersAndJWT(t *testing.T) {
	ts, calls := fakeAPNs(t, nil)
	clock := &fakeClock{t: time.Unix(1_760_000_000, 0)}
	c, key := newTestClient(t, ts, clock)

	req := Request{
		DeviceToken: "abc123", Topic: "com.twotwoba.devpilot", PushType: "alert",
		Priority: 10, Expiration: 1_760_000_120, CollapseID: "apv_1",
		Payload: map[string]any{"aps": map[string]any{"alert": "hi"}},
	}
	res, err := c.Send(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted() || res.APNSID != "test-id" {
		t.Fatalf("res = %+v", res)
	}
	got := calls()
	if len(got) != 1 {
		t.Fatalf("请求数 = %d", len(got))
	}
	g := got[0]
	if g.protoMajor != 2 {
		t.Fatalf("must use HTTP/2, got HTTP/%d", g.protoMajor)
	}
	if g.topic != req.Topic || g.pushType != "alert" || g.priority != "10" ||
		g.expiration != "1760000120" || g.collapseID != "apv_1" {
		t.Fatalf("request headers mismatch: %+v", g)
	}
	if !strings.HasPrefix(g.auth, "bearer ") {
		t.Fatalf("authorization = %q", g.auth)
	}

	// 校验 JWT：kid 与 iss。
	parsed, err := jwt.Parse(strings.TrimPrefix(g.auth, "bearer "), func(tok *jwt.Token) (any, error) {
		if tok.Method != jwt.SigningMethodES256 {
			return nil, errors.New("not ES256")
		}
		return &key.PublicKey, nil
	})
	if err != nil {
		t.Fatalf("JWT verification failed: %v", err)
	}
	if kid, _ := parsed.Header["kid"].(string); kid != "KEY123" {
		t.Fatalf("kid = %v", parsed.Header["kid"])
	}
	if iss, _ := parsed.Claims.GetIssuer(); iss != "TEAM123" {
		t.Fatalf("iss = %q", iss)
	}

	// 50 分钟内复用同一 JWT。
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	clock.Advance(51 * time.Minute)
	if _, err := c.Send(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	all := calls()
	if len(all) != 3 || all[0].auth != all[1].auth {
		t.Fatal("JWT cache miss")
	}
	if all[2].auth == all[0].auth {
		t.Fatal("JWT not refreshed after 50 minutes")
	}

	// 载荷在假服务器端可解析。
	var body map[string]any
	if err := json.Unmarshal(all[0].body, &body); err != nil {
		t.Fatal(err)
	}
	if body["aps"] == nil {
		t.Fatalf("body = %s", all[0].body)
	}
}

func TestErrorClassification(t *testing.T) {
	respond := func(status int, reason string) func(http.ResponseWriter, *http.Request, int) {
		return func(w http.ResponseWriter, _ *http.Request, _ int) {
			w.WriteHeader(status)
			if reason != "" {
				_ = json.NewEncoder(w).Encode(map[string]string{"reason": reason})
			}
		}
	}

	t.Run("410 触发剪枝且不重试", func(t *testing.T) {
		ts, calls := fakeAPNs(t, respond(http.StatusGone, "Unregistered"))
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{"aps": map[string]any{}}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Prune || res.Reason != "Unregistered" {
			t.Fatalf("res = %+v", res)
		}
		if len(calls()) != 1 {
			t.Fatal("410 should not retry")
		}
	})

	t.Run("400 丢弃且不重试", func(t *testing.T) {
		ts, calls := fakeAPNs(t, respond(http.StatusBadRequest, "BadDeviceToken"))
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusBadRequest || res.Reason != "BadDeviceToken" {
			t.Fatalf("res = %+v", res)
		}
		if len(calls()) != 1 {
			t.Fatal("400 should not retry")
		}
	})

	t.Run("403 不重试", func(t *testing.T) {
		ts, calls := fakeAPNs(t, respond(http.StatusForbidden, "InvalidProviderToken"))
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusForbidden || len(calls()) != 1 {
			t.Fatalf("res = %+v calls=%d", res, len(calls()))
		}
	})

	t.Run("5xx 重试三次", func(t *testing.T) {
		ts, calls := fakeAPNs(t, respond(http.StatusInternalServerError, "InternalServerError"))
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if res.Status != http.StatusInternalServerError || len(calls()) != maxAttempts {
			t.Fatalf("res = %+v calls=%d", res, len(calls()))
		}
	})

	t.Run("429 后重试成功", func(t *testing.T) {
		ts, calls := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request, n int) {
			if n < 3 {
				w.WriteHeader(http.StatusTooManyRequests)
				_ = json.NewEncoder(w).Encode(map[string]string{"reason": "TooManyRequests"})
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Accepted() || len(calls()) != 3 {
			t.Fatalf("res = %+v calls=%d", res, len(calls()))
		}
	})

	t.Run("ExpiredProviderToken 换新 token 重试", func(t *testing.T) {
		ts, calls := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request, n int) {
			if n == 1 {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]string{"reason": "ExpiredProviderToken"})
				return
			}
			w.WriteHeader(http.StatusOK)
		})
		c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
		res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
		if err != nil || !res.Accepted() {
			t.Fatalf("res=%+v err=%v", res, err)
		}
		all := calls()
		if len(all) != 2 || all[0].auth == all[1].auth {
			t.Fatal("JWT not force-refreshed")
		}
	})
}

func TestTopicBackoffSkip(t *testing.T) {
	ts, calls := fakeAPNs(t, func(w http.ResponseWriter, _ *http.Request, _ int) {
		w.WriteHeader(http.StatusTooManyRequests)
	})
	c, _ := newTestClient(t, ts, &fakeClock{t: time.Unix(1_760_000_000, 0)})
	c.cfg.BackoffBase = 100 * time.Millisecond

	res, err := c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
	if err != nil || res.Status != http.StatusTooManyRequests {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(calls()) != maxAttempts {
		t.Fatalf("calls = %d", len(calls()))
	}
	// 退避窗口内不再发出请求。
	res, err = c.Send(context.Background(), Request{DeviceToken: "x", Topic: "t", Payload: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || len(calls()) != maxAttempts {
		t.Fatalf("res=%+v calls=%d", res, len(calls()))
	}
}

func TestPayloadTooLarge(t *testing.T) {
	c, err := New(Config{Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Send(context.Background(), Request{
		DeviceToken: "x", Topic: "t",
		Payload: map[string]any{"aps": map[string]any{"alert": strings.Repeat("x", 5000)}},
	})
	if !errors.Is(err, ErrPayloadTooLarge) {
		t.Fatalf("err = %v", err)
	}
}

func TestMockRecordsCalls(t *testing.T) {
	c, err := New(Config{Mock: true})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		res, err := c.Send(context.Background(), Request{
			DeviceToken: "tok", Topic: "com.twotwoba.devpilot",
			PushType: "liveactivity", Priority: 5,
			Payload: map[string]any{"aps": map[string]any{"event": "update"}},
		})
		if err != nil || !res.Accepted() {
			t.Fatalf("res=%+v err=%v", res, err)
		}
	}
	calls := c.Calls()
	if len(calls) != 2 || calls[0].Request.PushType != "liveactivity" ||
		!strings.Contains(string(calls[0].Payload), "update") {
		t.Fatalf("calls = %+v", calls)
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Environment: "staging"}); err == nil {
		t.Fatal("unknown environment should error")
	}
	if _, err := New(Config{Environment: "sandbox", KeyID: "k", TeamID: "t"}); err == nil {
		t.Fatal("missing KeyPath should error")
	}
	path := filepath.Join(t.TempDir(), "bad.p8")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(Config{KeyPath: path, KeyID: "k", TeamID: "t"}); err == nil {
		t.Fatal("invalid .p8 should error")
	}
}

func TestTruncateBytes(t *testing.T) {
	if got := truncateBytes("abcdef", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := truncateBytes("你好世界", 4); got != "你" {
		t.Fatalf("UTF-8 boundary truncation wrong: %q", got)
	}
}
