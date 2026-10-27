package wss

import (
	"context"
	"strconv"

	"github.com/twotwoba/DevPilot-APNS/internal/id"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// onEvent 处理 daemon 上行事件：分配 seq、落库、路由、回 ack。
func (h *Hub) onEvent(ctx context.Context, c *Conn, env *wire.Envelope) {
	if c.Role != wire.RoleDaemon || env.DaemonID != c.DaemonID {
		h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckRejected)
		return
	}
	// 速率闸最前、AppendInbox 之前：被拒不烧 seq、不写收件箱、不扇出 APNs。
	// daemon 收到 rejected 即终态丢弃（删 WAL），无重试风暴。
	if h.cfg.Quota != nil && !h.cfg.Quota.AllowEvent(c.Tier, c.DaemonID, env.Type) {
		h.cfg.Metrics.IncQuotaRejected("rate_event")
		h.log.Warn("事件被速率闸拒绝", "daemon_id", c.DaemonID,
			"event_id", env.EventID, "type", env.Type)
		h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckRejected)
		return
	}
	now := h.now().Unix()
	stored, duplicate, err := h.cfg.Store.AppendInbox(c.DaemonID, *env, now)
	if err != nil {
		h.log.Error("写入收件箱失败", "event_id", env.EventID, "err", err)
		h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckRejected)
		return
	}
	h.cfg.Metrics.IncEvent(string(env.Type))
	if duplicate {
		// 重复事件不重复入箱、不重复推送；ack 让 daemon 清 WAL。
		h.cfg.Metrics.IncDedup()
		h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckAccepted)
		return
	}

	if stored.Type == wire.EventApprovalRequired {
		p := store.Pending{
			ApprovalID: stored.ApprovalID,
			DaemonID:   stored.DaemonID,
			Event:      stored,
			Status:     "pending",
			CreatedAt:  now,
			ExpiresAt:  now + wire.PendingTTLSeconds,
		}
		if err := h.cfg.Store.PutPending(p); err != nil {
			h.log.Error("登记待审批失败", "approval_id", stored.ApprovalID, "err", err)
			h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckRejected)
			return
		}
	}

	h.sendToApps(stored.DaemonID, stored)
	h.notify(ctx, stored)
	h.ack(c, env.MsgID, env.EventID, env.ApprovalID, wire.AckAccepted)
}

// onDecision 处理 App 决策：first-wins 落库，必要时转发给 daemon。
func (h *Hub) onDecision(c *Conn, d *wire.Decision) {
	if c.Role != wire.RoleApp {
		h.ack(c, d.MsgID, "", d.ApprovalID, wire.AckRejected)
		return
	}
	now := h.now().Unix()
	status, p, err := h.cfg.Store.ApplyDecision(d.ApprovalID, c.DaemonID, string(d.Decision), now)
	if err != nil {
		h.log.Error("写入决策失败", "approval_id", d.ApprovalID, "err", err)
		h.ack(c, d.MsgID, "", d.ApprovalID, wire.AckRejected)
		return
	}
	h.cfg.Metrics.IncDecision(string(d.Decision), string(status))

	if status == wire.AckAccepted {
		h.forwardDecision(p.DaemonID, wire.Decision{
			V: wire.Version, Kind: wire.KindDecision, MsgID: id.New(),
			ApprovalID: d.ApprovalID, Decision: d.Decision, TS: now,
		})
	}
	h.ack(c, d.MsgID, "", d.ApprovalID, status)
}

// onAck 处理 daemon 对决策的确认，标记为已送达，重连时不再重发。
func (h *Hub) onAck(c *Conn, a *wire.Ack) {
	if c.Role != wire.RoleDaemon || a.ApprovalID == "" {
		return
	}
	if err := h.cfg.Store.MarkDecisionDelivered(a.ApprovalID); err != nil {
		h.log.Warn("标记决策已送达失败", "approval_id", a.ApprovalID, "err", err)
	}
}

// onLAToken 登记/注销 Live Activity push token。
func (h *Hub) onLAToken(c *Conn, l *wire.LAToken) {
	status := wire.AckAccepted
	switch {
	case c.Role != wire.RoleApp || l.DaemonID != c.DaemonID:
		status = wire.AckRejected
	case l.Event == wire.LAEnd:
		if err := h.cfg.Store.DeleteLAToken(c.ID, l.SessionID); err != nil {
			h.log.Warn("删除 LA token 失败", "session_id", l.SessionID, "err", err)
			status = wire.AckRejected
		}
	default:
		if err := h.cfg.Store.PutLAToken(store.LAToken{
			DeviceID: c.ID, DaemonID: l.DaemonID, SessionID: l.SessionID,
			PushToken: l.PushToken, UpdatedAt: h.now().Unix(),
		}); err != nil {
			h.log.Warn("保存 LA token 失败", "session_id", l.SessionID, "err", err)
			status = wire.AckRejected
		}
	}
	_ = c.Send(wire.LATokenAck{
		V: wire.Version, Kind: wire.KindLATokenAck, MsgID: id.New(),
		RefID: l.MsgID, Status: status, TS: h.now().Unix(),
	})
}

