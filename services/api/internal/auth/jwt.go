package auth

import (
	"context"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// 角色。用户端与商户端共用同一套签发逻辑，靠 role 区分权限。
const (
	RoleUser     = "user"
	RoleMerchant = "merchant"
)

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

// RequireRole 判断 claims 是否具备指定角色。
func (c *Claims) HasRole(role string) bool { return c.Role == role }
