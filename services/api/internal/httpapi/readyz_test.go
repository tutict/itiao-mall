package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ★ /readyz 是公开路由（编排探针不该要 token），响应体任何人可见。
//
// 依赖错误的原文带着内网地址与端口 —— 实测过原始响应：
//
//	{"checks":{"redis":"dial tcp [::1]:6379: connectex: No connection could be made ..."},
//	 "status":"unavailable"}
//
// pgx 在 PG 挂掉时还会带上 host= / user= / database=。
//
// 这里用**假依赖**注入一段明确可辨认的内网地址文本，断言它不出现在响应里。
// 刻意不用"把 Redis 停掉"那种做法：那测的是环境而不是代码，而且 refused 与
// timeout 两种形态不同（前者必然带地址、后者不一定），守卫会时绿时红。
func TestReadyz_DoesNotLeakDependencyError(t *testing.T) {
	const secret = "host=secret-db.internal port=5432 user=itiao database=itiao"

	handler := readyzHandler(
		func(context.Context) error { return errors.New(secret) },
		func(context.Context) error { return errors.New("dial tcp [::1]:6379: connectex: 拒绝连接") },
	)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := rec.Body.String()

	// 1) 状态码仍必须是 503 —— 脱敏不能把"不可用"这个故障信号一起抹掉。
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("状态码 = %d，期望 503（body=%s）", rec.Code, body)
	}

	// 2) 内网地址与凭据一个字都不能出现。
	for _, leak := range []string{
		secret, "secret-db.internal", "user=itiao", "[::1]:6379", "port=5432",
	} {
		if strings.Contains(body, leak) {
			t.Errorf("响应体泄露了依赖细节 %q → %s", leak, body)
		}
	}

	// 3) 但"哪个依赖不可用"这个可操作信号必须保留 —— 否则编排层只能看到
	//    一个没有信息量的 503，排查要人工上机器。
	var got struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%s)", err, body)
	}
	if got.Status != "unavailable" {
		t.Errorf("status = %q，期望 unavailable", got.Status)
	}
	if got.Checks["pg"] != "unavailable" {
		t.Errorf("checks[pg] = %q，期望 unavailable", got.Checks["pg"])
	}
	if got.Checks["redis"] != "unavailable" {
		t.Errorf("checks[redis] = %q，期望 unavailable", got.Checks["redis"])
	}
}

// 健康时必须回 200 且 checks 为 ok。
//
// 这条是上一条的对照：没有它，"把 readyz 改成永远回 unavailable" 也能让
// 泄露测试变绿 —— 那是把探针弄坏，不是修好。
func TestReadyz_HealthyReportsOk(t *testing.T) {
	handler := readyzHandler(
		func(context.Context) error { return nil },
		func(context.Context) error { return nil },
	)

	rec := httptest.NewRecorder()
	handler(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，期望 200（body=%s）", rec.Code, body)
	}

	var got struct {
		Status string            `json:"status"`
		Checks map[string]string `json:"checks"`
	}
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatalf("响应体不是合法 JSON: %v (%s)", err, body)
	}
	if got.Status != "ready" {
		t.Errorf("status = %q，期望 ready", got.Status)
	}
	if got.Checks["pg"] != "ok" || got.Checks["redis"] != "ok" {
		t.Errorf("健康时 checks 应为全 ok，实际: %v", got.Checks)
	}
}