// sendResync 返回 pending 审批与收件箱增量。
func (h *Hub) sendResync(c *Conn, r wire.Resync) {
	h.cfg.Metrics.IncResync()
	now := h.now().Unix()
	pending, err := h.cfg.Store.PendingForDaemon(c.DaemonID, now)
	if err != nil {
		h.log.Error("读取待审批失败", "daemon_id", c.DaemonID, "err", err)
	}
	cursor, _ := strconv.ParseInt(r.Cursor, 10, 64)
	events, next, err := h.cfg.Store.InboxSince(c.DaemonID, cursor, r.Since, resyncLimit)
	if err != nil {
		h.log.Error("读取收件箱失败", "daemon_id", c.DaemonID, "err", err)
	}

	result := wire.ResyncResult{
		V: wire.Version, Kind: wire.KindResyncResult, MsgID: id.New(),
		RefMsgID: r.MsgID, Events: events, TS: now,
	}
	for _, p := range pending {
		result.Pending = append(result.Pending, p.Event)
	}
	if next > 0 {
		result.NextCursor = strconv.FormatInt(next, 10)
	}
	if err := c.Send(result); err != nil {
		h.log.Warn("发送 resync 结果失败", "device_id", c.ID, "err", err)
	}
}

// replayDecisions 在 daemon 重连时补发尚未确认的决策。
func (h *Hub) replayDecisions(c *Conn) {
	items, err := h.cfg.Store.UndeliveredDecisions(c.DaemonID, h.now().Unix())
	if err != nil {
		h.log.Error("读取未送达决策失败", "daemon_id", c.DaemonID, "err", err)
		return
	}
	for _, p := range items {
		if err := c.Send(wire.Decision{
			V: wire.Version, Kind: wire.KindDecision, MsgID: id.New(),
			ApprovalID: p.ApprovalID, Decision: wire.DecisionKind(p.Decision), TS: h.now().Unix(),
		}); err != nil {
			h.log.Warn("补发决策失败", "approval_id", p.ApprovalID, "err", err)
			return
		}
	}
}

func (h *Hub) forwardDecision(daemonID string, d wire.Decision) {
	h.mu.RLock()
	c := h.daemons[daemonID]
	h.mu.RUnlock()
	if c == nil {
		// daemon 离线：决策已落库，重连后由 replayDecisions 补发。
		h.log.Info("daemon 离线，决策等待重连补发", "daemon_id", daemonID, "approval_id", d.ApprovalID)
		return
	}
	if err := c.Send(d); err != nil {
		h.log.Warn("转发决策失败", "daemon_id", daemonID, "approval_id", d.ApprovalID, "err", err)
	}
}

func (h *Hub) sendToApps(daemonID string, env wire.Envelope) {
	h.mu.RLock()
	targets := make([]*Conn, 0, 1)
	for _, c := range h.apps {
		if c.DaemonID == daemonID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		if err := c.Send(env); err != nil {
			h.log.Warn("推送事件给 App 失败", "device_id", c.ID, "event_id", env.EventID, "err", err)
		}
	}
}

// notify 异步扇出到 APNs，不阻塞 hook 的等待链路。
func (h *Hub) notify(ctx context.Context, env wire.Envelope) {
	if h.cfg.Notifier == nil {
		return
	}
	devices, err := h.cfg.Store.DevicesForDaemon(env.DaemonID)
	if err != nil {
		h.log.Error("读取设备列表失败", "daemon_id", env.DaemonID, "err", err)
		return
	}
	for _, d := range devices {
		device := d
		// 不继承连接的 ctx：daemon 断线不应取消已排队的 APNs 投递。
		go h.cfg.Notifier.Notify(context.Background(), device, env)
	}
}

func (h *Hub) ack(c *Conn, ref, eventID, approvalID string, status wire.AckStatus) {
	ack := wire.Ack{
		V: wire.Version, Kind: wire.KindAck, MsgID: id.New(), RefMsgID: ref,
		EventID: eventID, ApprovalID: approvalID, Status: status, TS: h.now().Unix(),
	}
	if err := c.Send(ack); err != nil {
		h.log.Debug("发送 ack 失败", "id", c.ID, "err", err)
	}
}
