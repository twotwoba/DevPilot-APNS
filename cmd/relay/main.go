// Package main 是 DevPilot relay 的入口。
//
// 子命令：
//
//	relay serve       启动服务（默认）
//	relay healthcheck 容器 HEALTHCHECK 用
//	relay snapshot    导出 bbolt 一致性快照
//	relay version
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/twotwoba/DevPilot-APNS/internal/apns"
	"github.com/twotwoba/DevPilot-APNS/internal/config"
	"github.com/twotwoba/DevPilot-APNS/internal/deliver"
	"github.com/twotwoba/DevPilot-APNS/internal/metrics"
	"github.com/twotwoba/DevPilot-APNS/internal/quota"
	"github.com/twotwoba/DevPilot-APNS/internal/ratelimit"
	"github.com/twotwoba/DevPilot-APNS/internal/server"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
	"github.com/twotwoba/DevPilot-APNS/pkg/wire"
)

// Version 由 goreleaser 注入。
var Version = "dev"

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	cmd := "serve"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd = args[0]
		args = args[1:]
	}
	switch cmd {
	case "serve":
		return runServe(args)
	case "healthcheck":
		return runHealthcheck()
	case "snapshot":
		return runSnapshot(args)
	case "test-push":
		return runTestPush(args)
	case "version", "--version", "-v":
		fmt.Println(Version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `devpilot-relay - DevPilot cloud push relay

Usage:
  devpilot-relay serve       start the relay (configured via environment variables)
  devpilot-relay healthcheck check /healthz (for container HEALTHCHECK)
  devpilot-relay snapshot    export a bbolt snapshot [--out PATH]
  devpilot-relay test-push   send a test push to a real device
                             [--relay URL] [--credential dpd_...] [--kind approval|task]
  devpilot-relay version
`)
}

