// Package quota 提供按 daemon 的双层配额:令牌桶事件速率 + UTC 日历日推送配额。
//
// 锚点:事件速率对齐 moshi(free 10/60s、Pro 60/60s),日配额对齐 Home Assistant /
// ntfy(free 500/天、Pro 5000/天)。纯内存态,重启即清——配额是防滥用软约束,
// 不值得为此引入持久化与恢复语义。任何阈值为 0(或窗口为 0)表示对应闸门关闭。
package quota

import (
	"sync"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// Config 是配额参数。
type Config struct {
	RateEventFree   int           // free 档每窗口事件数(桶容量=此值,即瞬时 burst 上限)
	RateEventPro    int           // pro 档每窗口事件数
	RateEventWindow time.Duration // 令牌桶补充窗口
	QuotaDailyFree  int64         // free 档 UTC 日历日推送上限
	QuotaDailyPro   int64         // pro 档 UTC 日历日推送上限
	// ExemptApprovals 时审批类事件(approval_required/task_complete)豁免速率闸:
	// 审批不能被普通遥测饿死;但日配额不豁免,那是防滥用硬上界。
	ExemptApprovals bool
	Now             func() time.Time
}

// bucket 是 lazy refill 令牌桶;容量由调用时档位决定,升档后一个窗口内自然补满。
type bucket struct {
	tokens float64
	last   time.Time
}

// daily 是 UTC 日历日计数;day = unix 秒 / 86400。
type daily struct {
	day   int64
	count int64
}

type daemonState struct {
	rate bucket
	day  daily
}

// Engine 是并发安全的配额引擎,以 daemonID 为 key。
type Engine struct {
	cfg Config
	now func() time.Time

	mu     sync.Mutex
	states map[string]*daemonState
}

// New 构造引擎。
func New(cfg Config) *Engine {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &Engine{cfg: cfg, now: cfg.Now, states: make(map[string]*daemonState)}
}

// AllowEvent 是事件速率闸(WSS onEvent 最前、AppendInbox 之前)。
// 返回 false 时调用方应回 AckRejected:被拒不烧 seq、不写 inbox、不扇出推送。
func (e *Engine) AllowEvent(tier store.Tier, daemonID string, eventType wire.EventType) bool {
	rateLimit, _ := e.limits(tier)
	if rateLimit <= 0 || e.cfg.RateEventWindow <= 0 {
		return true
	}
	if e.cfg.ExemptApprovals && exemptEvent(eventType) {
		return true
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.takeTokenLocked(e.stateLocked(daemonID), rateLimit, now)
}

// AllowPush 是 UTC 日配额闸(deliverer push() 头部,APNs 成本唯一漏斗)。
// 只拦 APNs 推送;WSS 实时/inbox/resync 永不受限,App 前台仍可审批(fail-safe)。
func (e *Engine) AllowPush(tier store.Tier, daemonID string) bool {
	_, dayLimit := e.limits(tier)
	if dayLimit <= 0 {
		return true
	}
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()
	st := e.stateLocked(daemonID)
	today := now.UTC().Unix() / 86400
	if st.day.day != today {
		st.day = daily{day: today}
	}
	if st.day.count >= dayLimit {
		return false
	}
	st.day.count++
	return true
}

// limits 返回档位对应参数;空档位(legacy 记录)按 free 处理。
func (e *Engine) limits(tier store.Tier) (rate int, day int64) {
	if tier == store.TierPro {
		return e.cfg.RateEventPro, e.cfg.QuotaDailyPro
	}
	return e.cfg.RateEventFree, e.cfg.QuotaDailyFree
}

// exemptEvent 审批类事件:用户在终端前等人点头,饿死它等于功能不可用。
// approval_closed 是审批终态广播(撤横幅/LA 收场),同样不能被饿死。
func exemptEvent(t wire.EventType) bool {
	return t == wire.EventApprovalRequired || t == wire.EventTaskComplete ||
		t == wire.EventApprovalClosed
}

// takeTokenLocked 消费一个令牌;按流逝时间线性补充,上限=容量。
func (e *Engine) takeTokenLocked(st *daemonState, limit int, now time.Time) bool {
	capacity := float64(limit)
	if st.rate.last.IsZero() {
		st.rate = bucket{tokens: capacity, last: now}
	} else if elapsed := now.Sub(st.rate.last); elapsed > 0 {
		refill := elapsed.Seconds() / e.cfg.RateEventWindow.Seconds() * capacity
		st.rate.tokens = min(capacity, st.rate.tokens+refill)
		st.rate.last = now
	}
	if st.rate.tokens < 1 {
		return false
	}
	st.rate.tokens--
	return true
}

// stateLocked 取(或建)daemon 状态;超阈值时顺手清理陈旧状态防 map 无限增长。
func (e *Engine) stateLocked(daemonID string) *daemonState {
	st, ok := e.states[daemonID]
	if !ok {
		st = &daemonState{}
		e.states[daemonID] = st
	}
	if len(e.states) > 10000 {
		e.pruneLocked(e.now().UTC().Unix() / 86400)
	}
	return st
}

// pruneLocked 删除速率桶已整体过期、且日计数已是昨天的状态——
// 两者同时满足说明该 daemon 两个维度下次触碰都会自然归零。
func (e *Engine) pruneLocked(today int64) {
	for id, st := range e.states {
		rateStale := st.rate.last.IsZero() || e.now().Sub(st.rate.last) > e.cfg.RateEventWindow
		if rateStale && st.day.day < today {
			delete(e.states, id)
		}
	}
}
