package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// LoginWechat 用户端一键登录：code → openid/unionid → 查或建用户 → 签发凭证。
func (s *Service) LoginWechat(ctx context.Context, code, nickname, avatarURL string) (*Session, error) {
	wu, err := s.Wechat.Exchange(ctx, code)
	if err != nil {
		return nil, err
	}

	user, err := s.Q.GetUserByWechatUnionID(ctx, repo.GetUserByWechatUnionIDParams{
		UnionID: nullIfEmpty(wu.UnionID),
		OpenID:  wu.OpenID,
	})
	switch {
	case err == nil:
		// 已有用户，直接用
	case errors.Is(err, pgx.ErrNoRows):
		// 首次登录，建号。昵称/头像优先用客户端传的，其次用微信换回来的。
		user, err = s.Q.CreateWechatUser(ctx, repo.CreateWechatUserParams{
			OpenID:    &wu.OpenID,
			UnionID:   nullIfEmpty(wu.UnionID),
			Nickname:  firstNonEmpty(nickname, wu.Nickname, "微信用户"),
			AvatarUrl: firstNonEmpty(avatarURL, wu.AvatarURL),
		})
		if err != nil {
			return nil, fmt.Errorf("创建用户失败: %w", err)
		}
	default:
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}

	if err := ensureActive(user); err != nil {
		return nil, err
	}
	return s.issueSession(ctx, user)
}

// LoginMerchant 商户端登录：手机号 + 密码。
func (s *Service) LoginMerchant(ctx context.Context, phone, password string) (*Session, error) {
	return s.loginWithPassword(ctx, phone, password, auth.RoleMerchant)
}

// LoginAdmin 管理员端登录：手机号 + 密码。
//
// 与商户端登录**共用实现**，但角色要求各自写死。这样「手机号 + 密码」这套凭证
// 只能从它对应的入口换到对应角色的令牌：商户入口拒收 admin 账号，管理员入口拒收
// merchant 账号。共用一个入口的话，商户端就能拿到 admin 的令牌，而前端拿到的身份
// 与它以为的对不上 —— 这类错配在客户端极难发现，通常要等越权事故发生才暴露。
func (s *Service) LoginAdmin(ctx context.Context, phone, password string) (*Session, error) {
	return s.loginWithPassword(ctx, phone, password, auth.RoleAdmin)
}

// loginWithPassword 是手机号 + 密码登录的共用实现。
func (s *Service) loginWithPassword(ctx context.Context, phone, password, wantRole string) (*Session, error) {
	user, err := s.Q.GetUserByPhone(ctx, phone)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 不区分"用户不存在"和"密码错误"，避免账号枚举
			return nil, domain.ErrInvalidCredentials
		}
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	if user.PasswordHash == nil || *user.PasswordHash == "" {
		return nil, domain.ErrInvalidCredentials
	}
	if err := bcrypt.CompareHashAndPassword([]byte(*user.PasswordHash), []byte(password)); err != nil {
		return nil, domain.ErrInvalidCredentials
	}
	if user.Role != wantRole {
		// 刻意回 403 而不是 401：凭证本身是对的，只是这个入口不接待这个角色。
		// 回 401 会让调用方以为密码错了，然后反复重试。
		return nil, domain.ErrForbidden
	}
	if err := ensureActive(user); err != nil {
		return nil, err
	}
	return s.issueSession(ctx, user)
}

// Refresh 轮换 refresh token 并签发新的 access token。
// 完整实现见 token.go —— 权威判定在数据库 CAS，Redis 只做加速与互斥。

// Logout 吊销该 token 所属的整个 family。
//
// 两个存储都处理，顺序是「数据库 → Redis」：数据库是权威，先吊它；
// Redis 失败只记日志。反过来的话，Redis 里已经删了而数据库还留着，
// 用户会以为登出成功了，实际换个入口（或等缓存过期）又能用。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}

	hash := auth.HashToken(refreshToken)

	// 先在权威侧定位 family。查不到就直接成功返回：
	// 登出是幂等操作，"这个 token 不存在"与"已经登出了"对调用方无差别。
	tok, err := s.Q.GetRefreshTokenByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 数据库里没有，但 Redis 里可能有（理论上不该：写库在前）。
			// 仍尝试吊销 Redis 侧，让登出在两个存储上都成立。
			if rerr := s.RefreshStore.RevokeToken(ctx, refreshToken); rerr != nil {
				s.Logger().Warn("登出时吊销 Redis 缓存失败", "err", rerr)
			}
			return nil
		}
		return fmt.Errorf("查询 refresh 记录失败: %w", err)
	}

	if _, err := s.Q.RevokeRefreshFamily(ctx, tok.FamilyID); err != nil {
		return fmt.Errorf("吊销会话失败: %w", err)
	}
	if _, err := s.RefreshStore.RevokeFamily(ctx, tok.FamilyID); err != nil {
		// 数据库已吊销，权威侧安全。Redis 残留会在 TTL 到期后消失。
		s.Logger().Warn("登出时吊销 Redis 缓存失败（数据库已吊销）", "err", err)
	}
	return nil
}

func (s *Service) GetUser(ctx context.Context, userID string) (repo.User, error) {
	user, err := s.Q.GetUserByID(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return repo.User{}, domain.ErrNotFound
		}
		return repo.User{}, fmt.Errorf("查询用户失败: %w", err)
	}
	return user, nil
}

// ensureActive 封号用户不允许登录/刷新/下单。
func ensureActive(user repo.User) error {
	if user.Status != "ACTIVE" {
		return fmt.Errorf("%w: 账号已被停用", domain.ErrForbidden)
	}
	return nil
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
