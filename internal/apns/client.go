// Package apns 实现最小可用的 APNs HTTP/2 推送客户端。
//
// 自研而不引 apns2：需要可注入的 http.Client（假服务器测试）、明确的错误分类，
// 以及按 topic 的全局退避。客户端不记录 payload，日志里只出现 token 摘要。
package apns

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/golang-jwt/jwt/v5"
)

const (
	// MaxPayloadBytes 是 APNs 单条推送的载荷上限。
	MaxPayloadBytes = 4096
	// tokenTTL 是 JWT 的本地缓存时长（Apple 要求 20~60 分钟刷新一次）。
	tokenTTL = 50 * time.Minute
	// maxAttempts 是单条推送的最大尝试次数（含首次）。
	maxAttempts = 3
	// maxCollapseIDBytes 是 apns-collapse-id 的字节上限。
	maxCollapseIDBytes = 64
	// defaultConcurrency 是同时在途的 HTTP/2 请求上限。
	defaultConcurrency = 64
)

const (
	productionEndpoint = "https://api.push.apple.com"
	sandboxEndpoint    = "https://api.sandbox.push.apple.com"
)

// ErrPayloadTooLarge 表示序列化后的载荷超过 APNs 上限。
var ErrPayloadTooLarge = errors.New("apns: 载荷超过 4KB")

// Request 是一次推送请求。
//
// Payload 可以是任意可 JSON 序列化的值；客户端只负责序列化与校验大小。
type Request struct {
	DeviceToken string
	Topic       string
	// PushType 为 alert / liveactivity / background；为空按 alert 处理。
	PushType string
	// Priority 为 10（立即）或 5（省电）；0 时按 10 处理。
	Priority int
	// Expiration 是绝对 Unix 秒；0 表示不设置（APNs 默认立即丢弃过期）。
	Expiration int64
	CollapseID string
	Payload    any
}

// Result 是 APNs 的响应摘要。
type Result struct {
	Status  int
	Reason  string
	APNSID  string
	Prune   bool
	Skipped bool
}

// Accepted 表示 APNs 已受理。
func (r Result) Accepted() bool { return r.Status == http.StatusOK }

// Config 是客户端配置。
type Config struct {
	KeyPath           string
	KeyID             string
	TeamID            string
	BundleID          string
	LiveActivityTopic string
	Environment       string // production / sandbox
	Mock              bool
	// Endpoint 仅用于测试注入（假 APNs 服务器）；为空时按 Environment 选择。
	Endpoint string
	// HTTPClient 允许测试注入信任假服务器证书的 client。
	HTTPClient *http.Client
	// RetryBase 是 5xx / 传输错误的首次退避，默认 500ms。
	RetryBase time.Duration
	// BackoffBase 是 429 时按 topic 退避的起始值，默认 500ms（测试可调小）。
	BackoffBase time.Duration
	Now         func() time.Time
}

// Call 是 Mock 模式下记录的一次推送。
type Call struct {
	Request Request
	Payload []byte
}

// Client 是并发安全的 APNs 客户端。
type Client struct {
	cfg  Config
	key  *ecdsa.PrivateKey
	http *http.Client
	sem  chan struct{}
	now  func() time.Time

	tokenMu     sync.Mutex
	token       string
	tokenIssued time.Time

	backoffMu sync.Mutex
	backoff   map[string]backoffState

	mockMu sync.Mutex
	calls  []Call
}

type backoffState struct {
	until time.Time
	delay time.Duration
}

// New 加载 .p8 并构造客户端；Mock 模式不需要密钥。
func New(cfg Config) (*Client, error) {
	if cfg.Environment == "" {
		cfg.Environment = "production"
	}
	switch cfg.Environment {
	case "production", "sandbox":
	default:
		return nil, fmt.Errorf("apns: 未知环境 %q", cfg.Environment)
	}
	if cfg.RetryBase <= 0 {
		cfg.RetryBase = 500 * time.Millisecond
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = 500 * time.Millisecond
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{
			Transport: &http.Transport{
				ForceAttemptHTTP2:   true,
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 32,
				IdleConnTimeout:     90 * time.Second,
			},
			Timeout: 15 * time.Second,
		}
	}
	c := &Client{
		cfg:     cfg,
		http:    cfg.HTTPClient,
		sem:     make(chan struct{}, defaultConcurrency),
		now:     cfg.Now,
		backoff: make(map[string]backoffState),
	}
	if cfg.Mock {
		return c, nil
	}
	if cfg.KeyPath == "" || cfg.KeyID == "" || cfg.TeamID == "" {
		return nil, errors.New("apns: 非 Mock 模式需要 KeyPath、KeyID 与 TeamID")
	}
	pem, err := os.ReadFile(cfg.KeyPath)
	if err != nil {
		return nil, fmt.Errorf("apns: 读取 .p8 失败: %w", err)
	}
	key, err := jwt.ParseECPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("apns: 解析 .p8 失败: %w", err)
	}
	c.key = key
	return c, nil
}

