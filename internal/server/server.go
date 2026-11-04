// Package server 暴露 relay 的 HTTP/WebSocket 端点。
package server

import (
	"bufio"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/internal/proentitlement"
	"github.com/twotwoba/DevPilot-APNS/internal/ratelimit"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

const (
	maxBodyBytes = 64 << 10
	maxNameRunes = 80
	// 配对端点限流收紧(防爆破,锚点 fail2ban 宽于人工误操作)。
	pairLimit    = 15 // 每 IP 每分钟
	pairAttempts = 10
)

// Config 是 server 依赖。
type Config struct {
	Store          *store.Store
	Hub            *wss.Hub
	Metrics        *metrics.Metrics
	Logger         *slog.Logger
	PublicBaseURL  string
	Now            func() time.Time
	PairLimiter    *ratelimit.Limiter
	AttemptLimiter *ratelimit.Limiter
	// Bans 非 nil 时启用 fail2ban 式 IP 封禁;TrustedProxyCount 是可信反代数
	// (Caddy 部署设 1;0 表示忽略 X-Forwarded-For)。
	Bans              *ratelimit.BanManager
	TrustedProxyCount int
	// MetricsToken 非空时 /metrics 需要 Bearer(常量时间比较)。
	MetricsToken string
	// Ready 是额外的就绪检查（如 APNs 密钥可用性）；为空时只检查数据库。
	Ready func() error
	// ProVerifier 非 nil 时启用 Pro 档位收据验签(官方部署):客户端自报
	// tier=pro 必须附 Apple 签名的 StoreKit 2 交易 JWS,验签通过才落 pro,
	// 否则配对静默降 free、token 改档 422。nil = 不验(自建部署,客户端
	// 自报直通——v1 行为)。
	ProVerifier *proentitlement.Verifier
}

// Server 实现 http.Handler。
type Server struct {
	cfg Config
	log *slog.Logger
	now func() time.Time
	// snapshotMu 在线快照防重入(POST /admin/snapshot):bbolt 读事务
	// 本身不挡写路径,但重复快照纯属浪费磁盘 IO,并发触发直接 409。
	snapshotMu sync.Mutex
}

// New 构造 server。
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Hub == nil {
		return nil, errors.New("server: Store and Hub must not be nil")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Metrics == nil {
		cfg.Metrics = metrics.New()
	}
	if cfg.PairLimiter == nil {
		cfg.PairLimiter = ratelimit.New(pairLimit, time.Minute, cfg.Now)
	}
	if cfg.AttemptLimiter == nil {
		cfg.AttemptLimiter = ratelimit.New(pairAttempts, time.Minute, cfg.Now)
	}
	m := cfg.Metrics
	m.DaemonConns = func() int { d, _ := cfg.Hub.Counts(); return d }
	m.AppConns = func() int { _, a := cfg.Hub.Counts(); return a }
	m.Pending = func() int { n, _ := cfg.Store.CountPending(cfg.Now().Unix()); return n }
	return &Server{cfg: cfg, log: cfg.Logger, now: cfg.Now}, nil
}

// Handler 返回带中间件的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/pairing/sessions", s.handlePairingSession)
	mux.HandleFunc("POST /v1/pair", s.handlePair)
	mux.HandleFunc("GET /ws/daemon", s.handleDaemonWS)
	mux.HandleFunc("GET /ws/app", s.handleAppWS)
	mux.HandleFunc("POST /v1/la-tokens", s.handleLATokenREST)
	mux.HandleFunc("GET /v1/pending", s.handlePending)
	mux.HandleFunc("POST /v1/decisions", s.handleDecisionREST)
	mux.HandleFunc("POST /v1/devices/token", s.handleDeviceToken)
	mux.HandleFunc("DELETE /v1/device", s.handleDeleteDevice)
	mux.HandleFunc("DELETE /v1/daemon", s.handleDeleteDaemon)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	mux.HandleFunc("POST /admin/snapshot", s.handleAdminSnapshot)
	return s.logging(s.recoverPanics(s.banGuard(mux)))
}

// ---------- 配对 ----------

type pairingSessionRequest struct {
	Code           string `json:"code"`
	DaemonID       string `json:"daemon_id"`
	DaemonCredHash string `json:"daemon_cred_hash"`
	DaemonName     string `json:"daemon_name"`
	ExpiresAt      int64  `json:"expires_at"`
}

