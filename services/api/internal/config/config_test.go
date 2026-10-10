package config

import (
	"strings"
	"testing"
)

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