// Mock 表示客户端是否处于 Mock 模式。
func (c *Client) Mock() bool { return c.cfg.Mock }

// Healthy 校验客户端可用：Mock 恒为 nil，真实模式要求 JWT 能签发。
func (c *Client) Healthy() error {
	if c.cfg.Mock {
		return nil
	}
	_, err := c.authToken()
	return err
}

// Calls 返回 Mock 模式下记录的推送副本。
func (c *Client) Calls() []Call {
	c.mockMu.Lock()
	defer c.mockMu.Unlock()
	out := make([]Call, len(c.calls))
	copy(out, c.calls)
	return out
}

// Send 发送一条推送，按错误分类做有限重试。
//
// 返回值约定：HTTP 层错误放在 Result 里（Status/Reason/Prune），
// error 仅表示本地序列化、JWT 或传输失败。
func (c *Client) Send(ctx context.Context, req Request) (Result, error) {
	if req.DeviceToken == "" {
		return Result{}, errors.New("apns: device token 为空")
	}
	if req.Topic == "" {
		return Result{}, errors.New("apns: topic 为空")
	}
	payload, err := json.Marshal(req.Payload)
	if err != nil {
		return Result{}, fmt.Errorf("apns: 序列化载荷失败: %w", err)
	}
	if len(payload) > MaxPayloadBytes {
		return Result{}, fmt.Errorf("%w（%d 字节）", ErrPayloadTooLarge, len(payload))
	}
	if c.cfg.Mock {
		return c.recordMock(req, payload), nil
	}

	// 已有 topic 处于 429 退避窗口时直接跳过，避免放大限流。
	// 注意：窗口只在进入 Send 时检查；本次请求的既有重试不再被自己设置的窗口拦截。
	if c.throttled(req.Topic, c.now()) {
		return Result{Skipped: true, Reason: "topic-backoff"}, nil
	}

	for attempt := 0; ; attempt++ {
		res, err := c.sendOnce(ctx, req, payload)
		if err != nil {
			if attempt < maxAttempts-1 && ctx.Err() == nil {
				if !sleepCtx(ctx, c.cfg.RetryBase<<attempt) {
					return Result{}, ctx.Err()
				}
				continue
			}
			return res, err
		}
		switch {
		case res.Accepted():
			c.resetBackoff(req.Topic)
			return res, nil
		case res.Status == http.StatusTooManyRequests:
			delay := c.bumpBackoff(req.Topic, c.now())
			if attempt < maxAttempts-1 {
				if !sleepCtx(ctx, delay) {
					return res, ctx.Err()
				}
				continue
			}
			return res, nil
		case res.Status >= 500:
			if attempt < maxAttempts-1 {
				if !sleepCtx(ctx, c.cfg.RetryBase<<attempt) {
					return res, ctx.Err()
				}
				continue
			}
			return res, nil
		case res.Status == http.StatusForbidden && res.Reason == "ExpiredProviderToken" && attempt == 0:
			// 本机时钟跳变或缓存 JWT 被拒：强制换新 token 再试一次。
			c.invalidateToken()
			continue
		default:
			// 400 / 403 / 410 等不可重试，交给投递层分类处理。
			return res, nil
		}
	}
}

func (c *Client) recordMock(req Request, payload []byte) Result {
	c.mockMu.Lock()
	defer c.mockMu.Unlock()
	c.calls = append(c.calls, Call{Request: req, Payload: append([]byte(nil), payload...)})
	return Result{Status: http.StatusOK, APNSID: fmt.Sprintf("mock-%d", len(c.calls))}
}

