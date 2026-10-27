// Package server 暴露 relay 的 HTTP/WebSocket 端点。
package server

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/pairing"
	"github.com/twotwoba/DevPilot-APNS/internal/ratelimit"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

const (
	maxBodyBytes = 64 << 10
	maxNameRunes = 80
	pairLimit    = 30 // 每 IP 每分钟
	pairAttempts = 20
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
	// Ready 是额外的就绪检查（如 APNs 密钥可用性）；为空时只检查数据库。
	Ready func() error
}

// Server 实现 http.Handler。
type Server struct {
	cfg Config
	log *slog.Logger
	now func() time.Time
}

// New 构造 server。
func New(cfg Config) (*Server, error) {
	if cfg.Store == nil || cfg.Hub == nil {
		return nil, errors.New("server: Store 与 Hub 均不能为空")
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
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	return s.logging(s.recoverPanics(mux))
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
	if !s.cfg.PairLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "请求过于频繁")
		return
	}
	var req pairingSessionRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	now := s.now().Unix()
	code := canonicalCode(req.Code)
	if !validCode(code) || !validDaemonID(req.DaemonID) || !validHash(req.DaemonCredHash) {
		writeError(w, http.StatusBadRequest, "配对会话参数不合法")
		return
	}
	if req.ExpiresAt == 0 {
		req.ExpiresAt = now + int64(pairing.CodeTTL.Seconds())
	}
	if req.ExpiresAt <= now+60 || req.ExpiresAt > now+int64((30*time.Minute).Seconds()) {
		writeError(w, http.StatusBadRequest, "配对会话有效期不合法")
		return
	}
	name := truncateRunes(strings.TrimSpace(req.DaemonName), maxNameRunes)
	if name == "" {
		name = "devpilot-daemon"
	}

	// 已注册的 daemon 轮换凭据时必须证明持有旧凭据，防止他人抢占 daemon_id。
	if _, err := s.cfg.Store.Daemon(req.DaemonID); err == nil {
		if _, ok := s.authDaemon(r); !ok {
			writeError(w, http.StatusUnauthorized, "轮换凭据需要提供当前 daemon 凭据")
			return
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		s.internal(w, "读取 daemon 失败", err)
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
		s.internal(w, "登记配对会话失败", err)
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
}

func (s *Server) handlePair(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.AttemptLimiter.Allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, "请求过于频繁")
		return
	}
	var req pairRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if !validCode(canonicalCode(req.Code)) {
		writeError(w, http.StatusBadRequest, "配对码格式不合法")
		return
	}
	// 档位由 App 配对时提交(空=free;非法值拒绝,防拼写错误静默降档)。
	tier, ok := store.ParseTier(req.Tier)
	if !ok {
		writeError(w, http.StatusBadRequest, "档位不合法(空或 free|pro)")
		return
	}
	now := s.now().Unix()
	secret, err := pairing.NewSecret()
	if err != nil {
		s.internal(w, "生成设备凭据失败", err)
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
		writeError(w, http.StatusGone, "配对码已过期")
		return
	case errors.Is(err, store.ErrCodeInvalid):
		writeError(w, http.StatusNotFound, "配对码无效或已使用")
		return
	case err != nil:
		s.internal(w, "配对失败", err)
		return
	}
	s.log.Info("设备配对成功", "daemon_id", daemon.ID, "device_id", deviceID, "device_name", name, "tier", tier)
	writeJSON(w, http.StatusCreated, map[string]any{
		"device_id":         deviceID,
		"device_credential": pairing.DeviceCredential(deviceID, secret),
		"daemon_id":         daemon.ID,
		"daemon_name":       daemon.Name,
		"server_ts":         now,
	})
}

// ---------- WebSocket ----------

func (s *Server) handleDaemonWS(w http.ResponseWriter, r *http.Request) {
	daemon, ok := s.authDaemon(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "daemon 凭据无效")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("daemon WebSocket 升级失败", "daemon_id", daemon.ID, "err", err)
		return
	}
	s.log.Info("daemon 已连接", "daemon_id", daemon.ID)
	s.cfg.Hub.Serve(r.Context(), conn, wire.RoleDaemon, daemon.ID, daemon.ID, daemon.Tier)
	s.log.Info("daemon 已断开", "daemon_id", daemon.ID)
}

