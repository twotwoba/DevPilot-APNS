// Package wss 实现 relay 的 WebSocket 接入、路由与审批状态推进。
package wss

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/internal/id"
	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/quota"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

const (
	readLimit    = 1 << 20
	writeTimeout = 10 * time.Second
	sendTimeout  = 5 * time.Second
	helloTimeout = 15 * time.Second
	pingInterval = 30 * time.Second
	outBuffer    = 128
	resyncLimit  = 200
)

// Notifier 负责把事件推给 APNs（P4 注入；测试时可为 nil）。
type Notifier interface {
	Notify(ctx context.Context, device store.Device, env wire.Envelope)
}

// Config 是 Hub 依赖。
type Config struct {
	Store    *store.Store
	Logger   *slog.Logger
	Metrics  *metrics.Metrics
	Notifier Notifier
	// Quota 为空表示不限流(自部署单用户场景可关)。
	Quota *quota.Engine
	Now   func() time.Time
}

// Hub 维护在线连接并驱动事件/决策路由。
type Hub struct {
	cfg Config
	log *slog.Logger
	now func() time.Time

	mu      sync.RWMutex
	daemons map[string]*Conn
	apps    map[string]*Conn
}

// New 创建 Hub。
func New(cfg Config) *Hub {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	return &Hub{
		cfg:     cfg,
		log:     cfg.Logger,
		now:     cfg.Now,
		daemons: make(map[string]*Conn),
		apps:    make(map[string]*Conn),
	}
}

// KickDevice 主动断开指定设备的在线连接（设备解绑后调用）：
// 防止已注销 device 的僵尸连接继续收事件。close 幂等，Serve 的
// ws.Read 立即出错退出，由其 defer unregister 完成摘除。
func (h *Hub) KickDevice(deviceID string) {
	h.mu.RLock()
	c := h.apps[deviceID]
	h.mu.RUnlock()
	if c != nil {
		c.close()
	}
}

// KickDaemon 主动断开 daemon 自身及其名下全部设备的在线连接
// （daemon 吊销级联删除后调用）。锁内只收集，close 在锁外执行。
func (h *Hub) KickDaemon(daemonID string) {
	h.mu.RLock()
	targets := make([]*Conn, 0, 1)
	if c := h.daemons[daemonID]; c != nil {
		targets = append(targets, c)
	}
	for _, c := range h.apps {
		if c.DaemonID == daemonID {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close()
	}
}

// Counts 返回在线连接数（指标用）。
func (h *Hub) Counts() (daemons, apps int) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.daemons), len(h.apps)
}

// Conn 是一条已鉴权的 WSS 连接。
type Conn struct {
	Role     wire.Role
	ID       string
	DaemonID string
	Tier     store.Tier // daemon 连接缓存档位(App 连接为空,事件只来自 daemon)

	ws   *websocket.Conn
	out  chan []byte
	done chan struct{}
	once sync.Once
}

// Serve 处理一条已鉴权连接直到断开。tier 为 daemon 的配额档位,建连时缓存。
func (h *Hub) Serve(parent context.Context, ws *websocket.Conn, role wire.Role, id, daemonID string, tier store.Tier) {
	c := &Conn{
		Role: role, ID: id, DaemonID: daemonID, Tier: tier,
		ws: ws, out: make(chan []byte, outBuffer), done: make(chan struct{}),
	}
	h.register(c)
	defer h.unregister(c)
	defer c.close()

	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go c.writeLoop()

	ws.SetReadLimit(readLimit)
	if !h.handshake(ctx, c) {
		return
	}

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			if ctx.Err() == nil {
				h.log.Debug("connection closed", "role", role, "id", id, "err", err)
			}
			return
		}
		msg, err := wire.Decode(data)
		if err != nil {
			h.log.Warn("received undecodable message", "role", role, "id", id, "err", err)
			continue
		}
		h.dispatch(ctx, c, msg)
	}
}