func (s *Server) handlePairingSession(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.PairLimiter.Allow(s.ipOf(r)) {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	var req pairingSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	now := s.now().Unix()
	code := canonicalCode(req.Code)
	if !validCode(code) || !validDaemonID(req.DaemonID) || !validHash(req.DaemonCredHash) {
		writeError(w, http.StatusBadRequest, "invalid pairing session parameters")
		return
	}
	if req.ExpiresAt == 0 {
		req.ExpiresAt = now + int64(pairing.CodeTTL.Seconds())
	}
	if req.ExpiresAt <= now+60 || req.ExpiresAt > now+int64((30*time.Minute).Seconds()) {
		writeError(w, http.StatusBadRequest, "invalid pairing session expiry")
		return
	}
	name := truncateRunes(strings.TrimSpace(req.DaemonName), maxNameRunes)
	if name == "" {
		name = "devpilot-daemon"
	}

	// 已注册的 daemon 轮换凭据时必须证明持有旧凭据，防止他人抢占 daemon_id。
	if _, err := s.cfg.Store.Daemon(req.DaemonID); err == nil {
		if _, ok := s.authDaemon(r); !ok {
			writeError(w, http.StatusUnauthorized, "credential rotation requires the current daemon credential")
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "failed to read daemon", err)
		return
	}

	if err := s.cfg.Store.RegisterPairingSession(store.PairingSession{
		CodeHash:   pairing.CodeHash(code),
		DaemonID:   req.DaemonID,
		DaemonName: name,
		CredHash:   req.DaemonCredHash,
		ExpiresAt:  req.ExpiresAt,
		CreatedAt:  now,
	}); err != nil {
		s.internal(w, "failed to register pairing session", err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"daemon_id":  req.DaemonID,
		"expires_at": req.ExpiresAt,
		"server_ts":  now,
	})
}

type pairRequest struct {
	Code       string `json:"code"`
	APNSToken  string `json:"apns_token"`
	DeviceName string `json:"device_name"`
	Tier       string `json:"tier"`
	// ProJWS Apple 签名的 StoreKit 2 交易凭证(ProStore.currentEntitlements
	// 的 jwsRepresentation)。仅 ENFORCE_PRO_TIER 部署消费,自建忽略。
	ProJWS string `json:"pro_jws,omitempty"`
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AttemptLimiter.Allow(s.ipOf(r)) {
		writeError(w, http.StatusTooManyRequests, "too many requests")
		return
	}
	var req pairRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validCode(canonicalCode(req.Code)) {
		writeError(w, http.StatusBadRequest, "invalid pairing code format")
		return
	}
	// 档位由 App 配对时提交(空=free;非法值拒绝,防拼写错误静默降档)。
	// 验签部署(verifier 非 nil)上自报 pro 还须过收据闸(缺失/失败降 free,
	// 配对不因此失败);自建部署 verifier 为 nil 直通。
	tier, ok := store.ParseTier(req.Tier)
	if !ok {
		writeError(w, http.StatusBadRequest, "invalid tier (empty or free|pro)")
		return
	}
	tier = s.resolveTier(tier, req.ProJWS)
	now := s.now().Unix()
	secret, err := pairing.NewSecret()
	if err != nil {
		s.internal(w, "failed to generate device credential", err)
		return
	}
	deviceID := pairing.NewDeviceID()
	name := truncateRunes(strings.TrimSpace(req.DeviceName), maxNameRunes)
	if name == "" {
		name = "iPhone"
	}
	device := store.Device{
		ID:        deviceID,
		CredHash:  pairing.SecretHash(secret),
		APNSToken: strings.TrimSpace(req.APNSToken),
		Name:      name,
	}
	daemon, _, err := s.cfg.Store.Pair(pairing.CodeHash(canonicalCode(req.Code)), device, tier, now)
	switch {
	case errors.Is(err, store.ErrCodeExpired):
		writeError(w, http.StatusGone, "pairing code expired")
		return
	case errors.Is(err, store.ErrCodeInvalid):
		writeError(w, http.StatusNotFound, "pairing code invalid or already used")
		return
	case err != nil:
		s.internal(w, "pairing failed", err)
		return
	}
	s.log.Info("device paired", "daemon_id", daemon.ID, "device_id", deviceID, "device_name", name, "tier", tier)
	// tier 回显实际落库档位:验签部署上客户端声称 pro 被降 free 时可对齐
	writeJSON(w, http.StatusCreated, map[string]any{
		"device_id":         deviceID,
		"device_credential": pairing.DeviceCredential(deviceID, secret),
		"daemon_id":         daemon.ID,
		"daemon_name":       daemon.Name,
		"tier":              string(tier),
		"server_ts":         now,
	})
}

// resolveTier Pro 档位收据闸(唯一裁决点,pair 与 token 改档共用):
// 验签部署上自报 pro 必须持有 Apple 签名的有效交易 JWS;缺失/失败降 free
// 并记日志。free 恒放行(降档永远不该被挡);verifier 为 nil(自建)直通。
func (s *Server) resolveTier(claimed store.Tier, proJWS string) store.Tier {
	if s.cfg.ProVerifier == nil || claimed != store.TierPro {
		return claimed
	}
	if strings.TrimSpace(proJWS) == "" {
		s.log.Warn("pro tier claimed without receipt, downgrading to free")
		return store.TierFree
	}
	ent, err := s.cfg.ProVerifier.Verify(proJWS)
	if err != nil {
		s.log.Warn("pro receipt verification failed, downgrading to free", "err", err)
		return store.TierFree
	}
	s.log.Info("pro receipt verified",
		"product_id", ent.ProductID, "environment", ent.Environment)
	return store.TierPro
}

// ---------- WebSocket ----------

func (s *Server) handleDaemonWS(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.authDaemon(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid daemon credential")
		return
	}
	// 建连即刷新活跃时刻与观测源 IP,TTL 清理据此判定存续;
	// LastIP 是 NAT 后公网地址,仅作路由提示(内网直连匹配靠 payload.host_ip)。
	if err := s.cfg.Store.SetDaemonObservedIP(daemon.ID, s.ipOf(r), s.now().Unix()); err != nil {
		s.log.Warn("failed to touch daemon liveness", "daemon_id", daemon.ID, "err", err)
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("daemon websocket upgrade failed", "daemon_id", daemon.ID, "err", err)
		return
	}
	s.log.Info("daemon connected", "daemon_id", daemon.ID)
	s.cfg.Hub.Serve(r.Context(), conn, wire.RoleDaemon, daemon.ID, daemon.ID, daemon.Tier)
	s.log.Info("daemon disconnected", "daemon_id", daemon.ID)
}

func (s *Server) handleAppWS(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("app websocket upgrade failed", "device_id", device.ID, "err", err)
		return
	}
	_ = s.cfg.Store.TouchDevice(device.ID, s.now().Unix())
	s.log.Info("app connected", "device_id", device.ID, "daemon_id", device.DaemonID)
	s.cfg.Hub.Serve(r.Context(), conn, wire.RoleApp, device.ID, device.DaemonID, "")
	s.log.Info("app disconnected", "device_id", device.ID)
}

