package config

import (
	"os"
	"strings"
	"testing"
)

// unsetEnv 临时移除若干环境变量，测试结束后恢复。
//
// 为什么必须用它而不是 t.Setenv(k, "")：caarlos0/env 的 envDefault 只在变量
// **不存在**时生效；变量存在但为空串会走进解析失败，得到的是"报错"而不是
// "拿到默认值"。所以要测真默认值，只能真的把变量摘掉。
func unsetEnv(t *testing.T, keys ...string) {
	t.Helper()
	for _, k := range keys {
		old, existed := os.LookupEnv(k)
		if err := os.Unsetenv(k); err != nil {
			t.Fatalf("unset %s: %v", k, err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(k, old)
			} else {
				_ = os.Unsetenv(k)
			}
		})
	}
}

// baseEnv 把会影响 Load() 的所有相关变量显式设成一组已知值。
//
// 为什么要"显式设全部"而不是只设要测的那几个：Load() 读的是**真实环境变量**。
// 开发机上恰好导出了 AUTH_PROVIDER=wechat 之类的话，只设一半的测试会变得
// 依赖机器状态 —— 表现为「我这儿是绿的，CI 上红」。
func baseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("APP_ENV", "dev")
	t.Setenv("JWT_SECRET", "test-secret-not-the-default")
	t.Setenv("AUTH_PROVIDER", "mock")
	t.Setenv("PAYMENT_PROVIDER", "mock")
	t.Setenv("ALLOW_MOCK_PROVIDERS_IN_PROD", "false")
	t.Setenv("WECHAT_APP_ID", "")
	t.Setenv("WECHAT_APP_SECRET", "")
	t.Setenv("DATABASE_URL", "postgres://itiao:itiao@localhost:5432/itiao?sslmode=disable")
	t.Setenv("MAX_BODY_BYTES", "1048576")
	t.Setenv("MAX_HEADER_BYTES", "16384")
	// 资源上限类。与上面同理：显式设全部，避免机器上恰好导出了
	// MAX_INFLIGHT_REQUESTS=1 之类的值时，测试变得依赖机器状态。
	t.Setenv("MAX_CONNS", "4096")
	t.Setenv("MAX_INFLIGHT_REQUESTS", "2048")
	t.Setenv("PG_MAX_CONNS", "20")
	t.Setenv("REDIS_POOL_SIZE", "20")
	t.Setenv("RATE_LIMIT_REDIS_POOL_SIZE", "100")
	t.Setenv("RATE_LIMIT_BREAKER_THRESHOLD", "5")
	t.Setenv("RATE_LIMIT_BREAKER_OPEN", "5s")
}

