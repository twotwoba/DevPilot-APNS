package ratelimit

import (
	"testing"
	"time"
)

// fakeClock 单线程测试用假时钟。
type fakeClock struct{ t time.Time }

func (c *fakeClock) Now() time.Time      { return c.t }
func (c *fakeClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func newBan(t *testing.T, mutate func(*BanConfig)) (*BanManager, *fakeClock) {
	t.Helper()
	clk := &fakeClock{t: time.Unix(1_760_000_000, 0)}
	cfg := BanConfig{Threshold: 3, Window: 5 * time.Minute, TTL: 24 * time.Hour, Now: clk.Now}
	if mutate != nil {
		mutate(&cfg)
	}
	return NewBanManager(cfg), clk
}

func TestBanThresholdAndExpiry(t *testing.T) {
	b, clk := newBan(t, nil)
	for range 2 {
		b.RecordFailure("1.2.3.4")
	}
	if b.Banned("1.2.3.4") {
		t.Fatal("未达阈值不应封禁")
	}
	b.RecordFailure("1.2.3.4")
	if !b.Banned("1.2.3.4") {
		t.Fatal("达阈值应封禁")
	}
	if b.Active() != 1 {
		t.Fatalf("Active = %d", b.Active())
	}
	// TTL 内持续封禁;过期后解除。
	clk.Advance(23 * time.Hour)
	if !b.Banned("1.2.3.4") {
		t.Fatal("TTL 内应仍封禁")
	}
	clk.Advance(2 * time.Hour)
	if b.Banned("1.2.3.4") {
		t.Fatal("TTL 过期应解除")
	}
	if b.Active() != 0 {
		t.Fatalf("过期后 Active = %d", b.Active())
	}
}

func TestBanWindowReset(t *testing.T) {
	b, clk := newBan(t, nil)
	b.RecordFailure("1.2.3.4")
	b.RecordFailure("1.2.3.4")
	// 出窗后计数重置,不封禁。
	clk.Advance(6 * time.Minute)
	b.RecordFailure("1.2.3.4")
	if b.Banned("1.2.3.4") {
		t.Fatal("出窗重置后不应封禁")
	}
}

func TestBanDisabled(t *testing.T) {
	b, _ := newBan(t, func(c *BanConfig) { c.Threshold = 0 })
	b.RecordFailure("1.2.3.4")
	if b.Banned("1.2.3.4") || b.Enabled() {
		t.Fatal("阈值为 0 应禁用")
	}
	var nilBan *BanManager
	if nilBan.Enabled() || nilBan.Active() != 0 {
		t.Fatal("nil 管理器应安全禁用")
	}
}

func TestAggregateKey(t *testing.T) {
	cases := []struct {
		ip   string
		want string
	}{
		{"203.0.113.7", "203.0.113.7"},
		{"2001:db8:1a2b:3c4d:5e6f:0001:0002:0003", "2001:db8:1a2b:3c4d::"},
		{"not-an-ip", "not-an-ip"},
	}
	for _, c := range cases {
		if got := AggregateKey(c.ip); got != c.want {
			t.Fatalf("AggregateKey(%q) = %q, want %q", c.ip, got, c.want)
		}
	}
	// 同一 /64 段的两个地址应聚合到同一 key。
	a := AggregateKey("2001:db8:1a2b:3c4d::1")
	b := AggregateKey("2001:db8:1a2b:3c4d:ffff:ffff:ffff:ffff")
	if a != b {
		t.Fatalf("同 /64 段应同 key: %q vs %q", a, b)
	}
	// 不同 /64 段应不同 key。
	if AggregateKey("2001:db8:1a2b:3c4e::1") == a {
		t.Fatal("不同 /64 段不应同 key")
	}
}
