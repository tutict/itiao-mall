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

	// AllowMockProvidersInProd 是显式逃生门：在生产环境下允许 mock 认证/支付。
	//
	// 它存在的唯一理由是「演示环境 / 客户验收环境」，那种场合确实需要
	// 不接微信也能跑通全链路。默认关闭，且打开时必须显式设置并会在启动日志里
	// 打 Warn —— 让人看得见这是一个刻意的选择，而不是忘记配。
	//
	// 命名刻意笨重：任何人在 grep 到它的时候，都应该立刻明白自己正在降低安全等级。
	AllowMockProvidersInProd bool `env:"ALLOW_MOCK_PROVIDERS_IN_PROD" envDefault:"false"`

	// 未支付订单的超时时长，超时后取消并释放库存。
	OrderPendingTTL time.Duration `env:"ORDER_PENDING_TTL" envDefault:"15m"`

	// ============================ 限流 ============================
	//
	// 三个维度分开配，因为它们的**攻击形态**不一样：
	//   - 认证类（登录/注册/刷新）阈值低、窗口短：防的是撞库与暴力破解，
	//     它的代价是"用户输错几次密码就被挡"，所以不能太紧。
	//   - 读写业务类阈值高、窗口中等：防的是脚本刷单与爬取，
	//     误伤成本低，可以给紧一点。
	// 全局 RATE_LIMIT_* 是读写业务的默认值，认证类用自己的两项。
	RateLimitEnabled    bool          `env:"RATE_LIMIT_ENABLED" envDefault:"true"`
	RateLimitWindow     time.Duration `env:"RATE_LIMIT_WINDOW" envDefault:"1m"`
	RateLimitMax        int           `env:"RATE_LIMIT_MAX" envDefault:"120"`
	RateLimitAuthWindow time.Duration `env:"RATE_LIMIT_AUTH_WINDOW" envDefault:"1m"`
	RateLimitAuthMax    int           `env:"RATE_LIMIT_AUTH_MAX" envDefault:"10"`

	// 单次限流判定自身的超时。刻意做得很短（几百毫秒）：限流是**准入控制**，
	// 它拖慢请求的代价由所有正常用户承担，而它自己慢一点最多放过几个请求。
	RateLimitTimeout time.Duration `env:"RATE_LIMIT_TIMEOUT" envDefault:"300ms"`

	// RateLimitFailMode 决定 Redis 不可用时怎么办：auto | open | closed。
	//
	// 这是一笔明确的取舍，不是"随便选一个"：
	//   - closed：Redis 挂 ⇒ 全站 503。安全上无缺口，代价是可用性完全绑死在 Redis 上。
	//   - open：Redis 挂 ⇒ 限流整体失效，放行一切。业务可用，但暴力破解与刷单同时回归。
	// 之所以敢让 open 成为开发环境的默认：本地没 Redis 时服务仍能跑通全链路，
	// 而生产用 auto ⇒ closed（Redis 本来就是硬依赖，readyz 已经在查它了）。
	RateLimitFailMode string `env:"RATE_LIMIT_FAIL_MODE" envDefault:"auto"`

	// ============================ token 签发并发 ============================
	//
	// 一次登录/刷新里有多步彼此无依赖的 IO（校验 refresh 记录、签 access token、
	// 写 Redis、写 DB）。串行做等于把它们的延迟相加，并行则取最大值。
	// 但并发必须有上限，否则一次刷新风暴会把连接池打满并把正常请求一起拖死。
	TokenIOConcurrency int `env:"TOKEN_IO_CONCURRENCY" envDefault:"4"`

	// TokenIOTimeout 是整条 token 流程的墙钟上限。内部所有派生 ctx 都挂在它下面，
	// 超时即取消 —— 没有它，一个卡住的下游能把 HTTP 请求挂到网关超时。
	TokenIOTimeout time.Duration `env:"TOKEN_IO_TIMEOUT" envDefault:"5s"`

	// CORSAllowedOrigins 是允许跨域的精确 origin，逗号分隔（如 http://localhost:5173,https://m.example.com）。
	//
	// 留空时按环境分流：dev 放行任意 localhost / 127.0.0.1 / [::1] 端口
	// （Expo web 与 Vite 每次端口都可能不同，逐个登记不现实）；
	// prod 留空等于**不允许任何跨域** —— 要开放必须显式配置，不替使用方猜。
	CORSAllowedOrigins string `env:"CORS_ALLOWED_ORIGINS" envDefault:""`

	// ============================ 请求体积上限 ============================
	//
	// 见 httpapi.BodyLimit 的注释：oapi-codegen 会把 body 整个读进内存，
	// 没有上限等于把内存分配权交给任何能发请求的人（含未认证的公开端点）。
	//
	// 1 MiB 的依据：最大的合法请求体是「创建商品 + 若干 SKU」，实测几 KB。
	// 两个数量级余量足够，同时把最坏情况钉在可接受的数字上。
	// 若将来支持图片内联上传，这个值要按 base64 膨胀率（×1.37）重算。
	MaxBodyBytes   int64 `env:"MAX_BODY_BYTES" envDefault:"1048576"`
	MaxHeaderBytes int   `env:"MAX_HEADER_BYTES" envDefault:"16384"`
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

	if err := c.validateMockProviders(); err != nil {
		return err
	}

	if c.DatabaseURL == "" {
		return fmt.Errorf("DATABASE_URL 不能为空")
	}
	if c.AccessTTL <= 0 || c.RefreshTTL <= 0 || c.OrderPendingTTL <= 0 {
		return fmt.Errorf("ACCESS_TOKEN_TTL / REFRESH_TOKEN_TTL / ORDER_PENDING_TTL 必须为正数")
	}
	if err := c.validateRateLimit(); err != nil {
		return err
	}
	if c.TokenIOConcurrency < 1 {
		return fmt.Errorf("TOKEN_IO_CONCURRENCY 必须 ≥ 1（0 会让所有签发流程立刻失败）")
	}
	if c.TokenIOConcurrency > 32 {
		// 上界不是性能保护，是**故障半径**保护：并发度直接决定同时占用的
		// 连接数与 Redis 连接数。放到 32 以上时，一次刷新风暴就能把连接池吃满，
		// 于是「签发变慢 → 更多请求堆积 → 更多并发」形成正反馈。
		return fmt.Errorf("TOKEN_IO_CONCURRENCY 不能超过 32（当前 %d）", c.TokenIOConcurrency)
	}
	if c.TokenIOTimeout <= 0 {
		return fmt.Errorf("TOKEN_IO_TIMEOUT 必须为正数")
	}
	if c.MaxBodyBytes < 1024 {
		// 下限不是拍脑袋：比 1 KiB 还小的上限会让正常的登录/下单请求都过不去，
		// 表现为"服务起来了但所有写操作都 413"，排查起来要翻好几层。
		return fmt.Errorf("MAX_BODY_BYTES 不能小于 1024（当前 %d）", c.MaxBodyBytes)
	}
	if c.MaxHeaderBytes < 4096 {
		// Go 的 http.Server 默认 1 MiB。我们主动收到 16 KiB —— HTTP 头里
		// 塞不下任何合法业务数据，而每个连接都要为 header 预留缓冲区，
		// 上限越大，同样连接数下内存占用越高。
		return fmt.Errorf("MAX_HEADER_BYTES 不能小于 4096（当前 %d）", c.MaxHeaderBytes)
	}
	return nil
}

