// Package config 从环境变量读取 relay 配置。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config 是 relay 的运行配置。所有字段都可以通过环境变量覆盖。
type Config struct {
	ListenAddr    string
	DataDir       string
	PublicBaseURL string
	LogLevel      string

	APNSKeyPath           string
	APNSKeyID             string
	APNSTeamID            string
	APNSBundleID          string
	APNSLiveActivityTopic string
	APNSEnvironment       string
	APNSMock              bool
	// APNSCategory 是审批通知的 category 标识。默认 DEVPILOT_APPROVAL(修正拼写);
	// 存量 App 只注册旧串 DEVPLOT_APPROVAL,App 新版上线前部署侧应显式设旧值,
	// 新版铺开后再移除该 env 回归默认。
	APNSCategory string

	// —— 托管多租户:per-daemon 配额(0=关闭对应闸)——
	RateEventFree   int           // free 档每窗口事件数
	RateEventPro    int           // pro 档每窗口事件数
	RateEventWindow time.Duration // 令牌桶补充窗口
	QuotaDailyFree  int64         // free 档 UTC 日推送上限
	QuotaDailyPro   int64         // pro 档 UTC 日推送上限
	// QuotaExemptApprovals 时审批类事件豁免速率闸(不豁免日配额)。
	QuotaExemptApprovals bool
	// QuotaExemptApprovalPushes 时审批范围推送(approval alert + 云审批 LA)豁免
	// UTC 日配额:免费档 10 条/日几分钟就会被 LA 更新烧穿,旗舰审批不能被配额哑掉。
	// 关闭则恢复全面计数(遥测 LA 始终计数)。
	QuotaExemptApprovalPushes bool

	// —— 生命周期 TTL(天;0=禁用,首发部署保持 0 观察)——
	DaemonTTLDays int
	DeviceTTLDays int

	// —— 防滥用与运维 ——
	TrustedProxyCount int // TRUSTED_PROXY_COUNT 可信反代数(Caddy 部署设 1;0=忽略 XFF)
	PairSessionLimit  int // 每 IP 每分钟配对会话登记
	PairAttemptLimit  int // 每 IP 每分钟配对尝试
	BanEnabled        bool
	BanThreshold      int   // 窗口内 4xx/5xx 次数阈值
	BanWindowSeconds  int64 // 失败计数窗口
	BanTTLSeconds     int64 // 封禁时长
	MetricsToken      string
}

// FromEnv 读取环境变量并做基本校验。
func FromEnv() (*Config, error) {
	c := &Config{
		ListenAddr:            env("LISTEN_ADDR", ":8080"),
		DataDir:               env("DATA_DIR", "./data"),
		PublicBaseURL:         strings.TrimRight(env("PUBLIC_BASE_URL", ""), "/"),
		LogLevel:              env("LOG_LEVEL", "info"),
		APNSKeyPath:           env("APNS_KEY_PATH", ""),
		APNSKeyID:             env("APNS_KEY_ID", ""),
		APNSTeamID:            env("APNS_TEAM_ID", ""),
		APNSBundleID:          env("APNS_BUNDLE_ID", "com.twotwoba.devpilot"),
		APNSLiveActivityTopic: env("APNS_LIVEACTIVITY_TOPIC", "com.twotwoba.devpilot.push-type.liveactivity"),
		APNSEnvironment:       env("APNS_ENVIRONMENT", "production"),
		APNSCategory:          env("APNS_CATEGORY", "DEVPILOT_APPROVAL"),
	}
	mock, err := strconv.ParseBool(env("APNS_MOCK", "false"))
	if err != nil {
		return nil, fmt.Errorf("config: APNS_MOCK is not a valid boolean: %w", err)
	}
	c.APNSMock = mock

	// 配额参数:锚点 free 10 事件/60s + 500 推送/天,Pro 60/60s + 5000/天。
	if c.RateEventFree, err = envInt("RATE_EVENT_FREE", 10); err != nil {
		return nil, err
	}
	if c.RateEventPro, err = envInt("RATE_EVENT_PRO", 60); err != nil {
		return nil, err
	}
	var windowSec int64
	if windowSec, err = envInt64("RATE_EVENT_WINDOW_SECONDS", 60); err != nil {
		return nil, err
	}
	c.RateEventWindow = time.Duration(windowSec) * time.Second
	if c.QuotaDailyFree, err = envInt64("QUOTA_DAILY_FREE", 500); err != nil {
		return nil, err
	}
	if c.QuotaDailyPro, err = envInt64("QUOTA_DAILY_PRO", 5000); err != nil {
		return nil, err
	}
	if c.QuotaExemptApprovals, err = envBool("QUOTA_EXEMPT_APPROVALS", true); err != nil {
		return nil, err
	}
	if c.QuotaExemptApprovalPushes, err = envBool("QUOTA_EXEMPT_APPROVAL_PUSHES", true); err != nil {
		return nil, err
	}
	if c.DaemonTTLDays, err = envInt("DAEMON_TTL_DAYS", 0); err != nil {
		return nil, err
	}
	if c.DeviceTTLDays, err = envInt("DEVICE_TTL_DAYS", 0); err != nil {
		return nil, err
	}

	// 防滥用参数:锚点 Bark「5min 1000 次失败 → 封 24h」,阈值放宽到 100 次防误伤共享 NAT。
	if c.TrustedProxyCount, err = envInt("TRUSTED_PROXY_COUNT", 0); err != nil {
		return nil, err
	}
	if c.PairSessionLimit, err = envInt("PAIR_SESSION_LIMIT", 15); err != nil {
		return nil, err
	}
	if c.PairAttemptLimit, err = envInt("PAIR_ATTEMPT_LIMIT", 10); err != nil {
		return nil, err
	}
	if c.BanEnabled, err = envBool("BAN_ENABLED", true); err != nil {
		return nil, err
	}
	if c.BanThreshold, err = envInt("BAN_THRESHOLD", 100); err != nil {
		return nil, err
	}
	if c.BanWindowSeconds, err = envInt64("BAN_WINDOW_SECONDS", 300); err != nil {
		return nil, err
	}
	if c.BanTTLSeconds, err = envInt64("BAN_TTL_SECONDS", 86400); err != nil {
		return nil, err
	}
	c.MetricsToken = env("METRICS_TOKEN", "")

	switch c.APNSEnvironment {
	case "production", "sandbox":
	default:
		return nil, fmt.Errorf("config: APNS_ENVIRONMENT must be production or sandbox")
	}
	if c.ListenAddr == "" {
		return nil, fmt.Errorf("config: LISTEN_ADDR must not be empty")
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("config: DATA_DIR must not be empty")
	}
	return c, nil
}

// APNSEnabled 表示是否具备真实推送凭据。
func (c *Config) APNSEnabled() bool {
	return c.APNSMock || (c.APNSKeyPath != "" && c.APNSKeyID != "" && c.APNSTeamID != "")
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

// envInt 解析整型环境变量;0 是合法值(表示关闭对应闸)。
func envInt(key string, def int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid integer: %w", key, err)
	}
	return v, nil
}

func envInt64(key string, def int64) (int64, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s is not a valid integer: %w", key, err)
	}
	return v, nil
}

func envBool(key string, def bool) (bool, error) {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return false, fmt.Errorf("config: %s is not a valid boolean: %w", key, err)
	}
	return v, nil
}
