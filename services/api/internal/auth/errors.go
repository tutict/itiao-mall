package auth

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
)

var (
	ErrInvalidToken        = errors.New("access token 无效")
	ErrRefreshInvalid      = errors.New("refresh token 无效或已过期")
	ErrRefreshReused       = errors.New("refresh token 已被使用（疑似重放）")
	ErrUnsupportedProvider = errors.New("不支持的认证提供方")
	// ErrInvalidCredentials 表示换取的凭证不成立（code 无效/过期、微信返回错误等）。
	// auth 包刻意不 import domain —— 外部依赖的适配层不该知道业务错误语义。
	ErrInvalidCredentials = errors.New("认证凭证无效")
)

// newRandomID 生成 16 字节随机串，用于 jti 等非安全关键标识。
func newRandomID() string { return mustRandom(16) }

// newRefreshToken 生成 32 字节随机串作为不透明 refresh token。
// 为什么不用 JWT 做 refresh：JWT 无法即时吊销，要补 Redis 黑名单 —— 那等于已经存了
// Redis 还多付一次验签成本。直接用不透明串更简单，且天然支持一次性轮换。
func newRefreshToken() string { return mustRandom(32) }

func mustRandom(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 失败意味着系统熵源不可用，此时继续跑只会签发可预测的令牌，
		// 属于不可恢复的严重故障，直接 panic 比返回可被忽略的 error 更安全。
		panic(fmt.Sprintf("crypto/rand 不可用: %v", err))
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