// TestValidate_MockProvidersRejectedInProd 是这一组的核心断言。
//
// 它守的是这样一个场景：运维部署时忘了设 PAYMENT_PROVIDER。
// 在加固之前，服务会**正常启动**，下单流程看起来也完全正常 ——
// 但 /api/v1/pay/mock/notify 是公开路由且不验签，
// 任何人 POST 一个订单号就能把订单变成「已支付」。
//
// 一个"忘记配置"就能敞开的后门，必须在启动阶段被拦住。
func TestValidate_MockProvidersRejectedInProd(t *testing.T) {
	cases := []struct {
		name            string
		appEnv          string
		auth            string
		payment         string
		allowMockInProd bool
		wantErr         bool
		wantMention     []string
	}{
		{
			name:    "生产 + 两个都 mock：必须拒绝",
			appEnv:  "prod",
			auth:    "mock",
			payment: "mock",
			wantErr: true,
			// 错误信息必须点名**是哪一个** provider 有问题。只说
			// "配置非法"的话，运维得逐个变量去猜，而这类问题通常发生在
			// 部署窗口里，没人有空猜。
			wantMention: []string{"AUTH_PROVIDER", "PAYMENT_PROVIDER"},
		},
		{
			name:        "生产 + 只有支付是 mock：必须拒绝",
			appEnv:      "prod",
			auth:        "wechat",
			payment:     "mock",
			wantErr:     true,
			wantMention: []string{"PAYMENT_PROVIDER"},
		},
		{
			name:        "生产 + 只有认证是 mock：必须拒绝",
			appEnv:      "prod",
			auth:        "mock",
			payment:     "wechat",
			wantErr:     true,
			wantMention: []string{"AUTH_PROVIDER"},
		},
		{
			name:            "生产 + 显式逃生门：放行",
			appEnv:          "prod",
			auth:            "mock",
			payment:         "mock",
			allowMockInProd: true,
		},
		{
			name:    "dev + mock：放行（这是默认值，放行是刻意的）",
			appEnv:  "dev",
			auth:    "mock",
			payment: "mock",
		},
		{
			// staging 不在 IsProd 的白名单里（dev/local/test），
			// 所以它按生产处理。这条断言钉住的是"白名单反查"这个设计：
			// 新加的环境名默认走安全分支，而不是默认走宽松分支。
			name:    "staging（非白名单环境）按生产处理，必须拒绝",
			appEnv:  "staging",
			auth:    "mock",
			payment: "mock",
			wantErr: true,
		},
		{
			// 拼写变体：黑名单写法（EqualFold(AppEnv,"prod")）会在这里失守。
			name:    "环境名拼错成 production：仍按生产处理",
			appEnv:  "production",
			auth:    "mock",
			payment: "mock",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("AUTH_PROVIDER", tc.auth)
			t.Setenv("PAYMENT_PROVIDER", tc.payment)
			if tc.allowMockInProd {
				t.Setenv("ALLOW_MOCK_PROVIDERS_IN_PROD", "true")
			}
			// 走 wechat 时需要凭证，否则会先在凭证校验上失败，
			// 测的就不是 mock 这条规则了。
			if tc.auth == "wechat" || tc.payment == "wechat" {
				t.Setenv("WECHAT_APP_ID", "wx-test")
				t.Setenv("WECHAT_APP_SECRET", "secret-test")
			}

			_, err := Load()

			if tc.wantErr && err == nil {
				t.Fatal("期望启动失败（生产环境不允许 mock provider），实际通过了 —— 后门是敞开的")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("期望通过，实际报错: %v", err)
			}
			for _, want := range tc.wantMention {
				if err != nil && !strings.Contains(err.Error(), want) {
					t.Errorf("错误信息应点名 %s，实际: %v", want, err)
				}
			}
		})
	}
}

// TestMockProvidersAllowed 守的是启动日志那一侧。
//
// 只把校验写对是不够的：一个跑着 mock 支付的服务，在**日志**里也必须
// 和跑真实支付的长得不一样。否则事后查"这笔订单为什么没收到钱"时，
// 第一屏看不出任何异常。
func TestMockProvidersAllowed(t *testing.T) {
	cases := []struct {
		auth, payment string
		want          bool
	}{
		{"mock", "mock", true},
		{"mock", "wechat", true},
		{"wechat", "mock", true},
		{"wechat", "wechat", false},
	}
	for _, tc := range cases {
		c := &Config{AuthProvider: tc.auth, PaymentProvider: tc.payment}
		if got := c.MockProvidersAllowed(); got != tc.want {
			t.Errorf("auth=%s payment=%s: 期望 %v，实际 %v", tc.auth, tc.payment, tc.want, got)
		}
	}
}

// TestValidate_BodyLimits 守的是体积上限的下限。
//
// 上限配成 0 或负数时如果被静默忽略（或换算成"无限制"），
// 就等于这一层加固不存在。所以必须有下限校验。
func TestValidate_BodyLimits(t *testing.T) {
	cases := []struct {
		name       string
		maxBody    string
		maxHeader  string
		wantErrSub string
	}{
		{"正常值", "1048576", "16384", ""},
		{"body 上限为 0", "0", "16384", "MAX_BODY_BYTES"},
		{"body 上限过小", "512", "16384", "MAX_BODY_BYTES"},
		{"header 上限为 0", "1048576", "0", "MAX_HEADER_BYTES"},
		{"header 上限过小", "1048576", "1024", "MAX_HEADER_BYTES"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("MAX_BODY_BYTES", tc.maxBody)
			t.Setenv("MAX_HEADER_BYTES", tc.maxHeader)

			_, err := Load()
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错（上限太低会让正常的写操作全部 413），实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("错误信息应提到 %s，实际: %v", tc.wantErrSub, err)
			}
		})
	}
}