// validateMockProviders 拦住"生产环境跑 mock 认证/支付"这件事。
//
// ★ 为什么这是一条安全校验，而不只是配置卫生
//
// 两个 provider 的默认值都是 mock，而它们的 mock 实现**完全不设防**：
//
//   - payment.Mock.ParseNotify 不验签，接受任意 order_no 并返回 Success=true。
//     mock 模式下 /api/v1/pay/mock/notify 是**公开路由**，任何人 POST 一个
//     订单号就能把订单变成「已支付」—— 商品照发，钱不收。
//   - auth.MockExchanger 把任意 code 哈希成 openid。任何人 POST 一个 code
//     就能登录成一个账号。
//
// 危险的地方在于**忘记配置的代价是不对称的**：漏设 PAYMENT_PROVIDER 不会让
// 服务启动失败，也不会让任何一条现有测试变红，下单流程看上去完全正常 ——
// 只有在有人主动打那个端点时才暴露。这类"默认值在生产环境恰好是最弱配置"
// 的问题，靠代码评审很难发现，必须让**启动阶段**替人记住。
//
// 所以这里的策略与 JWT_SECRET 完全同构：宁可启动失败，不要静默留后门。
// 真需要在不接微信的环境里跑全链路（演示 / 验收），就走显式逃生门，
// 而且启动日志会 Warn —— 让人看得见这是刻意的选择。
func (c *Config) validateMockProviders() error {
	if !c.IsProd() {
		return nil
	}
	if c.AllowMockProvidersInProd {
		return nil
	}

	var offending []string
	if c.AuthProvider == "mock" {
		offending = append(offending, "AUTH_PROVIDER=mock（任意 code 即可登录成任意账号）")
	}
	if c.PaymentProvider == "mock" {
		offending = append(offending, "PAYMENT_PROVIDER=mock（任意订单可被标记为已支付）")
	}
	if len(offending) == 0 {
		return nil
	}

	return fmt.Errorf(
		"生产环境（APP_ENV=%q）不允许使用 mock provider：\n"+
			"  - %s\n"+
			"  这两个 mock 实现都不做验签/校验，在生产环境等于敞开的后门。\n"+
			"  改法：配置真实 provider（AUTH_PROVIDER=wechat / PAYMENT_PROVIDER=wechat）；\n"+
			"  如果这只是一台演示/验收环境，请显式设置 ALLOW_MOCK_PROVIDERS_IN_PROD=true。",
		c.AppEnv, strings.Join(offending, "\n  - "))
}

