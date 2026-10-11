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
	"github.com/tutic/itiao-mall/services/api/internal/platform/profiling"
	"github.com/tutic/itiao-mall/services/api/internal/ratelimit"
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

	// 限流器。nil 表示整体关闭（配置显式关掉时）。
	//
	// 关闭的判定刻意放在这里而不是 NewRouter 内部：限流器需要一个 Redis 客户端，
	// 而 NewRouter 的 Deps 里已经有 Redis —— 但把"要不要建"这个决定
	// 藏进路由组装里，会让"为什么这次没限流"变成一个要读两处代码才能回答的问题。
	var limiter *ratelimit.Limiter
	if cfg.RateLimitEnabled {
		// 刻意用**专用** Redis 客户端而不是上面那个业务客户端。
		// 原因见 ratelimit.New 的注释：go-redis 的 ctx deadline 拦不住
		// 阻塞中的 socket 读，真正把单次判定钉在几百毫秒上的是
		// 客户端自己的 ReadTimeout。而业务客户端的 ReadTimeout 是 3s
		// （为 refresh 轮换定的），共用它会让限流在 Redis 卡顿时
		// 把每个请求拖满 3 秒 —— 准入控制反而成了最大的一笔延迟。
		rlRedis := ratelimit.NewDedicatedRedis(
			cfg.RedisAddr, cfg.RedisPass, cfg.RedisDB, cfg.RateLimitTimeout)
		defer func() { _ = rlRedis.Close() }()

		limiter = ratelimit.New(rlRedis, "rl:"+cfg.AppEnv, cfg.RateLimitTimeout, cfg.RateLimitFailsClosed())
		logger.Info("限流已启用",
			slog.Bool("fails_closed", cfg.RateLimitFailsClosed()),
			slog.Int("readwrite_limit", cfg.RateLimitMax),
			slog.String("readwrite_window", cfg.RateLimitWindow.String()),
			slog.Int("auth_limit", cfg.RateLimitAuthMax),
			slog.String("auth_window", cfg.RateLimitAuthWindow.String()),
			slog.String("judge_timeout", cfg.RateLimitTimeout.String()),
		)
	} else {
		logger.Warn("限流已关闭：登录接口将没有频率限制，仅可用于本地开发")
	}

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

	// 把"正在跑 mock"这件事写进启动日志。
	//
	// 配置校验已经保证了生产环境不会走到这里（除非显式开了逃生门），
	// 但**日志是给事后复盘的人看的**：一个跑着 mock 支付的服务，
	// 在日志里必须和跑真实支付的长得不一样。否则排查"这笔订单为什么没收到钱"
	// 时，第一屏看不出任何异常。
	if cfg.MockProvidersAllowed() {
		attrs := []any{
			slog.String("auth_provider", cfg.AuthProvider),
			slog.String("payment_provider", cfg.PaymentProvider),
			slog.String("env", cfg.AppEnv),
		}
		if cfg.IsProd() {
			logger.Warn("⚠ 生产环境正在使用 mock provider（由 ALLOW_MOCK_PROVIDERS_IN_PROD 显式开启）"+
				"：任意 code 可登录、任意订单可被标记为已支付。切勿用于真实业务。", attrs...)
		} else {
			logger.Info("使用 mock provider（开发环境）", attrs...)
		}
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
		RefreshTTL:   cfg.RefreshTTL,
		Limiter:      limiter,
		RateLimits:   ratelimit.Defaults{Window: cfg.RateLimitWindow, Limit: cfg.RateLimitMax},
		AuthLimits:   ratelimit.Defaults{Window: cfg.RateLimitAuthWindow, Limit: cfg.RateLimitAuthMax},

		CORSOrigins:    cfg.CORSOrigins(),
		CORSAllowLocal: cfg.CORSAllowLocalhost(),
		MaxBodyBytes:   cfg.MaxBodyBytes,
	})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		// Go 默认 1 MiB。主动收到 16 KiB：每个连接都要为 header 预留读缓冲，
		// 而上限直接乘在连接数上。HTTP 头里装不下任何合法业务数据，
		// 16 KiB 对一个带 JWT 的请求仍有几十倍余量。
		MaxHeaderBytes: cfg.MaxHeaderBytes,
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

	// 剖析端口。刻意是**独立的 http.Server**，理由见 profiling 包的文档注释。
	//
	// 启动日志走 Warn 而不是 Info：它的作用是让"这个进程此刻可以被读到内存"
	// 这件事在日志里显眼。事故复盘时，事后才知道半小时前能抓 profile 毫无用处。
	var pprofSrv *http.Server
	if cfg.PprofEnabled() {
		pprofSrv = profiling.NewServer(cfg.PprofAddr, cfg.PprofMaxSeconds)
		go func() {
			logger.Warn("⚠ 性能剖析端点已启用",
				slog.String("addr", pprofSrv.Addr),
				slog.Int("max_seconds", cfg.PprofMaxSeconds),
				slog.Any("profiles", profiling.ProfileNames()),
				slog.Any("needs_sampling_rate_enabled", profiling.BlockedProfiles()),
			)
			if err := pprofSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				// 剖析端口起不来**不影响**主服务：它只影响排障能力。
				// 但必须打出来 —— 否则现象是"我配了 PPROF_ADDR 却抓不到 profile"，
				// 而配置、启动日志、实际监听三处都显示一切正常。
				logger.Error("性能剖析端点启动失败（主服务不受影响，但排障时将无法抓 profile）",
					slog.Any("err", err))
			}
		}()
	}

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
	// 剖析端口也要关。不关的话进程退出前会挂着一个空等的 listener，
	// 而此时主服务已经不再接受请求 —— 现象是"关闭时抓 profile 超时"，
	// 会被误读成 profile 端点有问题。
	if pprofSrv != nil {
		if err := pprofSrv.Shutdown(shutdownCtx); err != nil {
			logger.Warn("剖析端点关闭失败", slog.Any("err", err))
		}
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