// TestValidate_PprofRejectedInProd 守"剖析端点可被外部访问"这件事。
//
// ★ 为什么这条与 validateMockProviders 是同一类问题
//
// mock provider 的危险在于"忘记配置"：漏设 PAYMENT_PROVIDER，服务照常启动、
// 下单流程看上去完全正常、没有任何测试变红，只有主动打那个端点的人才发现。
//
// PPROF_ADDR 的形态一模一样，而且后果更直接：剖析端点可以读到进程内存。
// heap/allocs 带出分配点的参数内容，goroutine?debug=2 带出每个协程的完整栈，
// 而 cmdline（虽然本实现刻意不挂）会把 DATABASE_URL 的密码与 JWT_SECRET 一起吐出来。
//
// 特别要防的是 **":6060"** 这个写法 —— 它长得最像"本机端口"，
// 但 Go 在 host 为空时绑的是通配地址，也就是所有网卡。
// 那个默认分支必须落在拒绝的一侧。
func TestValidate_PprofRejectedInProd(t *testing.T) {
	cases := []struct {
		name            string
		appEnv          string
		pprofAddr       string
		maxSeconds      string
		allowPublicProd string
		wantErr         bool
		wantMention     []string
	}{
		{
			name:      "生产 + 空 host（绑通配，最危险的形态）：必须拒绝",
			appEnv:    "prod",
			pprofAddr: ":6060",
			wantErr:   true,
			// 错误信息必须点名逃生门，否则运维在部署窗口里无从下手。
			wantMention: []string{"PPROF_ADDR", "ALLOW_PUBLIC_PPROF_IN_PROD"},
		},
		{
			name:        "生产 + 显式 0.0.0.0：必须拒绝",
			appEnv:      "prod",
			pprofAddr:   "0.0.0.0:6060",
			wantErr:     true,
			wantMention: []string{"PPROF_ADDR"},
		},
		{
			name:        "生产 + 局域网 IP：必须拒绝",
			appEnv:      "prod",
			pprofAddr:   "192.168.1.10:6060",
			wantErr:     true,
			wantMention: []string{"PPROF_ADDR"},
		},
		{
			name:        "生产 + 公网 IP：必须拒绝",
			appEnv:      "prod",
			pprofAddr:   "203.0.113.5:6060",
			wantErr:     true,
			wantMention: []string{"PPROF_ADDR"},
		},
		{
			name:            "生产 + 显式逃生门：放行（容器编排要这个）",
			appEnv:          "prod",
			pprofAddr:       "0.0.0.0:6060",
			allowPublicProd: "true",
		},
		{
			name:      "生产 + 回环：放行",
			appEnv:    "prod",
			pprofAddr: "127.0.0.1:6060",
		},
		{
			name:      "生产 + IPv6 回环：放行",
			appEnv:    "prod",
			pprofAddr: "[::1]:6060",
		},
		{
			name:      "dev + 通配：放行（开发便利优先，生产才是硬要求）",
			appEnv:    "dev",
			pprofAddr: ":6060",
		},
		{
			name:   "生产 + 没配 PPROF_ADDR：放行（默认就是关）",
			appEnv: "prod",
			// 这是最重要的一条：**默认关闭**必须是安全的。
			// 若哪天有人把默认值改成 ":6060"，这里会红。
		},
		{
			name:        "开着 + maxSeconds 为 0：必须拒绝（会让每次采集都被拒）",
			appEnv:      "dev",
			pprofAddr:   "127.0.0.1:6060",
			maxSeconds:  "0",
			wantErr:     true,
			wantMention: []string{"PPROF_MAX_SECONDS"},
		},
		{
			name:        "开着 + maxSeconds 过大：必须拒绝（profiling 永久生效像负载问题）",
			appEnv:      "dev",
			pprofAddr:   "127.0.0.1:6060",
			maxSeconds:  "86400",
			wantErr:     true,
			wantMention: []string{"PPROF_MAX_SECONDS"},
		},
		{
			name:   "开着但 maxSeconds 超界，且 PPROF_ADDR 关闭：放行",
			appEnv: "dev",
			// 没开 pprof 就不该校验采集时长 ——
			// 否则会出现"明明关了 pprof，却因为没配时长而启动失败"这种反直觉组合。
			// 理由与 validateRateLimit 里对 RATE_LIMIT_* 的处理一致。
			maxSeconds: "999999",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			t.Setenv("APP_ENV", tc.appEnv)
			t.Setenv("PPROF_ADDR", tc.pprofAddr)
			t.Setenv("PPROF_MAX_SECONDS", tc.maxSeconds)
			t.Setenv("ALLOW_PUBLIC_PPROF_IN_PROD", tc.allowPublicProd)

			// ★ 生产场景统一开 mock 逃生门，这条与被测的 pprof 校验无关。
			//
			// 不开的话，每个"生产 + 期望通过"的用例都会先被既有的
			// validateMockProviders 拦下（baseEnv 里 AUTH/PAYMENT 都是 mock），
			// 于是这条用例实际测的是"mock 校验还在生效"，
			// 而它自己声称要测的东西（回环地址放行）**一次都没跑到**。
			// 那种"名字说测 A、实际测 B"的用例比没有更坏：
			// 它在回环判定写错时仍然全绿。
			t.Setenv("ALLOW_MOCK_PROVIDERS_IN_PROD", "true")

			_, err := Load()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			for _, m := range tc.wantMention {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("错误信息应提到 %s，实际: %v", m, err)
				}
			}
		})
	}
}