// handshake 要求第一条消息是 hello，并校验身份与鉴权结果一致。
func (h *Hub) handshake(ctx context.Context, c *Conn) bool {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	defer cancel()
	_, data, err := c.ws.Read(hctx)
	if err != nil {
		h.log.Warn("failed to wait for hello", "role", c.Role, "id", c.ID, "err", err)
		return false
	}
	msg, err := wire.Decode(data)
	if err != nil {
		h.log.Warn("hello is undecodable", "err", err)
		return false
	}
	hello, ok := msg.(*wire.Hello)
	if !ok || hello.Role != c.Role {
		h.log.Warn("first message must be a hello with matching role", "role", c.Role, "id", c.ID)
		return false
	}
	if c.Role == wire.RoleDaemon && hello.DaemonID != c.DaemonID {
		h.log.Warn("hello daemon_id does not match credential", "cred", c.DaemonID, "hello", hello.DaemonID)
		return false
	}
	if c.Role == wire.RoleApp && hello.DeviceID != c.ID {
		h.log.Warn("hello device_id does not match credential", "cred", c.ID, "hello", hello.DeviceID)
		return false
	}

	if err := c.Send(wire.HelloAck{
		V: wire.Version, Kind: wire.KindHelloAck, MsgID: id.New(),
		RefMsgID: hello.MsgID, ServerTS: h.now().Unix(),
	}); err != nil {
		return false
	}

	switch c.Role {
	case wire.RoleDaemon:
		h.replayDecisions(c)
	case wire.RoleApp:
		if hello.Resync {
			h.sendResync(c, wire.Resync{
				V: wire.Version, Kind: wire.KindResync, MsgID: id.New(), TS: h.now().Unix(),
			})
		}
	}
	return true
}

func (h *Hub) register(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.Role == wire.RoleDaemon {
		if old := h.daemons[c.ID]; old != nil {
			old.close()
		}
		h.daemons[c.ID] = c
	} else {
		if old := h.apps[c.ID]; old != nil {
			old.close()
		}
		h.apps[c.ID] = c
	}
}

func (h *Hub) unregister(c *Conn) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c.Role == wire.RoleDaemon {
		if h.daemons[c.ID] == c {
			delete(h.daemons, c.ID)
		}
	} else if h.apps[c.ID] == c {
		delete(h.apps, c.ID)
	}
}

func (c *Conn) close() {
	c.once.Do(func() {
		close(c.done)
		c.ws.CloseNow()
	})
}

func (c *Conn) writeLoop() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case data := <-c.out:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.ws.Write(ctx, websocket.MessageText, data)
			cancel()
			if err != nil {
				c.close()
				return
			}
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), writeTimeout)
			err := c.ws.Ping(ctx)
			cancel()
			if err != nil {
				c.close()
				return
			}
		}
	}
}

// Send 序列化并投递；慢客户端会阻塞至多 sendTimeout。
func (c *Conn) Send(v any) error {
	data, err := wire.Encode(v)
	if err != nil {
		return err
	}
	timer := time.NewTimer(sendTimeout)
	defer timer.Stop()
	select {
	case c.out <- data:
		return nil
	case <-c.done:
		return errors.New("wss: connection closed")
	case <-timer.C:
		return errors.New("wss: send queue full")
	}
}

func (h *Hub) dispatch(ctx context.Context, c *Conn, msg any) {
	switch m := msg.(type) {
	case *wire.Envelope:
		h.onEvent(ctx, c, m)
	case *wire.Decision:
		h.onDecision(c, m)
	case *wire.Ack:
		h.onAck(c, m)
	case *wire.Resync:
		if c.Role == wire.RoleApp {
			h.sendResync(c, *m)
		}
	case *wire.LAToken:
		h.onLAToken(c, m)
	case *wire.Ping:
		_ = c.Send(wire.Pong{V: wire.Version, Kind: wire.KindPong, RefTime: m.TS, TS: h.now().Unix()})
	case *wire.Pong:
		// 心跳应答，无需处理。
	default:
		h.log.Debug("ignoring unknown message", "role", c.Role, "id", c.ID)
	}
}
