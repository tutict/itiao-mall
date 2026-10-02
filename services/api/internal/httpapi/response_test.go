package httpapi

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
)

// 这一组测试盯的是错误响应里的信息泄露。
//
// 实测过的真实泄露（修之前的活服务器响应）：
//
//	{"status":409,"detail":"资源冲突: can't decode JSON body: json: cannot unmarshal
//	 number into Go struct field PasswordLoginRequest.phone of type string"}
//
// 这不是"多写了点调试信息"，而是把 Go 类型名与字段名白送给任何能发请求的人。
// 字段名往往正是业务语义里抢不到的部分 —— 攻击者据此能逐步画出整个请求体 schema，
// 也就知道该往哪个字段塞越界值。
//
// 修法是"默认不外泄"：WriteProblem 只回 mapError 里的固定标题 + 业务层用
// domain.UserFacingf 显式声明的文案，其余一律进日志。
//
// 注意这里刻意**不**断言"detail 为空"：业务校验文案（"密码至少 8 位"）必须还能
// 出去，否则前端只能把提示写死。真正要断言的是"不出现内部痕迹"。

// 内部痕迹特征。命中任意一条即视为泄露。
// 这些是 Go / encoding-json / pgx 的惯用词形，不是我们自己的中文文案。
var leakMarkers = []string{
	"json:",            // encoding/json 的错误前缀
	"cannot unmarshal", //
	"Go struct field",  // 反射类型名 —— 最致命的一条
	".go:",             // 文件:行号
	"sql:",             // database/sql
	"pgx",              // pgx
	"sqlc",
	"runtime.",
	"0x", // 十六进制指针/内存地址
	"panic:",
	"goroutine",
	"nil pointer",
}

func assertNoLeak(t *testing.T, body string) {
	t.Helper()
	for _, m := range leakMarkers {
		if strings.Contains(body, m) {
			t.Errorf("响应体泄露内部痕迹 %q：%s", m, body)
		}
	}
}

// problemBody 发一个请求并返回 (状态码, 响应体原文)。
func problemBody(t *testing.T, h http.Handler, req *http.Request) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestWriteProblem_NoInternalLeak 对每个映射分支都打一遍。
//
// 覆盖用 sentinel + 包裹错误两类：只有带包裹的那类才可能带进库错误原文，
// 所以两类都必须测 —— 只测 sentinel 会以为已经防住了。
func TestWriteProblem_NoInternalLeak(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"裸哨兵-未找到", domain.ErrNotFound},
		{"裸哨兵-冲突", domain.ErrConflict},
		{"裸哨兵-校验", domain.ErrValidation},
		{"裸哨兵-库存", domain.ErrInsufficientStock},
		{"包裹-json原文", wrapLikeJSON()},
		{"包裹-sql原文", wrapLikeSQL()},
		{"包裹-pgx原文", wrapLikePGX()},
		{"payment回调错误", payment.ErrInvalidNotify},
		{"auth令牌错误", auth.ErrInvalidToken},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/v1/whatever", nil)
			WriteProblem(rec, req, tc.err)

			if rec.Code < 400 || rec.Code >= 500 {
				t.Fatalf("应回 4xx，实际 %d", rec.Code)
			}
			assertNoLeak(t, rec.Body.String())
		})
	}
}

// TestWriteProblem_UserFacingStillVisible 反向断言：显式声明的文案必须还出去。
//
// 没有这一条，上面的测试可以通过"把所有 detail 都抹掉"来满足 —— 那是回归：
// 前端会失去所有字段级提示，只能把错误文案写死在代码里。
func TestWriteProblem_UserFacingStillVisible(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/merchant/register", nil)
	WriteProblem(rec, req, domain.UserFacingf("密码至少 8 位", domain.ErrValidation))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("校验错误应回 400，实际 %d", rec.Code)
	}
	var p Problem
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if p.Detail != "密码至少 8 位" {
		t.Fatalf("显式声明的文案应外泄，实际 detail=%q", p.Detail)
	}
}

// TestWriteProblem_UserFacingSurvivesWrapping 校验 UserFacing 被再包一层后仍能被取到。
//
// 这是实际调用形态：service 层返回 UserFacing，handler 再往上包一层。
// 如果只认最外层类型，一次包裹就静默丢掉全部提示 —— 而且没有任何报错。
func TestWriteProblem_UserFacingSurvivesWrapping(t *testing.T) {
	inner := domain.UserFacingf("驳回必须给出理由", domain.ErrValidation)
	wrapped := wrap(inner, "审核商户失败")

	var p Problem
	rec := httptest.NewRecorder()
	WriteProblem(rec, httptest.NewRequest(http.MethodPost, "/x", nil), wrapped)
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if p.Detail != "驳回必须给出理由" {
		t.Fatalf("包裹后仍应取到内层文案，实际 %q", p.Detail)
	}
}