// MockProvidersAllowed 回答"当前是否在跑 mock 认证/支付"。
//
// 供启动日志使用：让"我正在用 mock"这件事在日志里有一行痕迹。
// 一个跑着 mock 支付的服务，在日志里看起来必须和跑真实支付的不一样。
func (c *Config) MockProvidersAllowed() bool {
	return c.AuthProvider == "mock" || c.PaymentProvider == "mock"
}

func (c *Config) validateRateLimit() error {
	switch c.RateLimitFailMode {
	case "auto", "open", "closed":
	default:
		return fmt.Errorf("RATE_LIMIT_FAIL_MODE 取值非法: %q（只允许 auto/open/closed）", c.RateLimitFailMode)
	}
	// 限流关掉时，下面几项不参与校验 —— 否则会出现"明明关了限流，
	// 却因为阈值没填对而启动失败"这种反直觉的组合。
	if !c.RateLimitEnabled {
		return nil
	}
	if c.RateLimitWindow <= 0 || c.RateLimitAuthWindow <= 0 {
		return fmt.Errorf("RATE_LIMIT_WINDOW / RATE_LIMIT_AUTH_WINDOW 必须为正数")
	}
	if c.RateLimitMax < 1 || c.RateLimitAuthMax < 1 {
		return fmt.Errorf("RATE_LIMIT_MAX / RATE_LIMIT_AUTH_MAX 必须 ≥ 1")
	}
	if c.RateLimitTimeout <= 0 {
		return fmt.Errorf("RATE_LIMIT_TIMEOUT 必须为正数")
	}
	return nil
}

// RateLimitFailsClosed 回答"Redis 挂时限流是收紧还是放开"。
//
// auto 的语义按环境分流：生产收紧、开发放开。理由是这两种场合的**失败代价**
// 完全不同 —— 开发环境里 Redis 挂掉的代价是"你本地登录不上去"，
// 生产环境里限流失效的代价是"被人拿几台机器刷登录接口"。
// 与其让每个部署自己记得改这个开关，不如让默认值本身站在正确的一边。
func (c *Config) RateLimitFailsClosed() bool {
	switch strings.ToLower(strings.TrimSpace(c.RateLimitFailMode)) {
	case "closed":
		return true
	case "open":
		return false
	default: // auto
		return c.IsProd()
	}
}

// IsProd 用**白名单反查**而不是黑名单：只有明确写成 dev/local/test 的值才算非生产，
// 其余一切（含拼错的 "production"、"PROD "、空串以外的任意值）都按生产处理。
//
// 黑名单写法（strings.EqualFold(AppEnv, "prod")）的问题是校验形同虚设只差一个字母：
// APP_ENV=production 时密钥强度校验被静默跳过，dev-only 默认密钥直接上生产，
// 而 HS256 是对称的 —— 签发与校验共用同一个 secret，泄露即等于全站 token 可伪造。
//
// 代价是新环境名（比如 staging）会默认走生产分支，即必须显式配 JWT_SECRET。
// 这是我们想要的偏保守方向：多配一个变量，比上线后重签所有 token 便宜。
func (c *Config) IsProd() bool {
	switch strings.ToLower(strings.TrimSpace(c.AppEnv)) {
	case "dev", "local", "test":
		return false
	default:
		return true
	}
}

func (c *Config) IsDev() bool { return !c.IsProd() }

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