func (s *Server) handleAppWS(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "设备凭据无效")
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		CompressionMode: websocket.CompressionDisabled,
	})
	if err != nil {
		s.log.Warn("App WebSocket 升级失败", "device_id", device.ID, "err", err)
		return
	}
	_ = s.cfg.Store.TouchDevice(device.ID, s.now().Unix())
	s.log.Info("App 已连接", "device_id", device.ID, "daemon_id", device.DaemonID)
	s.cfg.Hub.Serve(r.Context(), conn, wire.RoleApp, device.ID, device.DaemonID, "")
	s.log.Info("App 已断开", "device_id", device.ID)
}

// ---------- Live Activity / 对账 ----------

type laTokenRequest struct {
	DaemonID  string       `json:"daemon_id"`
	SessionID string       `json:"session_id"`
	PushToken string       `json:"push_token"`
	Event     wire.LAEvent `json:"event"`
}

func (s *Server) handleLATokenREST(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "设备凭据无效")
		return
	}
	var req laTokenRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.DaemonID != device.DaemonID || req.SessionID == "" {
		writeError(w, http.StatusBadRequest, "la_token 参数不合法")
		return
	}
	if req.Event == "" {
		req.Event = wire.LAUpdate
	}
	switch req.Event {
	case wire.LAStart, wire.LAUpdate:
		if req.PushToken == "" {
			writeError(w, http.StatusBadRequest, "缺少 push_token")
			return
		}
		if err := s.cfg.Store.PutLAToken(store.LAToken{
			DeviceID: device.ID, DaemonID: device.DaemonID, SessionID: req.SessionID,
			PushToken: req.PushToken, UpdatedAt: s.now().Unix(),
		}); err != nil {
			s.internal(w, "保存 LA token 失败", err)
			return
		}
	case wire.LAEnd:
		if err := s.cfg.Store.DeleteLAToken(device.ID, req.SessionID); err != nil {
			s.internal(w, "删除 LA token 失败", err)
			return
		}
	default:
		writeError(w, http.StatusBadRequest, "未知 la event")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePending(w http.ResponseWriter, r *http.Request) {
	device, ok := s.authDevice(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "设备凭据无效")
		return
	}
	now := s.now().Unix()
	pending, err := s.cfg.Store.PendingForDaemon(device.DaemonID, now)
	if err != nil {
		s.internal(w, "读取待审批失败", err)
		return
	}
	cursor, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	events, next, err := s.cfg.Store.InboxSince(device.DaemonID, cursor, since, 200)
	if err != nil {
		s.internal(w, "读取收件箱失败", err)
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

// ---------- 健康检查与指标 ----------

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ok\n")
}

func (s *Server) handleReadyz(w http.ResponseWriter, _ *http.Request) {
	if err := s.cfg.Store.Ping(); err != nil {
		writeError(w, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready(); err != nil {
			writeError(w, http.StatusServiceUnavailable, "依赖不可用")
			return
		}
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = io.WriteString(w, "ready\n")
}

func (s *Server) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = io.WriteString(w, s.cfg.Metrics.Render())
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
		return nil, nil, errors.New("server: 底层 ResponseWriter 不支持 Hijack")
	}
	return hj.Hijack()
}

// Flush 透传给底层实现。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := s.now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		// 只记录元数据，绝不记录请求体（其中可能含配对码）。
		s.log.Debug("http",
			"method", r.Method, "path", r.URL.Path, "status", sw.status,
			"remote", clientIP(r), "duration", s.now().Sub(start).String())
	})
}

func (s *Server) recoverPanics(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				s.log.Error("请求处理 panic", "path", r.URL.Path, "panic", v)
				writeError(w, http.StatusInternalServerError, "内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return false
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		writeError(w, http.StatusBadRequest, "请求体包含多余内容")
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

// clientIP 优先使用反向代理写入的 X-Forwarded-For。
//
// relay 只应通过 Caddy 暴露；若直接公网监听，必须去掉对 XFF 的信任。
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if i := strings.IndexByte(xff, ','); i >= 0 {
			return strings.TrimSpace(xff[:i])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
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