// ---------- Live Activity / 对账 ----------

// laTokenRequest 是 /v1/la-tokens 的请求体。event=push_start 时上报设备级
// push-to-start token(云审批 LA 用,session_id 留空);start/update/end 对应
// 会话级 activity token 的生命周期。
type laTokenRequest struct {
	DaemonID  string       `json:"daemon_id"`
	SessionID string       `json:"session_id"`
	PushToken string       `json:"push_token"`
	Event     wire.LAEvent `json:"event"`
}

func (s *Server) handleLATokenREST(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	var req laTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Event == "" {
		req.Event = wire.LAUpdate
	}
	// push_start 上报设备级 token:允许不带 session_id(App 未起任何 activity)。
	if req.Event == wire.LAPushStart {
		if req.PushToken == "" {
			writeError(w, http.StatusBadRequest, "missing push_token")
			return
		}
		if err := s.cfg.Store.SetPushToStartToken(device.ID, req.PushToken, s.now().Unix()); err != nil {
			s.internal(w, "failed to save push-to-start token", err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if req.DaemonID != device.DaemonID || req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "invalid la_token parameters")
		return
	}
	switch req.Event {
	case wire.LAStart, wire.LAUpdate:
		if req.PushToken == "" {
			writeError(w, http.StatusBadRequest, "missing push_token")
			return
		}
		if err := s.cfg.Store.PutLAToken(store.LAToken{
			DeviceID: device.ID, DaemonID: device.DaemonID, SessionID: req.SessionID,
			PushToken: req.PushToken, UpdatedAt: s.now().Unix(),
		}); err != nil {
			s.internal(w, "failed to save LA token", err)
			return
		}
	case wire.LAEnd:
		if err := s.cfg.Store.DeleteLAToken(device.ID, req.SessionID); err != nil {
			s.internal(w, "failed to delete LA token", err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "unknown la event")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	now := s.now().Unix()
	pending, err := s.cfg.Store.PendingForDaemon(device.DaemonID, now)
	if err != nil {
		s.internal(w, "failed to read pending approvals", err)
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	events, next, err := s.cfg.Store.InboxSince(device.DaemonID, cursor, since, 200)
	if err != nil {
		s.internal(w, "failed to read inbox", err)
		return
	}
	result := wire.ResyncResult{
		V: wire.Version, Kind: wire.KindResyncResult, MsgID: "rest-" + strconv.FormatInt(now, 10),
		Events: events, TS: now,
	}
	for _, p := range pending {
		result.Pending = append(result.Pending, p.Event)
	}
	if next > 0 {
		result.NextCursor = strconv.FormatInt(next, 10)
	}
	writeJSON(w, http.StatusOK, result)
}

// ---------- 决策(LA 按钮 / 通知快捷钮的 REST 通道) ----------

type decisionRequest struct {
	ApprovalID string `json:"approval_id"`
	Decision   string `json:"decision"`
	// Choice 选项型审批所选标签（可空）；长度校验在 wire.Decision.Validate。
	Choice string `json:"choice,omitempty"`
}

// handleDecisionREST 是 App 侧无需 WSS 的决策入口:LA 按钮(App Intent 进程)与
// 通知快捷钮在网络在而 WSS 断连时的回落通道。语义与 WSS decision 完全一致
// (first-wins、合成 close、daemon 离线则待补发),经 Hub.ApplyDeviceDecision 共路。
func (s *Server) handleDecisionREST(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	var req decisionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	decision := wire.DecisionKind(strings.TrimSpace(req.Decision))
	if strings.TrimSpace(req.ApprovalID) == "" || !decision.Valid() {
		writeError(w, http.StatusBadRequest, "approval_id and decision (allow|deny) are required")
		return
	}
	choice := strings.TrimSpace(req.Choice)
	// choice 超帽 = 畸形请求：与 wire.Decision.Validate 同一钳制口径。
	if len([]rune(choice)) > wire.MaxOptionRunes {
		writeError(w, http.StatusBadRequest, "choice too long")
		return
	}
	status, err := s.cfg.Hub.ApplyDeviceDecision(device.DaemonID, strings.TrimSpace(req.ApprovalID), decision, choice, s.now().Unix())
	if err != nil {
		s.internal(w, "failed to apply decision", err)
		return
	}
	s.cfg.Metrics.IncDecision(string(decision), string(status))
	writeJSON(w, http.StatusOK, map[string]string{"status": string(status)})
}

// ---------- 生命周期 ----------

type deviceTokenRequest struct {
	APNSToken string `json:"apns_token"`
	// Tier 可选:App 权益翻转时同步档位;留空不触碰(旧版 App 兼容)。
	Tier string `json:"tier,omitempty"`
	// ProJWS 升 pro 时验签用;降 free 不需要(降档不该被挡)。
	ProJWS string `json:"pro_jws,omitempty"`
}

// handleDeviceToken App 重新上报 APNs token(410 剪枝后、重装、换机)。
func (s *Server) handleDeviceToken(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	var req deviceTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	token := strings.TrimSpace(req.APNSToken)
	if token == "" || len(token) > 256 {
		writeError(w, http.StatusBadRequest, "invalid apns_token")
		return
	}
	// 档位刷新:订阅到期降 free / 续费升 pro,配额随之收敛,不需要重新扫码。
	// 先校验再写库(避免 token 已改、tier 非法时的半提交);改档后踢 daemon
	// 连接——quota 按 Conn.Tier 结算、建连时缓存,daemon 秒级退避重连后
	// 即刻按新档生效。
	var tier store.Tier
	if req.Tier != "" {
		var ok bool
		if tier, ok = store.ParseTier(req.Tier); !ok {
			writeError(w, http.StatusBadRequest, "invalid tier")
			return
		}
		// 验签部署上升 pro 须过收据闸。与 pair 不同,这里不静默降级而是
		// 422:客户端在「权益同步」语义下需要知道升档没成(好下次再收敛);
		// 422 不写任何库(token/tier 都不碰),客户端静默容错,时机再试。
		if s.cfg.ProVerifier != nil && tier == store.TierPro &&
			s.resolveTier(tier, req.ProJWS) != store.TierPro {
			writeError(w, http.StatusUnprocessableEntity, "pro tier requires a valid receipt")
			return
		}
	}
	if err := s.cfg.Store.UpdateDeviceAPNSToken(device.ID, token, s.now().Unix()); err != nil {
		s.internal(w, "failed to update APNs token", err)
		return
	}
	if req.Tier != "" {
		if err := s.cfg.Store.SetDaemonTier(device.DaemonID, tier, s.now().Unix()); err != nil {
			s.internal(w, "failed to update tier", err)
			return
		}
		s.cfg.Hub.KickDaemon(device.DaemonID)
		s.log.Info("daemon tier updated", "daemon_id", device.DaemonID, "tier", tier)
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteDevice 设备自助解绑:级联删 LA token,凭据即刻失效。
func (s *Server) handleDeleteDevice(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid device credential")
		return
	}
	if err := s.cfg.Store.DeleteDevice(device.ID); err != nil {
		s.internal(w, "failed to unbind device", err)
		return
	}
	// 踢掉该设备的在线连接：防止已注销 device 的僵尸连接继续收事件。
	s.cfg.Hub.KickDevice(device.ID)
	s.log.Info("device unbound", "device_id", device.ID, "daemon_id", device.DaemonID)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteDaemon daemon 吊销(devpilot-hook unpair 调用):
// 级联删 devices/la_tokens/inbox/event_ids/pending。
func (s *Server) handleDeleteDaemon(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.authDaemon(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "invalid daemon credential")
		return
	}
	if err := s.cfg.Store.DeleteDaemonCascade(daemon.ID); err != nil {
		s.internal(w, "failed to revoke daemon", err)
		return
	}
	// 级联删除后凭据已失效，踢掉 daemon 及其名下设备的全部在线连接。
	s.cfg.Hub.KickDaemon(daemon.ID)
	s.log.Info("daemon revoked", "daemon_id", daemon.ID)
	w.WriteHeader(http.StatusNoContent)
}

// ---------- 健康检查与指标 ----------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if err := s.cfg.Store.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "dependency unavailable")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ready\n")
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	// 常量时间比较,防时序侧信道猜 token。
	if s.cfg.MetricsToken != "" &&
		subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.cfg.MetricsToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, s.cfg.Metrics.Render())
}

// handleAdminSnapshot 进程内 bbolt 一致性快照(在线备份通道)。
//
// 旧 `relay snapshot` 子命令是第二进程 bolt.Open 同一个数据库——常驻 relay
// 持 flock 独占,必然超时(bbolt 无 WAL,运行中直接 cp 文件是撕裂副本,官方
// 唯一认可的在线备份就是本进程读事务里 WriteTo)。CLI 子命令保留给停机
// 恢复场景,在线备份一律走这里。
//
// 鉴权复用 METRICS_TOKEN(同 ops 信任面,零新配置);token 未设时整个端点
// 禁用——快照触发磁盘写,不允许匿名触达。路由挂 /admin 不进 /v1(客户端面)。
// 输出落在数据库同级的 snapshots/ 目录:relay-<UTC 时间戳>.db。
func (s *Server) handleAdminSnapshot(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MetricsToken == "" {
		writeError(w, http.StatusServiceUnavailable, "snapshot disabled: no admin token configured")
		return
	}
	// 常量时间比较,与 /metrics 同一 pattern,防时序侧信道猜 token。
	if subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(s.cfg.MetricsToken)) != 1 {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !s.snapshotMu.TryLock() {
		writeError(w, http.StatusConflict, "snapshot already in progress")
		return
	}
	defer s.snapshotMu.Unlock()
	start := s.now()
	out := filepath.Join(
		filepath.Dir(s.cfg.Store.Path()), "snapshots",
		fmt.Sprintf("relay-%s.db", start.UTC().Format("20060102T150405Z")))
	if err := s.cfg.Store.Snapshot(out); err != nil {
		s.internal(w, "snapshot failed", err)
		return
	}
	info, err := os.Stat(out)
	if err != nil {
		s.internal(w, "snapshot stat failed", err)
		return
	}
	s.log.Info("snapshot written", "path", out, "bytes", info.Size())
	writeJSON(w, http.StatusCreated, map[string]any{
		"path":        out,
		"bytes":       info.Size(),
		"duration_ms": s.now().Sub(start).Milliseconds(),
	})
}

// ---------- 鉴权 ----------

func (s *Server) authDaemon(r *http.Request) (store.Daemon, bool) {
	token := bearerToken(r)
	id, secret, ok := pairing.ParseCredential(token, pairing.DaemonPrefix, pairing.DaemonIDPrefix)
	if !ok {
		return store.Daemon{}, false
	}
	return s.cfg.Store.VerifyDaemon(id, secret)
}

func (s *Server) authDevice(r *http.Request) (store.Device, bool) {
	token := bearerToken(r)
	id, secret, ok := pairing.ParseCredential(token, pairing.DevicePrefix, pairing.DeviceIDPrefix)
	if !ok {
		return store.Device{}, false
	}
	return s.cfg.Store.VerifyDevice(id, secret)
}

func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// ---------- 中间件与工具 ----------

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Unwrap 让 http.ResponseController 能穿透包装取到底层连接。
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Hijack 保证 WebSocket 升级不被日志中间件破坏。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hj, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("server: underlying ResponseWriter does not support Hijack")
	}
	return hj.Hijack()
}

// Flush 透传给底层实现。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// banGuard fail2ban 式 IP 封禁:失败累积达阈值的 IP 全端点 403(不泄露原因)。
// 健康与指标端点不计失败(监控探测不得引发封禁);环回地址整体豁免
// (容器 HEALTHCHECK 直连 127.0.0.1,绝不能被自己打死)。
func (s *Server) banGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.cfg.Bans.Enabled() {
			next.ServeHTTP(w, r)
			return
		}
		ip := s.ipOf(r)
		if pip := net.ParseIP(ip); pip != nil && pip.IsLoopback() {
			next.ServeHTTP(w, r)
			return
		}
		key := ratelimit.AggregateKey(ip)
		if s.cfg.Bans.Banned(key) {
			s.log.Debug("banned IP rejected", "ip", ip, "path", r.URL.Path)
			writeError(w, http.StatusForbidden, "request rejected")
			return
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		switch r.URL.Path {
		case "/healthz", "/readyz", "/metrics", "/admin/snapshot":
			// ops 面(健康/指标/快照)不计失败:监控与备份 cron 的过期
			// token 不得把部署机自己 IP 灌进封禁表(自打 DEAD)。
			return
		}
		if sw.status >= 400 {
			s.cfg.Bans.RecordFailure(key)
		}
	})
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// 只记录元数据，绝不记录请求体（其中可能含配对码）。
		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"remote", s.ipOf(r), "duration", s.now().Sub(start).String())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("panic handling request", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "request body is not valid JSON")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "request body contains trailing data")
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internal(w http.ResponseWriter, msg string, err error) {
	s.log.Error(msg, "err", err)
	writeError(w, http.StatusInternalServerError, msg)
}

