package config

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
)

// Config 是服务的全部运行期配置，全部来自环境变量。
// 缺必填项或取值非法时在启动阶段直接失败（fail-fast），不做静默兜底。
type Config struct {
	AppEnv   string `env:"APP_ENV" envDefault:"dev"`
	HTTPAddr string `env:"HTTP_ADDR" envDefault:":8080"`
	LogLevel string `env:"LOG_LEVEL" envDefault:"info"`

	DatabaseURL string `env:"DATABASE_URL" envDefault:"postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable"`
	RedisAddr   string `env:"REDIS_ADDR" envDefault:"localhost:6379"`
	RedisPass   string `env:"REDIS_PASSWORD" envDefault:""`
	RedisDB     int    `env:"REDIS_DB" envDefault:"0"`

	// AuthProvider: mock | wechat。默认 mock —— 没有微信开放平台企业资质时走这条。
	AuthProvider string        `env:"AUTH_PROVIDER" envDefault:"mock"`
	JWTSecret    string        `env:"JWT_SECRET" envDefault:"dev-only-secret-please-change"`
	AccessTTL    time.Duration `env:"ACCESS_TOKEN_TTL" envDefault:"15m"`
	RefreshTTL   time.Duration `env:"REFRESH_TOKEN_TTL" envDefault:"720h"` // 30d

	WeChatAppID     string `env:"WECHAT_APP_ID" envDefault:""`
	WeChatAppSecret string `env:"WECHAT_APP_SECRET" envDefault:""`

	// PaymentProvider: mock | wechat
	PaymentProvider string `env:"PAYMENT_PROVIDER" envDefault:"mock"`

	// 未支付订单的超时时长，超时后取消并释放库存。
	OrderPendingTTL time.Duration `env:"ORDER_PENDING_TTL" envDefault:"15m"`

	// CORSAllowedOrigins 是允许跨域的精确 origin，逗号分隔（如 http://localhost:5173,https://m.example.com）。
	//
	// 留空时按环境分流：dev 放行任意 localhost / 127.0.0.1 / [::1] 端口
	// （Expo web 与 Vite 每次端口都可能不同，逐个登记不现实）；
	// prod 留空等于**不允许任何跨域** —— 要开放必须显式配置，不替使用方猜。
	CORSAllowedOrigins string `env:"CORS_ALLOWED_ORIGINS" envDefault:""`
}

// Load 读取并校验配置。
func Load() (*Config, error) {
	cfg := &Config{}
	if err := env.Parse(cfg); err != nil {
		return nil, fmt.Errorf("解析环境变量失败: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) validate() error {
	switch c.AuthProvider {
	case "mock", "wechat":
	default:
		return fmt.Errorf("AUTH_PROVIDER 取值非法: %q（只允许 mock 或 wechat）", c.AuthProvider)
	}
	switch c.PaymentProvider {
	case "mock", "wechat":
	default:
		return fmt.Errorf("PAYMENT_PROVIDER 取值非法: %q（只允许 mock 或 wechat）", c.PaymentProvider)
	}

	// 走真实微信链路时，凭证必须是齐的 —— 否则启动即失败，不要等到第一次登录才炸。
	if c.AuthProvider == "wechat" {
		if c.WeChatAppID == "" || c.WeChatAppSecret == "" {
			return fmt.Errorf("AUTH_PROVIDER=wechat 时必须提供 WECHAT_APP_ID 与 WECHAT_APP_SECRET")
		}
	}

	// 生产环境不允许用默认密钥。
	if c.IsProd() && c.JWTSecret == "dev-only-secret-please-change" {
		return fmt.Errorf("生产环境必须显式设置 JWT_SECRET")
	}

	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL 不能为空")
	}
	if c.AccessTTL <= 0 || c.RefreshTTL <= 0 || c.OrderPendingTTL <= 0 {
		return fmt.Errorf("ACCESS_TOKEN_TTL / REFRESH_TOKEN_TTL / ORDER_PENDING_TTL 必须为正数")
	}
	return nil
}

func (c *Config) IsProd() bool { return strings.EqualFold(c.AppEnv, "prod") }
func (c *Config) IsDev() bool  { return !c.IsProd() }

// CORSOrigins 把逗号分隔的配置拆成列表。
func (c *Config) CORSOrigins() []string {
	if strings.TrimSpace(c.CORSAllowedOrigins) == "" {
		return nil
	}
	parts := strings.Split(c.CORSAllowedOrigins, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// CORSAllowLocalhost 仅在非生产环境打开"放行 localhost 任意端口"的便利开关。
func (c *Config) CORSAllowLocalhost() bool { return c.IsDev() }

// SlogLevel 把字符串日志级别映射到 slog 级别。
func (c *Config) SlogLevel() slog.Level {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