// TestPprofEnabled_DefaultIsOff 单独钉住"默认关闭"这条。
//
// 它与上面的表里"没配就放行"不是同一件事：那条验的是**不报错**，
// 这条验的是**真的不起端口**。一个"配了才校验、但没配时照样起端口"的实现
// 能通过上面那张表，却在这里现形。
func TestPprofEnabled_DefaultIsOff(t *testing.T) {
	baseEnv(t)
	t.Setenv("PPROF_ADDR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}
	if cfg.PprofEnabled() {
		t.Fatal("未配置 PPROF_ADDR 时 PprofEnabled() 为 true —— " +
			"剖析端点必须默认不存在（它可以读到进程内存）")
	}
}

// TestDefaults_AreProtective 守住"资源上限默认就是开着的"。
//
// ★ 为什么这条比边界测试更重要
//
// 容量保护最可能的失效方式不是"配错值"，而是"默认值被改成 0 / 关闭"。
// 那种改动**不会让任何测试变红**（0 是合法配置、启动照常），
// 现象是"服务在裸奔，但一切看起来正常"，直到某天被打挂才发现。
// 所以这里把"默认必须为正"写死成断言 —— 谁把默认值改成 0，这里立刻红。
func TestDefaults_AreProtective(t *testing.T) {
	baseEnv(t)
	// 摘掉这几个变量，才能读到**真默认值**而不是 baseEnv 里显式设的值。
	unsetEnv(t,
		"MAX_CONNS", "MAX_INFLIGHT_REQUESTS",
		"PG_MAX_CONNS", "REDIS_POOL_SIZE", "RATE_LIMIT_REDIS_POOL_SIZE",
		"RATE_LIMIT_BREAKER_THRESHOLD", "RATE_LIMIT_BREAKER_OPEN",
	)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load 失败: %v", err)
	}

	if cfg.MaxConns <= 0 {
		t.Error("MAX_CONNS 默认必须是正数（0 = 连接数无上限，fd 可被慢速攻击抽干）")
	}
	if cfg.MaxInflightRequests <= 0 {
		t.Error("MAX_INFLIGHT_REQUESTS 默认必须是正数（0 = 并发无上限，超时瀑布）")
	}
	if cfg.PGMaxConns < 1 {
		t.Error("PG_MAX_CONNS 默认必须 ≥ 1")
	}
	if cfg.RedisPoolSize < 1 {
		t.Error("REDIS_POOL_SIZE 默认必须 ≥ 1")
	}
	if cfg.RateLimitRedisPoolSize < 1 {
		t.Error("RATE_LIMIT_REDIS_POOL_SIZE 默认必须 ≥ 1")
	}
	if cfg.RateLimitBreakerThreshold < 1 {
		t.Error("RATE_LIMIT_BREAKER_THRESHOLD 默认必须 ≥ 1（0 = 熔断永不打开，安全侧静默失效）")
	}
	if cfg.RateLimitBreakerOpen <= 0 {
		t.Error("RATE_LIMIT_BREAKER_OPEN 默认必须为正")
	}
}