// TestRequestErrorHandler_NoDecodeLeak 打真实的解析失败路径。
//
// 这一条是本次修复的起因：RequestErrorHandlerFunc 曾经把 oapi-codegen 的
// 原始解析错误包进 ErrConflict，实测泄露 PasswordLoginRequest 字段名。
//
// 这里跑的是**真实的 NewRouter**（不是伪造一个错误字符串）：只有真的让
// oapi-codegen 去解一个畸形 body，才会经过生产配置里那个回调。伪造
// `errors.New("json: cannot unmarshal ...")` 塞进去，测的是"我们会不会过滤某个
// 已知字符串"，而不是"这条路到底会不会泄露"—— 前者会漏掉解析库换个措辞的情况。
func TestRequestErrorHandler_NoDecodeLeak(t *testing.T) {
	// Pool 传 nil：解析失败发生在路由到业务之前，不会真的查库。
	h := NewRouter(Deps{
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Pool:           nil,
		Issuer:         auth.NewTokenIssuer("test-secret", time.Minute),
		RefreshStore:   auth.NewRefreshStore(nil, time.Hour),
		Wechat:         auth.MockExchanger{},
		Payment:        payment.NewMock(),
		PendingTTL:     15 * time.Minute,
		CORSAllowLocal: true,
	})

	// phone 是 string，传数字 → encoding/json 报错，原文必然带
	// "PasswordLoginRequest" 这个类型名（修复前实测到的那条）。
	malformed := []struct {
		name string
		path string
		body string
	}{
		{"类型不匹配", "/api/v1/auth/admin/login", `{"phone":123}`},
		{"body 截断", "/api/v1/auth/admin/login", `{"phone":`},
		{"完全不是 JSON", "/api/v1/auth/admin/login", `this is not json`},
		{"数组当对象", "/api/v1/auth/admin/login", `[1,2,3]`},
		{"商户注册类型错", "/api/v1/auth/merchant/register", `{"phone":"13800000000","password":123}`},
	}

	for _, m := range malformed {
		t.Run(m.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, m.path, strings.NewReader(m.body))
			req.Header.Set("Content-Type", "application/json")

			code, body := problemBody(t, h, req)
			if code < 400 || code >= 500 {
				t.Fatalf("畸形 body 应回 4xx，实际 %d（body=%s）", code, body)
			}
			assertNoLeak(t, body)
			// 契约类型名同样算泄露：它把服务端内部类型图暴露给客户端。
			for _, typeName := range []string{"PasswordLoginRequest", "MerchantRegisterRequest", "gen."} {
				if strings.Contains(body, typeName) {
					t.Errorf("响应体泄露契约类型名 %q：%s", typeName, body)
				}
			}
		})
	}
}

// TestDerefInt32_RejectsOverflow 钉住 int32 收窄。
//
// 契约里 page 只有 minimum 没有 maximum，客户端可以传任意 int。
// 无条件 int32(*p) 在 page=2147483648 时截断成 -2147483648，随后被 normalizePage
// 悄悄改写成 1 —— 客户端请求第 2147483648 页拿到第 1 页，且**没有任何报错**。
// 这种静默错页比报错难查得多。
func TestDerefInt32_RejectsOverflow(t *testing.T) {
	big := 2147483648 // math.MaxInt32 + 1
	one := 1

	if got := derefInt32(&one, 7); got != 1 {
		t.Errorf("正常值应原样返回，实际 %d", got)
	}
	if got := derefInt32(nil, 7); got != 7 {
		t.Errorf("nil 应回默认值，实际 %d", got)
	}
	if got := derefInt32(&big, 7); got != 7 {
		t.Errorf("超出 int32 范围应回默认值 %d，实际 %d（截断后会被当成第 1 页）", 7, got)
	}
	if got := derefInt32(&big, 1); got != 1 {
		t.Errorf("越界值不能伪装成合法页码，实际 %d", got)
	}

	// 边界值本身是合法的 int32，不能被误判为越界。
	maxI32 := int(2147483647)
	minI32 := -maxI32 - 1
	if got := derefInt32(&maxI32, 7); got != 2147483647 {
		t.Errorf("int32 上界应放行，实际 %d", got)
	}
	if got := derefInt32(&minI32, 7); got != -2147483648 {
		t.Errorf("int32 下界应放行，实际 %d", got)
	}
}

// ---- 测试替身 ----

// wrapLikeJSON 复刻修复前实测到的那条泄露文本。
//
// 刻意用字符串而不是 json.UnmarshalTypeError：后者的 Error() 在 Type 为 nil 时
// 会 panic（它要读 typ.String()），而我们真正要守的是"字符串进响应体"这件事。
func wrapLikeJSON() error {
	return wrap(domain.ErrConflict,
		"can't decode JSON body: json: cannot unmarshal number into Go struct field "+
			"PasswordLoginRequest.phone of type string")
}

func wrapLikeSQL() error {
	return wrap(domain.ErrConflict,
		`ERROR: duplicate key value violates unique constraint "users_phone_key" (SQLSTATE 23505)`)
}

func wrapLikePGX() error {
	return wrap(domain.ErrConflict, `pgx: scan failed: cannot scan NULL into *string`)
}

func wrap(err error, msg string) error {
	return &wrapped{msg: msg, err: err}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.err.Error() + ": " + w.msg }
func (w *wrapped) Unwrap() error { return w.err }