// realIP 从 RemoteAddr 与 X-Forwarded-For 推断客户端真实 IP(纯函数,表驱动测试)。
//
// XFF 左侧可被客户端任意伪造;可信反代数为 trusted 时取右数第 trusted 项
// (最右项由最外层可信代理追加)。仅当 RemoteAddr 是私网/环回——即流量确实
// 经过我们的反代——才采信 XFF;直连公网一律用 RemoteAddr。
func realIP(remoteHost, xff string, trusted int) string {
	ip := net.ParseIP(remoteHost)
	if ip == nil || trusted <= 0 || xff == "" || (!ip.IsPrivate() && !ip.IsLoopback()) {
		return remoteHost
	}
	parts := strings.Split(xff, ",")
	idx := max(len(parts)-trusted, 0)
	if cand := strings.TrimSpace(parts[idx]); cand != "" && net.ParseIP(cand) != nil {
		return cand
	}
	return remoteHost
}

// ipOf 是 realIP 的请求封装,限流/封禁/日志统一取址入口。
func (s *Server) ipOf(r *http.Request) string {
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		host = h
	}
	return realIP(host, r.Header.Get("X-Forwarded-For"), s.cfg.TrustedProxyCount)
}

func validCode(code string) bool {
	if len(code) < 16 || len(code) > 64 {
		return false
	}
	for _, r := range code {
		switch {
		case r >= 'A' && r <= 'Z', r >= '2' && r <= '7':
		default:
			return false
		}
	}
	return true
}

// canonicalCode 统一大小写并去掉用户可能输入的分隔符。
func canonicalCode(code string) string {
	code = strings.ReplaceAll(strings.TrimSpace(code), "-", "")
	return strings.ToUpper(code)
}

func validDaemonID(id string) bool {
	if !strings.HasPrefix(id, pairing.DaemonIDPrefix) || len(id) < 8 || len(id) > 64 {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
		default:
			return false
		}
	}
	return true
}

func validHash(h string) bool {
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max])
}
