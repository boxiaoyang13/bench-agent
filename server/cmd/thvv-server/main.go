// Command thvv-server 是 THVV 的 HTTP 服务端（Go 薄封装 Python 引擎）。
//
// 用法:
//
//	thvv-server -config ./config/thvv.yaml
//	thvv-server -config ./config/thvv.yaml -check            # 只校验配置不启动
//	thvv-server -config ./config/thvv.yaml -thvv-home /opt/thvv
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/apexcloud/thvv/server/internal/config"
	"github.com/apexcloud/thvv/server/internal/engine"
	"github.com/apexcloud/thvv/server/internal/httpapi"
	"github.com/apexcloud/thvv/server/internal/runmgr"
	"github.com/apexcloud/thvv/server/internal/secret"
	"github.com/apexcloud/thvv/server/internal/store"
)

var (
	version = "1.0.0"
	commit  = "dev"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "\n✕ %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		cfgPath  = flag.String("config", "./config/thvv.yaml", "配置文件路径")
		thvvHome = flag.String("thvv-home", "", "覆盖 roots.thvv（THVV Python 工具链根目录）")
		listen   = flag.String("listen", "", "覆盖 server.listen")
		logLevel = flag.String("log-level", "", "覆盖 log_level（debug|info|warn|error）")
		check    = flag.Bool("check", false, "只校验配置与运行环境后退出")
		showVer  = flag.Bool("version", false, "打印版本并退出")
		roots    = flag.Bool("print-roots", false, "打印解析后的路径表并退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Printf("thvv-server %s (%s, %s/%s, %s)\n", version, commit, runtime.GOOS, runtime.GOARCH, runtime.Version())
		return nil
	}

	cfg, err := config.Load(config.LoadOptions{
		Path: *cfgPath, ThvvHome: *thvvHome, Listen: *listen, LogLevel: *logLevel,
	})
	if err != nil {
		return err
	}

	logger := newLogger(cfg)

	if *roots {
		printRoots(cfg)
		return nil
	}

	// -check：跑完校验与引擎自检后退出
	if *check {
		return doCheck(cfg, logger)
	}

	// 建库
	if err := os.MkdirAll(filepath.Dir(string(cfg.Store.SQLite.Path)), 0o755); err != nil {
		return fmt.Errorf("创建数据目录失败: %w", err)
	}
	st, err := store.Open(string(cfg.Store.SQLite.Path), cfg.Store.SQLite.BusyTimeout, cfg.Store.SQLite.MaxOpenConns)
	if err != nil {
		return fmt.Errorf("打开数据库失败: %w", err)
	}
	defer st.Close()

	box, err := secret.New(cfg.Secrets.MasterKey)
	if err != nil {
		return fmt.Errorf("初始化凭据加密失败: %w", err)
	}

	reg := engine.NewRegistry(cfg)
	mgr := runmgr.New(cfg, st, reg, box, logger)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	for _, w := range reg.SelfCheck(ctx) {
		logger.Warn(w)
	}
	mgr.Start(ctx)

	srv := httpapi.New(cfg, st, reg, mgr, box, logger, version, commit)
	httpSrv := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Server.ReadTimeout,
		WriteTimeout:      cfg.Server.WriteTimeout,
		IdleTimeout:       cfg.Server.IdleTimeout,
	}

	banner(cfg, logger)

	errCh := make(chan error, 1)
	go func() {
		var err error
		if cfg.Server.TLS.Enabled {
			err = httpSrv.ListenAndServeTLS(cfg.Server.TLS.CertFile, cfg.Server.TLS.KeyFile)
		} else {
			err = httpSrv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	for {
		select {
		case err := <-errCh:
			return fmt.Errorf("HTTP 服务异常: %w", err)
		case sig := <-sigCh:
			if sig == syscall.SIGHUP {
				// v1 不支持热加载，只提示
				logger.Warn("收到 SIGHUP：v1 不支持热加载配置，请重启进程")
				continue
			}
			logger.Info("收到退出信号，开始优雅关闭", "signal", sig.String())
			shutdownCtx, sc := context.WithTimeout(context.Background(), cfg.Server.ShutdownTimeout)
			_ = httpSrv.Shutdown(shutdownCtx)
			sc()
			cancel()
			mgr.Shutdown(shutdownCtx)
			logger.Info("已退出")
			return nil
		}
	}
}

func newLogger(cfg *config.Config) *slog.Logger {
	lvl := slog.LevelInfo
	switch strings.ToLower(cfg.Observability.LogLevel) {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	}
	opts := &slog.HandlerOptions{Level: lvl}
	var h slog.Handler
	if cfg.Observability.LogFormat == "json" {
		h = slog.NewJSONHandler(os.Stdout, opts)
	} else {
		h = slog.NewTextHandler(os.Stdout, opts)
	}
	return slog.New(h)
}

// printRoots 输出解析后的根与路径表（-print-roots / -check 共用）。
func printRoots(cfg *config.Config) {
	fmt.Println("根目录（roots）:")
	keys := make([]string, 0, len(cfg.Resolved))
	for k := range cfg.Resolved {
		if strings.HasPrefix(k, "roots.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("  %-22s = %s\n", strings.TrimPrefix(k, "roots."), cfg.Resolved[k])
	}
	fmt.Println("派生路径:")
	keys = keys[:0]
	for k := range cfg.Resolved {
		if !strings.HasPrefix(k, "roots.") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		state := "exists"
		if _, err := os.Stat(cfg.Resolved[k]); err != nil {
			state = "will create / missing"
		}
		fmt.Printf("  %-46s = %-52s (%s)\n", k, cfg.Resolved[k], state)
	}
	fmt.Printf("配置目录: %s\n", cfg.ConfigDir)
}

func doCheck(cfg *config.Config, log *slog.Logger) error {
	printRoots(cfg)

	issues := cfg.Check()
	var errs, warns int
	for _, is := range issues {
		if is.Level == "error" {
			errs++
		} else {
			warns++
		}
		fmt.Println(is.String())
	}

	fmt.Printf("\n引擎:\n")
	reg := engine.NewRegistry(cfg)
	for _, t := range reg.Tools() {
		fmt.Printf("  %-12s kind=%-10s v%-8s\n", t.Name, t.Kind, t.Version)
		for _, c := range t.Capabilities {
			fmt.Printf("      %-11s status=%-11s %s\n", c.Name, c.Status, c.Description)
		}
	}

	fmt.Printf("\n凭据: provider=%s 掩码指纹=%s\n",
		cfg.Secrets.Provider, secret.Fingerprint(cfg.Secrets.MasterKey))
	fmt.Printf("数据库: %s\n", cfg.Store.SQLite.Path)
	fmt.Printf("监听: %s  base_path=%s\n", cfg.Server.Listen, cfg.Server.BasePath)

	if errs > 0 {
		return fmt.Errorf("配置校验失败：%d 个 error，%d 个 warn", errs, warns)
	}
	fmt.Printf("\n✓ 校验通过（%d 个 warn）\n", warns)
	return nil
}

func banner(cfg *config.Config, log *slog.Logger) {
	log.Info("THVV Server 启动",
		"version", version, "commit", commit,
		"listen", cfg.Server.Listen, "base_path", cfg.Server.BasePath,
		"ui", cfg.Server.UI.Path, "db", cfg.Store.SQLite.Path)
	if v, ok := cfg.Resolved["roots.thvv"]; ok {
		log.Info("THVV 工具链", "roots.thvv", v)
	}
	log.Info("凭据加密", "provider", cfg.Secrets.Provider, "fingerprint", secret.Fingerprint(cfg.Secrets.MasterKey))
	log.Info("限额", "max_concurrent_runs", cfg.Limits.MaxConcurrentRuns,
		"queue", cfg.Limits.MaxQueuedRuns, "max_run_duration", cfg.Limits.MaxRunDuration.String())
	if cfg.Server.WriteTimeout > 0 {
		log.Warn("server.write_timeout 非 0，SSE 进度流可能被中断")
	}
	_ = time.Now
}