func runServe(args []string) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	logger := newLogger(cfg.LogLevel)
	now := time.Now

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		logger.Error("failed to create data directory", "err", err)
		return 1
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "relay.db"))
	if err != nil {
		logger.Error("failed to open database", "err", err)
		return 1
	}
	defer st.Close()

	m := metrics.New()

	// 托管多租户配额：事件速率 + 日推送上限,内存态(阈值见 config 注释)。
	eng := quota.New(quota.Config{
		RateEventFree:   cfg.RateEventFree,
		RateEventPro:    cfg.RateEventPro,
		RateEventWindow: cfg.RateEventWindow,
		QuotaDailyFree:  cfg.QuotaDailyFree,
		QuotaDailyPro:   cfg.QuotaDailyPro,
		ExemptApprovals: cfg.QuotaExemptApprovals,
		Now:             now,
	})
	logger.Info("quota settings", "rate_free", cfg.RateEventFree, "rate_pro", cfg.RateEventPro,
		"window", cfg.RateEventWindow.String(), "daily_free", cfg.QuotaDailyFree,
		"daily_pro", cfg.QuotaDailyPro, "exempt_approvals", cfg.QuotaExemptApprovals)

	// P4：APNs 投递。未配置凭据时降级为仅 WSS（App 在线仍可用）。
	var (
		notifier wss.Notifier
		apnsOK   func() error
	)
	if cfg.APNSEnabled() {
		client, err := apns.New(apns.Config{
			KeyPath:           cfg.APNSKeyPath,
			KeyID:             cfg.APNSKeyID,
			TeamID:            cfg.APNSTeamID,
			BundleID:          cfg.APNSBundleID,
			LiveActivityTopic: cfg.APNSLiveActivityTopic,
			Environment:       cfg.APNSEnvironment,
			Mock:              cfg.APNSMock,
		})
		if err != nil {
			logger.Error("failed to initialize APNs client", "err", err)
			return 1
		}
		notifier = deliver.New(deliver.Config{
			Store: st, Pusher: client, Metrics: m, Logger: logger, Quota: eng, Now: now,
			APNSCategory: cfg.APNSCategory, QuotaExemptApprovalPushes: cfg.QuotaExemptApprovalPushes,
		})
		apnsOK = client.Healthy
		logger.Info("APNs enabled", "mock", cfg.APNSMock, "environment", cfg.APNSEnvironment)
	} else {
		logger.Warn("APNs not configured; approvals delivered over WSS only (app may miss notifications while in background)")
	}

	hub := wss.New(wss.Config{
		Store:    st,
		Logger:   logger,
		Metrics:  m,
		Notifier: notifier,
		Quota:    eng,
		Now:      now,
	})
	// 防滥用:fail2ban 式封禁(BAN_ENABLED=false 或阈值为 0 关闭)+ 配对限流。
	var bans *ratelimit.BanManager
	if cfg.BanEnabled {
		bans = ratelimit.NewBanManager(ratelimit.BanConfig{
			Threshold: cfg.BanThreshold,
			Window:    time.Duration(cfg.BanWindowSeconds) * time.Second,
			TTL:       time.Duration(cfg.BanTTLSeconds) * time.Second,
			Now:       now,
		})
	}
	m.BansActive = bans.Active

	ready := func() error {
		if apnsOK == nil {
			return nil
		}
		return apnsOK()
	}
	srv, err := server.New(server.Config{
		Store: st, Hub: hub, Metrics: m, Logger: logger,
		PublicBaseURL: cfg.PublicBaseURL, Now: now, Ready: ready,
		PairLimiter:       ratelimit.New(cfg.PairSessionLimit, time.Minute, now),
		AttemptLimiter:    ratelimit.New(cfg.PairAttemptLimit, time.Minute, now),
		Bans:              bans,
		TrustedProxyCount: cfg.TrustedProxyCount,
		MetricsToken:      cfg.MetricsToken,
	})
	if err != nil {
		logger.Error("failed to initialize server", "err", err)
		return 1
	}

	httpSrv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 周期清理过期审批/收件箱/配对码;新过期的审批由 relay 合成 approval_closed
	// 广播手机(旧 daemon 不发自发 close,过期收敛在此兜底)。
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				expired, err := st.Cleanup(now().Unix())
				if err != nil {
					logger.Warn("failed to clean up expired data", "err", err)
				}
				for _, p := range expired {
					hub.SynthesizeClose(p, wire.CloseReasonExpired)
				}
			}
		}
	}()

	// 身份 TTL 清理(小时级):超期未连接的 daemon 级联注销,0 天=禁用。
	daemonTTL := time.Duration(cfg.DaemonTTLDays) * 24 * time.Hour
	deviceTTL := time.Duration(cfg.DeviceTTLDays) * 24 * time.Hour
	if daemonTTL > 0 || deviceTTL > 0 {
		go func() {
			ticker := time.NewTicker(time.Hour)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					removedD, removedDev, err := st.CleanupIdentities(now().Unix(), daemonTTL, deviceTTL)
					if err != nil {
						logger.Warn("identity TTL cleanup failed", "err", err)
						continue
					}
					if removedD > 0 || removedDev > 0 {
						logger.Info("identity TTL cleanup complete", "daemons", removedD, "devices", removedDev)
					}
				}
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("relay started", "listen", cfg.ListenAddr, "data_dir", cfg.DataDir,
			"apns_enabled", cfg.APNSEnabled())
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("relay exited unexpectedly", "err", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("shutdown signal received, starting graceful shutdown")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("graceful shutdown timed out", "err", err)
		}
	}
	return 0
}

func runHealthcheck() int {
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	url := healthURL(cfg.ListenAddr)
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "healthcheck failed: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "healthcheck returned HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	out := fs.String("out", "", "snapshot output path")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg, err := config.FromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	if *out == "" {
		*out = filepath.Join(cfg.DataDir, "snapshots",
			fmt.Sprintf("relay-%s.db", time.Now().Format("20060102-150405")))
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "relay.db"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer st.Close()
	if err := st.Snapshot(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println(*out)
	return 0
}

// healthURL 把监听地址转换为容器内可访问的 URL。
func healthURL(listen string) string {
	if strings.HasPrefix(listen, ":") {
		return "http://127.0.0.1" + listen + "/healthz"
	}
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "http://" + listen + "/healthz"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/healthz"
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn", "warning":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lv}))
}
