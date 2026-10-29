// Package metrics 提供最小可用的 Prometheus 文本指标。
package metrics

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

// Metrics 收集 relay 的计数器。
type Metrics struct {
	events    sync.Map // type -> *atomic.Int64
	decisions sync.Map // decision|status -> *atomic.Int64
	pushes    sync.Map // kind|status -> *atomic.Int64
	quota     sync.Map // kind -> *atomic.Int64
	resyncs   atomic.Int64
	dedupHits atomic.Int64

	// Gauges 由 server 注入，避免 metrics 反向依赖 store。
	DaemonConns func() int
	AppConns    func() int
	Pending     func() int
	BansActive  func() int
}

// New 创建指标集合。
func New() *Metrics { return &Metrics{} }

// IncEvent 记录一条上行事件。
func (m *Metrics) IncEvent(t string) { inc(&m.events, t) }

// IncDecision 记录一次决策结果。
func (m *Metrics) IncDecision(decision, status string) {
	inc(&m.decisions, decision+"|"+status)
}

// IncResync 记录一次 resync。
func (m *Metrics) IncResync() { m.resyncs.Add(1) }

// IncDedup 记录一次 event_id dedup hits。
func (m *Metrics) IncDedup() { m.dedupHits.Add(1) }

// IncPush 记录一次 APNs delivery results（kind=alert|liveactivity，status=HTTP 状态或 error/skipped）。
func (m *Metrics) IncPush(kind, status string) {
	inc(&m.pushes, kind+"|"+status)
}

// IncQuotaRejected 记录一次被配额闸拒绝的请求
// (kind=rate_event|push_alert|push_liveactivity)。
// 不加 per-daemon label:基数风险,排障走结构化日志。
func (m *Metrics) IncQuotaRejected(kind string) { inc(&m.quota, kind) }

// Render 输出 Prometheus 文本格式。
func (m *Metrics) Render() string {
	var b strings.Builder
	b.WriteString("# HELP devpilot_events_total Ingested events\n# TYPE devpilot_events_total counter\n")
	writeCounters(&b, "devpilot_events_total", "type", &m.events)

	b.WriteString("# HELP devpilot_decisions_total Decisions\n# TYPE devpilot_decisions_total counter\n")
	writeDecisions(&b, &m.decisions)

	b.WriteString("# HELP devpilot_resync_total Resync count\n# TYPE devpilot_resync_total counter\n")
	fmt.Fprintf(&b, "devpilot_resync_total %d\n", m.resyncs.Load())

	b.WriteString("# HELP devpilot_dedup_hits_total event_id dedup hits\n# TYPE devpilot_dedup_hits_total counter\n")
	fmt.Fprintf(&b, "devpilot_dedup_hits_total %d\n", m.dedupHits.Load())

	b.WriteString("# HELP devpilot_apns_pushes_total APNs delivery results\n# TYPE devpilot_apns_pushes_total counter\n")
	writePushes(&b, &m.pushes)

	b.WriteString("# HELP devpilot_quota_rejected_total Requests rejected by quota\n# TYPE devpilot_quota_rejected_total counter\n")
	writeCounters(&b, "devpilot_quota_rejected_total", "kind", &m.quota)

	if m.DaemonConns != nil {
		b.WriteString("# HELP devpilot_daemon_connections Online daemon connections\n# TYPE devpilot_daemon_connections gauge\n")
		fmt.Fprintf(&b, "devpilot_daemon_connections %d\n", m.DaemonConns())
	}
	if m.AppConns != nil {
		b.WriteString("# HELP devpilot_app_connections Online app connections\n# TYPE devpilot_app_connections gauge\n")
		fmt.Fprintf(&b, "devpilot_app_connections %d\n", m.AppConns())
	}
	if m.Pending != nil {
		b.WriteString("# HELP devpilot_pending_approvals Pending approvals\n# TYPE devpilot_pending_approvals gauge\n")
		fmt.Fprintf(&b, "devpilot_pending_approvals %d\n", m.Pending())
	}
	if m.BansActive != nil {
		b.WriteString("# HELP devpilot_ban_active Currently banned IP aggregates\n# TYPE devpilot_ban_active gauge\n")
		fmt.Fprintf(&b, "devpilot_ban_active %d\n", m.BansActive())
	}
	return b.String()
}

func inc(m *sync.Map, key string) {
	v, ok := m.Load(key)
	if !ok {
		v, _ = m.LoadOrStore(key, new(atomic.Int64))
	}
	v.(*atomic.Int64).Add(1)
}

func writeCounters(b *strings.Builder, name, label string, m *sync.Map) {
	keys := collectKeys(m)
	for _, k := range keys {
		v, _ := m.Load(k)
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, v.(*atomic.Int64).Load())
	}
}

func writeDecisions(b *strings.Builder, m *sync.Map) {
	keys := collectKeys(m)
	for _, k := range keys {
		v, _ := m.Load(k)
		parts := strings.SplitN(k, "|", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(b, "devpilot_decisions_total{decision=%q,status=%q} %d\n",
			parts[0], parts[1], v.(*atomic.Int64).Load())
	}
}

func writePushes(b *strings.Builder, m *sync.Map) {
	keys := collectKeys(m)
	for _, k := range keys {
		v, _ := m.Load(k)
		parts := strings.SplitN(k, "|", 2)
		if len(parts) != 2 {
			continue
		}
		fmt.Fprintf(b, "devpilot_apns_pushes_total{kind=%q,status=%q} %d\n",
			parts[0], parts[1], v.(*atomic.Int64).Load())
	}
}

func collectKeys(m *sync.Map) []string {
	var keys []string
	m.Range(func(k, _ any) bool {
		keys = append(keys, k.(string))
		return true
	})
	sort.Strings(keys)
	return keys
}
