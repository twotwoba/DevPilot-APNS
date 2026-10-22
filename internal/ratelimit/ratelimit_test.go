package ratelimit

import (
	"testing"
	"time"
)

func TestAllowWithinWindow(t *testing.T) {
	now := time.Unix(1000, 0)
	l := New(2, time.Minute, func() time.Time { return now })
	if !l.Allow("a") {
		t.Fatal("第一次应放行")
	}
	if !l.Allow("a") {
		t.Fatal("第二次应放行")
	}
	if l.Allow("a") {
		t.Fatal("第三次应拒绝")
	}
	if !l.Allow("b") {
		t.Fatal("不同 key 互不影响")
	}
	now = now.Add(time.Minute)
	if !l.Allow("a") {
		t.Fatal("新窗口应重置")
	}
}