// TestValidate_ResourceLimits 守资源上限的取值边界。
//
// 边界的着重点是"负数必须响"与"0 的语义必须明确"：
//   - 负数一定是笔误，没有任何"负的上限"语义，必须在启动时拦住；
//   - 0 对 MAX_CONNS / MAX_INFLIGHT_REQUESTS 是**显式关闭**（合法），
//     对连接池和熔断阈值则是**非法**（会让池建不出连接 / 熔断永不打开）。
//     两者分开处置，才不会逼人用魔法大数（1e9）表达"关闭"。
func TestValidate_ResourceLimits(t *testing.T) {
	cases := []struct {
		name       string
		env        map[string]string
		wantErrSub string
	}{
		{"默认值全部合法", nil, ""},

		{"MAX_CONNS 为 0：合法（显式关闭连接上限）", map[string]string{"MAX_CONNS": "0"}, ""},
		{"MAX_CONNS 为负：拒绝", map[string]string{"MAX_CONNS": "-1"}, "MAX_CONNS"},

		{"MAX_INFLIGHT_REQUESTS 为 0：合法（显式关闭闸门）",
			map[string]string{"MAX_INFLIGHT_REQUESTS": "0"}, ""},
		{"MAX_INFLIGHT_REQUESTS 为负：拒绝",
			map[string]string{"MAX_INFLIGHT_REQUESTS": "-5"}, "MAX_INFLIGHT_REQUESTS"},

		{"PG_MAX_CONNS 为 0：拒绝（池建不出连接）", map[string]string{"PG_MAX_CONNS": "0"}, "PG_MAX_CONNS"},
		{"REDIS_POOL_SIZE 为负：拒绝", map[string]string{"REDIS_POOL_SIZE": "-1"}, "REDIS_POOL_SIZE"},
		{"RATE_LIMIT_REDIS_POOL_SIZE 为 0：拒绝",
			map[string]string{"RATE_LIMIT_REDIS_POOL_SIZE": "0"}, "RATE_LIMIT_REDIS_POOL_SIZE"},

		{"熔断阈值为 0：拒绝（熔断永不打开）",
			map[string]string{"RATE_LIMIT_BREAKER_THRESHOLD": "0"}, "RATE_LIMIT_BREAKER_THRESHOLD"},
		{"熔断冷却为 0：拒绝",
			map[string]string{"RATE_LIMIT_BREAKER_OPEN": "0s"}, "RATE_LIMIT_BREAKER_OPEN"},

		// 关掉限流后，限流专属的几项不该再参与校验 ——
		// 否则会出现"明明关了限流，却因为熔断参数没配对而启动失败"这种反直觉组合。
		{"限流关闭时熔断参数不校验",
			map[string]string{"RATE_LIMIT_ENABLED": "false", "RATE_LIMIT_BREAKER_THRESHOLD": "0"}, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			baseEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			_, err := Load()
			if tc.wantErrSub == "" {
				if err != nil {
					t.Fatalf("期望通过，实际报错: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("期望报错，实际通过")
			}
			if !strings.Contains(err.Error(), tc.wantErrSub) {
				t.Errorf("错误信息应提到 %s，实际: %v", tc.wantErrSub, err)
			}
		})
	}
}
