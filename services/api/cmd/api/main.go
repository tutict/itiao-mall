package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/config"
	"github.com/tutic/itiao-mall/services/api/internal/httpapi"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/platform/cache"
	"github.com/tutic/itiao-mall/services/api/internal/platform/logging"
	"github.com/tutic/itiao-mall/services/api/internal/platform/migrate"
	"github.com/tutic/itiao-mall/services/api/internal/platform/postgres"
)

const usage = `itiao-mall API

用法:
  api serve        启动 HTTP 服务
  api migrate      执行数据库迁移（前滚）
  api version      打印版本信息
  api healthcheck  探测本机 /healthz（供容器 healthcheck 使用）
`

var (
	version = "dev"
	commit  = "none"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}

	cmd := os.Args[1]
	// healthcheck 必须能在配置不完整时也跑起来，所以单独处理，不加载完整配置。
	if cmd == "healthcheck" {
		os.Exit(runHealthcheck())
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "配置加载失败: %v\n", err)
		os.Exit(1)
	}
	logger := logging.New(cfg.SlogLevel(), cfg.IsDev())

	switch cmd {
	case "serve":
		if err := runServe(cfg, logger); err != nil {
			logger.Error("服务退出", slog.Any("err", err))
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrate(cfg, logger); err != nil {
			logger.Error("迁移失败", slog.Any("err", err))
			os.Exit(1)
		}
	case "version":
		fmt.Printf("version=%s commit=%s\n", version, commit)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
}

func runMigrate(cfg *config.Config, logger *slog.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pool, err := postgres.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	logger.Info("开始执行迁移")
	if err := migrate.Up(ctx, pool, logger); err != nil {
		return err
	}
	v, err := migrate.Version(ctx, pool)
	if err != nil {
		return err
	}
	logger.Info("迁移完成", slog.Int64("version", v))
	return nil
}

func runServe(cfg *config.Config, logger *slog.Logger) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := postgres.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer pool.Close()

	rdb, err := cache.New(ctx, cfg.RedisAddr, cfg.RedisPass, cfg.RedisDB)
	if err != nil {
		return err
	}
	defer func() { _ = rdb.Close() }()

	issuer := auth.NewTokenIssuer(cfg.JWTSecret, cfg.AccessTTL)
	refreshStore := auth.NewRefreshStore(rdb, cfg.RefreshTTL)

	var wechat auth.WechatExchanger
	switch cfg.AuthProvider {
	case "wechat":
		wechat = auth.NewWechatExchanger(cfg.WeChatAppID, cfg.WeChatAppSecret)
	default:
		wechat = auth.MockExchanger{}
	}

	var payProvider payment.Provider
	switch cfg.PaymentProvider {
	case "wechat":
		// 真实微信支付实现待资质到位后补 —— 在此之前显式失败，
		// 而不是静默退回 mock 让线上"假装付成功了"。
		return fmt.Errorf("PAYMENT_PROVIDER=wechat 尚未实现：需要微信支付商户号与 APIv3 证书")
	default:
		payProvider = payment.NewMock()
	}

	handler := httpapi.NewRouter(httpapi.Deps{
		Logger:       logger,
		Pool:         pool,
		Redis:        rdb,
		Issuer:       issuer,
		RefreshStore: refreshStore,
		Wechat:       wechat,
		Payment:      payProvider,
		PendingTTL:   cfg.OrderPendingTTL,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("HTTP 服务已启动",
			slog.String("addr", cfg.HTTPAddr),
			slog.String("env", cfg.AppEnv),
			slog.String("auth_provider", cfg.AuthProvider),
			slog.String("payment_provider", cfg.PaymentProvider),
			slog.String("version", version),
		)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return fmt.Errorf("HTTP 服务异常: %w", err)
	case sig := <-sigCh:
		logger.Info("收到退出信号，开始优雅关闭", slog.String("signal", sig.String()))
	}

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("优雅关闭失败: %w", err)
	}
	logger.Info("服务已停止")
	return nil
}

// runHealthcheck 探测本机 /healthz。容器 healthcheck 用它，
// 这样镜像里不需要额外装 curl/wget。
func runHealthcheck() int {
	addr := os.Getenv("HTTP_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "解析 HTTP_ADDR 失败: %v\n", err)
		return 1
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}

	url := fmt.Sprintf("http://%s/healthz", net.JoinHostPort(host, port))
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		fmt.Fprintf(os.Stderr, "健康探测失败: %v\n", err)
		return 1
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		fmt.Fprintf(os.Stderr, "健康探测返回 %d\n", resp.StatusCode)
		return 1
	}
	return 0
}
