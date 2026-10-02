package auth

import (
	"errors"
	"testing"
	"time"
)

const testUID = "11111111-1111-4111-8111-111111111111"

// 未知角色的 token 必须在**解析阶段**被拒。
//
// 它挡的是一条真实的漏洞路径：路由策略里有一档是「任意已登录角色放行」
// （`/api/v1/me` 那种），那一档本来就不看具体角色 —— role 为空字符串
// 或写了个别的值的 token 会从它下面直接穿过去。把合法性下限放在解析里，
// 比在每个策略条目上再加一次判断可靠。
func TestParseAccess_RejectsUnknownRole(t *testing.T) {
	issuer := NewTokenIssuer("s", time.Minute)

	for _, role := range []string{"", "superuser", "USER", "admin ", "user,merchant"} {
		tok, _, err := issuer.IssueAccess(testUID, role)
		if err != nil {
			t.Fatalf("签发角色 %q 失败: %v", role, err)
		}

		_, err = issuer.ParseAccess(tok)
		if err == nil {
			t.Fatalf("角色 %q 的 token 应被拒绝，却解析成功了", role)
		}
		if !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("角色 %q 应返回 ErrInvalidToken，实际 %v", role, err)
		}
	}
}

func TestParseAccess_AcceptsKnownRoles(t *testing.T) {
	issuer := NewTokenIssuer("s", time.Minute)

	for _, role := range []string{RoleUser, RoleMerchant, RoleAdmin} {
		tok, _, err := issuer.IssueAccess(testUID, role)
		if err != nil {
			t.Fatalf("签发角色 %s 失败: %v", role, err)
		}

		claims, err := issuer.ParseAccess(tok)
		if err != nil {
			t.Fatalf("已知角色 %s 应被接受，实际 %v", role, err)
		}
		if claims.Role != role {
			t.Fatalf("角色应回读为 %q，实际 %q", role, claims.Role)
		}
		if claims.UserID != testUID {
			t.Fatalf("uid 应回读为 %q，实际 %q", testUID, claims.UserID)
		}
	}
}

func TestIsKnownRole(t *testing.T) {
	for _, ok := range []string{RoleUser, RoleMerchant, RoleAdmin} {
		if !IsKnownRole(ok) {
			t.Fatalf("%q 应是已知角色", ok)
		}
	}
	for _, no := range []string{"", "root", "Admin", "operator"} {
		if IsKnownRole(no) {
			t.Fatalf("%q 不该被认作已知角色", no)
		}
	}
}
