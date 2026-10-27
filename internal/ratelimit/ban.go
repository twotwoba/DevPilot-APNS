package ratelimit

import (
	"net"
	"sync"
	"time"
)

// BanConfig 是封禁参数;任一为零表示禁用。
type BanConfig struct {
	Threshold int           // 窗口内失败次数阈值(4xx/5xx)
	Window    time.Duration // 失败计数窗口
	TTL       time.Duration // 封禁时长
	Now       func() time.Time
}

type banState struct {
	failures    int
	windowStart time.Time
	bannedUntil time.Time
}

// BanManager 是内存版 fail2ban:窗口内累计 N 次失败即封禁 TTL。
// 纯内存,重启即清;锚点 Bark「5min 1000 次失败 → 封 24h」。
type BanManager struct {
	cfg    BanConfig
	now    func() time.Time
	mu     sync.Mutex
	states map[string]*banState
}

// NewBanManager 构造封禁管理器。
func NewBanManager(cfg BanConfig) *BanManager {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &BanManager{cfg: cfg, now: cfg.Now, states: make(map[string]*banState)}
}

// Enabled 报告封禁是否启用(对 nil 接收者安全)。
func (b *BanManager) Enabled() bool {
	return b != nil && b.cfg.Threshold > 0 && b.cfg.Window > 0 && b.cfg.TTL > 0
}

// Banned 报告 key 是否处于封禁期;过期/出窗状态顺手清理。
func (b *BanManager) Banned(key string) bool {
	if !b.Enabled() {
		return false
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[key]
	if !ok {
		return false
	}
	if !st.bannedUntil.IsZero() {
		if now.Before(st.bannedUntil) {
			return true
		}
		delete(b.states, key)
		return false
	}
	if now.Sub(st.windowStart) >= b.cfg.Window {
		delete(b.states, key)
	}
	return false
}

// RecordFailure 记一次失败;窗口内累计达阈值即进入封禁。
func (b *BanManager) RecordFailure(key string) {
	if !b.Enabled() {
		return
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	st, ok := b.states[key]
	if !ok || now.Sub(st.windowStart) >= b.cfg.Window {
		st = &banState{failures: 1, windowStart: now}
		b.states[key] = st
	} else {
		st.failures++
	}
	if st.failures >= b.cfg.Threshold {
		st.bannedUntil = now.Add(b.cfg.TTL)
	}
	if len(b.states) > 10000 {
		b.pruneLocked(now)
	}
}

// Active 返回当前封禁中的 key 数(指标用)。
func (b *BanManager) Active() int {
	if !b.Enabled() {
		return 0
	}
	now := b.now()
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, st := range b.states {
		if !st.bannedUntil.IsZero() && now.Before(st.bannedUntil) {
			n++
		}
	}
	return n
}

func (b *BanManager) pruneLocked(now time.Time) {
	for k, st := range b.states {
		switch {
		case !st.bannedUntil.IsZero() && now.After(st.bannedUntil):
			delete(b.states, k)
		case st.bannedUntil.IsZero() && now.Sub(st.windowStart) >= b.cfg.Window:
			delete(b.states, k)
		}
	}
}

// AggregateKey 把 IP 聚合为封禁 key:IPv4 保留整段(/32),
// IPv6 聚合到 /64——同一拨号段内轮转地址无法绕过。
func AggregateKey(ipStr string) string {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return ipStr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String()
}
