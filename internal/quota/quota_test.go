package quota

import (
	"sync"
	"testing"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// testClock 单线程测试用的假时钟。
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

const daemonID = "dd_0123456789abcdef0123456789abcdef"

func newEngine(t *testing.T, mutate func(*Config)) (*Engine, *testClock) {
	t.Helper()
	clk := &testClock{t: time.Unix(1_760_000_000, 0)}
	cfg := Config{
		RateEventFree:   3,
		RateEventPro:    6,
		RateEventWindow: time.Minute,
		QuotaDailyFree:  5,
		QuotaDailyPro:   10,
		Now:             clk.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	return New(cfg), clk
}

func TestEventRateLimit(t *testing.T) {
	e, clk := newEngine(t, nil)
	for range 3 {
		if !e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
			t.Fatal("窗口内前 3 个事件应放行")
		}
	}
	if e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
		t.Fatal("第 4 个事件应被速率闸拒绝")
	}
	// 一个窗口后线性补充,应再次放行。
	clk.Advance(time.Minute)
	if !e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
		t.Fatal("窗口滚过后应放行")
	}
}

func TestApprovalExemptFromRate(t *testing.T) {
	e, _ := newEngine(t, func(c *Config) { c.ExemptApprovals = true })
	for range 3 {
		e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning)
	}
	if e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
		t.Fatal("普通遥测应被限流")
	}
	// 审批类豁免速率闸,且豁免不消耗令牌。
	for range 5 {
		if !e.AllowEvent(store.TierFree, daemonID, wire.EventApprovalRequired) {
			t.Fatal("审批事件不应被速率闸拦截")
		}
	}
	if !e.AllowEvent(store.TierFree, daemonID, wire.EventTaskComplete) {
		t.Fatal("任务完成事件不应被速率闸拦截")
	}
	if e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
		t.Fatal("豁免事件不该消耗令牌,普通事件仍应被拒")
	}
}

func TestApprovalNotExemptWhenDisabled(t *testing.T) {
	e, _ := newEngine(t, nil) // ExemptApprovals 缺省 false
	for range 3 {
		e.AllowEvent(store.TierFree, daemonID, wire.EventApprovalRequired)
	}
	if e.AllowEvent(store.TierFree, daemonID, wire.EventApprovalRequired) {
		t.Fatal("未开启豁免时审批也应被限流")
	}
}

func TestTierLimitsAndEmptyTier(t *testing.T) {
	e, _ := newEngine(t, func(c *Config) { c.ExemptApprovals = true })
	// pro 档 6 个;free 档第 4 个就拒。
	for range 6 {
		if !e.AllowEvent(store.TierPro, daemonID, wire.EventToolRunning) {
			t.Fatal("pro 档前 6 个应放行")
		}
	}
	if e.AllowEvent(store.TierPro, daemonID, wire.EventToolRunning) {
		t.Fatal("pro 档第 7 个应拒绝")
	}
	// 空档位(legacy)按 free。
	if e.AllowEvent(store.Tier(""), daemonID, wire.EventToolRunning) {
		t.Fatal("空档位应按 free 限额(此 daemon 的 free 桶已耗尽)")
	}
}

func TestDailyPushQuota(t *testing.T) {
	e, clk := newEngine(t, nil)
	for range 5 {
		if !e.AllowPush(store.TierFree, daemonID) {
			t.Fatal("日内前 5 次推送应放行")
		}
	}
	if e.AllowPush(store.TierFree, daemonID) {
		t.Fatal("第 6 次推送应被日配额拒绝")
	}
	// 同日推进 1 小时仍拒;跨过 UTC 零点后重置。
	clk.Advance(time.Hour)
	if e.AllowPush(store.TierFree, daemonID) {
		t.Fatal("同一 UTC 日内不应重置")
	}
	now := clk.Now()
	nextMidnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	clk.Advance(nextMidnight.Sub(now))
	if !e.AllowPush(store.TierFree, daemonID) {
		t.Fatal("跨 UTC 零点后应重置放行")
	}
}

func TestDailyQuotaPerTierSharedState(t *testing.T) {
	e, _ := newEngine(t, nil)
	// free 打满后升 pro:计数按 daemon 累计,不因升档清零。
	for range 5 {
		e.AllowPush(store.TierFree, daemonID)
	}
	if !e.AllowPush(store.TierPro, daemonID) {
		t.Fatal("pro 档日配额 10,应还能推 5 次")
	}
	for range 5 {
		e.AllowPush(store.TierPro, daemonID)
	}
	if e.AllowPush(store.TierPro, daemonID) {
		t.Fatal("pro 档第 11 次应拒绝")
	}
}

func TestDisabledGates(t *testing.T) {
	e, _ := newEngine(t, func(c *Config) {
		c.RateEventFree, c.RateEventPro = 0, 0
		c.QuotaDailyFree, c.QuotaDailyPro = 0, 0
	})
	for range 100 {
		if !e.AllowEvent(store.TierFree, daemonID, wire.EventToolRunning) {
			t.Fatal("速率闸关闭时应全放行")
		}
		if !e.AllowPush(store.TierPro, daemonID) {
			t.Fatal("日配额关闭时应全放行")
		}
	}
}

func TestPruneStaleStates(t *testing.T) {
	e, clk := newEngine(t, func(c *Config) { c.QuotaDailyFree = 1 })
	today := clk.Now().UTC().Unix() / 86400
	// 直接构造两类状态:昨日推送残留(应清)、今日活跃(应留)。
	e.mu.Lock()
	e.states["dd_stale00000000000000000000000"] = &daemonState{day: daily{day: today - 2}}
	e.states["dd_fresh00000000000000000000000"] = &daemonState{
		rate: bucket{tokens: 3, last: clk.Now()}, day: daily{day: today},
	}
	e.pruneLocked(today)
	_, staleKept := e.states["dd_stale00000000000000000000000"]
	_, freshKept := e.states["dd_fresh00000000000000000000000"]
	e.mu.Unlock()
	if staleKept {
		t.Fatal("昨日残留状态应被清理")
	}
	if !freshKept {
		t.Fatal("今日活跃状态不应被清理")
	}
}
