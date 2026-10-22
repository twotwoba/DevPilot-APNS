// Package ratelimit 提供按 key 的固定窗口限流（配对接口防爆破）。
package ratelimit

import (
	"sync"
	"time"
)

type entry struct {
	count int
	start time.Time
}

// Limiter 是并发安全的固定窗口计数器。
type Limiter struct {
	limit  int
	window time.Duration
	now    func() time.Time

	mu      sync.Mutex
	entries map[string]entry
}

// New 创建限流器；limit 为窗口内允许次数。
func New(limit int, window time.Duration, now func() time.Time) *Limiter {
	if now == nil {
		now = time.Now
	}
	return &Limiter{limit: limit, window: window, now: now, entries: make(map[string]entry)}
}

// Allow 判断 key 本次是否放行。
func (l *Limiter) Allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[key]
	if !ok || now.Sub(e.start) >= l.window {
		l.entries[key] = entry{count: 1, start: now}
		l.pruneLocked(now)
		return true
	}
	if e.count >= l.limit {
		return false
	}
	e.count++
	l.entries[key] = e
	return true
}

// pruneLocked 防止 map 无限增长。
func (l *Limiter) pruneLocked(now time.Time) {
	if len(l.entries) < 10000 {
		return
	}
	for k, e := range l.entries {
		if now.Sub(e.start) >= l.window {
			delete(l.entries, k)
		}
	}
}
