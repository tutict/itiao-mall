package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 角色。用户端、商户端、管理员端共用同一套签发逻辑，靠 role 区分权限。
//
// 权限判定不在这里做 —— 这里只声明常量。真正的判定收敛在 httpapi 的路由策略表，
// 因为「哪个角色能访问哪段路径」是路由层的知识，不该散落到 auth 包。
const (
	RoleUser     = "user"
	RoleMerchant = "merchant"
	// RoleAdmin 是平台运营：审核商户入驻、审核商品上架。
	RoleAdmin = "admin"
)

// knownRoles 是签发与校验都认可的角色集合。
//
// 校验侧也要用它：role 不在这个集合里的 token 属于**坏 token**，在解析阶段就该拒掉。
// 为什么必须在解析阶段拦而不是留给路由层：路由策略里有一条是「任意已登录角色放行」
// （`/api/v1/me` 那种），那类规则本来就**不看具体角色**，一个 role 为空字符串
// 或写了别的值的 token 会从它下面直接穿过去。把它挡在解析阶段，等于给
// 「token 里到底有没有一个合法身份」加了一道下限。
var knownRoles = map[string]struct{}{
	RoleUser:     {},
	RoleMerchant: {},
	RoleAdmin:    {},
}

// IsKnownRole 判断角色是否为已定义的角色之一。
func IsKnownRole(role string) bool {
	_, ok := knownRoles[role]
	return ok
}

const (
	typAccess  = "access"
	typRefresh = "refresh"
)

// Claims 是 access token 的载荷。
type Claims struct {
	UserID string `json:"uid"`
	Role   string `json:"role"`
	Typ    string `json:"typ"`
	jwt.RegisteredClaims
}

// TokenIssuer 负责签发与校验 access token。
// access token 是无状态的（不查 Redis），TTL 短；吊销能力靠 refresh token 那侧实现。
type TokenIssuer struct {
	secret    []byte
	accessTTL time.Duration
	now       func() time.Time
}

func NewTokenIssuer(secret string, accessTTL time.Duration) *TokenIssuer {
	return &TokenIssuer{
		secret:    []byte(secret),
		accessTTL: accessTTL,
		now:       time.Now,
	}
}

// IssueAccess 签发 access token，同时返回过期时间。
func (t *TokenIssuer) IssueAccess(userID, role string) (string, time.Time, error) {
	now := t.now()
	exp := now.Add(t.accessTTL)

	claims := Claims{
		UserID: userID,
		Role:   role,
		Typ:    typAccess,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			ID:        newRandomID(),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString(t.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("签发 access token 失败: %w", err)
	}
	return signed, exp, nil
}

// ParseAccess 校验签名与时效，并确认 typ 是 access
// （防止有人拿 refresh 串来当 access 用 —— 后者不在本函数管辖内，但 typ 校验是底线）。
func (t *TokenIssuer) ParseAccess(raw string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(raw, claims, func(token *jwt.Token) (any, error) {
		if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("非预期的签名算法: %v", token.Header["alg"])
		}
		return t.secret, nil
	},
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidToken, err)
	}
	if claims.Typ != typAccess {
		return nil, fmt.Errorf("%w: token 类型为 %q，期望 %q", ErrInvalidToken, claims.Typ, typAccess)
	}
	if claims.UserID == "" {
		return nil, fmt.Errorf("%w: 缺少 uid", ErrInvalidToken)
	}
	if !IsKnownRole(claims.Role) {
		return nil, fmt.Errorf("%w: 未知角色 %q", ErrInvalidToken, claims.Role)
	}
	return claims, nil
}

// --- context 传递 ---

type ctxKey int

const ctxKeyClaims ctxKey = iota

func WithClaims(ctx context.Context, c *Claims) context.Context {
	return context.WithValue(ctx, ctxKeyClaims, c)
}

// FromContext 取出当前请求的 claims；未认证时返回 nil, false。
func FromContext(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxKeyClaims).(*Claims)
	return c, ok && c != nil
}
