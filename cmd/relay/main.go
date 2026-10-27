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
	"github.com/twotwoba/DevPilot-APNS/internal/server"
	"github.com/twotwoba/DevPilot-APNS/internal/store"
	"github.com/twotwoba/DevPilot-APNS/internal/wss"
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
	case "version", "--version", "-v":
		fmt.Println(Version)
		return 0
	case "help", "--help", "-h":
		usage()
		return 0
	default:
		fmt.Fprintf(os.Stderr, "未知命令: %s\n\n", cmd)
		usage()
		return 2
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `devpilot-relay —— DevPilot 云端推送中继

用法:
  devpilot-relay serve       启动 relay（环境变量配置）
  devpilot-relay healthcheck 检查 /healthz（容器用）
  devpilot-relay snapshot    导出 bbolt 快照 [--out PATH]
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
		logger.Error("创建数据目录失败", "err", err)
		return 1
	}
	st, err := store.Open(filepath.Join(cfg.DataDir, "relay.db"))
	if err != nil {
		logger.Error("打开数据库失败", "err", err)
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
	logger.Info("配额参数", "rate_free", cfg.RateEventFree, "rate_pro", cfg.RateEventPro,
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
			logger.Error("初始化 APNs 客户端失败", "err", err)
			return 1
		}
		notifier = deliver.New(deliver.Config{
			Store: st, Pusher: client, Metrics: m, Logger: logger, Quota: eng, Now: now,
		})
		apnsOK = client.Healthy
		logger.Info("APNs 已启用", "mock", cfg.APNSMock, "environment", cfg.APNSEnvironment)
	} else {
		logger.Warn("APNs 未配置，审批仅走 WSS（App 后台可能收不到通知）")
	}

	hub := wss.New(wss.Config{
		Store:    st,
		Logger:   logger,
		Metrics:  m,
		Notifier: notifier,
		Quota:    eng,
		Now:      now,
	})
	ready := func() error {
		if apnsOK == nil {
			return nil
		}
		return apnsOK()
	}
	srv, err := server.New(server.Config{
		Store: st, Hub: hub, Metrics: m, Logger: logger,
		PublicBaseURL: cfg.PublicBaseURL, Now: now, Ready: ready,
	})
	if err != nil {
		logger.Error("初始化 server 失败", "err", err)
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

	// 周期清理过期审批/收件箱/配对码。
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := st.Cleanup(now().Unix()); err != nil {
					logger.Warn("清理过期数据失败", "err", err)
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
						logger.Warn("身份 TTL 清理失败", "err", err)
						continue
					}
					if removedD > 0 || removedDev > 0 {
						logger.Info("身份 TTL 清理完成", "daemons", removedD, "devices", removedDev)
					}
				}
			}
		}()
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("relay 启动", "listen", cfg.ListenAddr, "data_dir", cfg.DataDir,
			"apns_enabled", cfg.APNSEnabled())
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			logger.Error("relay 异常退出", "err", err)
			return 1
		}
	case <-ctx.Done():
		logger.Info("收到退出信号，开始优雅关闭")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("优雅关闭超时", "err", err)
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
		fmt.Fprintf(os.Stderr, "健康检查失败: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "健康检查 HTTP %d\n", resp.StatusCode)
		return 1
	}
	return 0
}

func runSnapshot(args []string) int {
	fs := flag.NewFlagSet("snapshot", flag.ContinueOnError)
	out := fs.String("out", "", "快照输出路径")
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
