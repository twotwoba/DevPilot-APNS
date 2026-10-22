// Package config 从环境变量读取 relay 配置。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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
	}
	mock, err := strconv.ParseBool(env("APNS_MOCK", "false"))
	if err != nil {
		return nil, fmt.Errorf("config: APNS_MOCK 不是合法布尔值: %w", err)
	}
	c.APNSMock = mock

	switch c.APNSEnvironment {
	case "production", "sandbox":
	default:
		return nil, fmt.Errorf("config: APNS_ENVIRONMENT 只能是 production 或 sandbox")
	}
	if c.ListenAddr == "" {
		return nil, fmt.Errorf("config: LISTEN_ADDR 不能为空")
	}
	if c.DataDir == "" {
		return nil, fmt.Errorf("config: DATA_DIR 不能为空")
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