func (c *Client) sendOnce(ctx context.Context, req Request, payload []byte) (Result, error) {
	token, err := c.authToken()
	if err != nil {
		return Result{}, err
	}

	select {
	case c.sem <- struct{}{}:
		defer func() { <-c.sem }()
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}

	url := c.endpoint() + "/3/device/" + req.DeviceToken
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("apns: 构造请求失败: %w", err)
	}
	pushType := req.PushType
	if pushType == "" {
		pushType = "alert"
	}
	priority := req.Priority
	if priority == 0 {
		priority = 10
	}
	hreq.Header.Set("authorization", "bearer "+token)
	hreq.Header.Set("apns-topic", req.Topic)
	hreq.Header.Set("apns-push-type", pushType)
	hreq.Header.Set("apns-priority", strconv.Itoa(priority))
	if req.Expiration > 0 {
		hreq.Header.Set("apns-expiration", strconv.FormatInt(req.Expiration, 10))
	}
	if req.CollapseID != "" {
		hreq.Header.Set("apns-collapse-id", truncateBytes(req.CollapseID, maxCollapseIDBytes))
	}
	hreq.Header.Set("content-type", "application/json")

	resp, err := c.http.Do(hreq)
	if err != nil {
		return Result{}, fmt.Errorf("apns: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	res := Result{
		Status: resp.StatusCode,
		APNSID: resp.Header.Get("apns-id"),
		Prune:  resp.StatusCode == http.StatusGone,
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if len(body) > 0 {
		var e struct {
			Reason string `json:"reason"`
		}
		if json.Unmarshal(body, &e) == nil {
			res.Reason = e.Reason
		}
	}
	return res, nil
}

// authToken 返回缓存的 ES256 JWT，过期后惰性刷新。
func (c *Client) authToken() (string, error) {
	c.tokenMu.Lock()
	defer c.tokenMu.Unlock()
	now := c.now()
	if c.token != "" && now.Before(c.tokenIssued.Add(tokenTTL)) {
		return c.token, nil
	}
	claims := jwt.MapClaims{"iss": c.cfg.TeamID, "iat": now.Unix()}
	t := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	t.Header["kid"] = c.cfg.KeyID
	signed, err := t.SignedString(c.key)
	if err != nil {
		return "", fmt.Errorf("apns: 签发 JWT 失败: %w", err)
	}
	c.token = signed
	c.tokenIssued = now
	return signed, nil
}

func (c *Client) invalidateToken() {
	c.tokenMu.Lock()
	c.token = ""
	c.tokenIssued = time.Time{}
	c.tokenMu.Unlock()
}

func (c *Client) endpoint() string {
	if c.cfg.Endpoint != "" {
		return strings.TrimRight(c.cfg.Endpoint, "/")
	}
	if c.cfg.Environment == "sandbox" {
		return sandboxEndpoint
	}
	return productionEndpoint
}

// TopicFor 返回普通通知的 topic。
func (c *Client) TopicFor() string { return c.cfg.BundleID }

// LiveActivityTopic 返回 Live Activity 推送的 topic。
func (c *Client) LiveActivityTopic() string { return c.cfg.LiveActivityTopic }

func (c *Client) throttled(topic string, now time.Time) bool {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	st, ok := c.backoff[topic]
	return ok && now.Before(st.until)
}

// bumpBackoff 把 topic 的退避窗口翻倍（0.5s 起，最长 30s），返回本次等待时长。
func (c *Client) bumpBackoff(topic string, now time.Time) time.Duration {
	c.backoffMu.Lock()
	defer c.backoffMu.Unlock()
	st := c.backoff[topic]
	if st.delay == 0 {
		st.delay = c.cfg.BackoffBase
	} else {
		st.delay *= 2
	}
	if st.delay > 30*time.Second {
		st.delay = 30 * time.Second
	}
	st.until = now.Add(st.delay)
	c.backoff[topic] = st
	return st.delay
}

func (c *Client) resetBackoff(topic string) {
	c.backoffMu.Lock()
	delete(c.backoff, topic)
	c.backoffMu.Unlock()
}

// sleepCtx 等待 d；ctx 取消时返回 false。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// truncateBytes 按字节截断（不切断 UTF-8 边界）。
func truncateBytes(s string, max int) string {
	if len(s) <= max {
		return s
	}
	b := []byte(s)[:max]
	for len(b) > 0 && !utf8.Valid(b) {
		b = b[:len(b)-1]
	}
	return string(b)
}
