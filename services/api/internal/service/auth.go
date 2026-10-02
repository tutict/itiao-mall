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
// 检测到重放时，Rotate 内部会吊销整个 family，这里把错误原样上抛。
func (s *Service) Refresh(ctx context.Context, refreshToken string) (*Session, error) {
	newRefresh, rec, _, err := s.RefreshStore.Rotate(ctx, refreshToken)
	if err != nil {
		return nil, err
	}

	user, err := s.Q.GetUserByID(ctx, rec.UserID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrRefreshTokenInvalid
		}
		return nil, fmt.Errorf("查询用户失败: %w", err)
	}
	if err := ensureActive(user); err != nil {
		return nil, err
	}

	access, exp, err := s.Issuer.IssueAccess(user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	return &Session{AccessToken: access, RefreshToken: newRefresh, ExpiresAt: exp, User: user}, nil
}

// Logout 吊销该 token 所属的整个 family。
func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	if refreshToken == "" {
		return nil
	}
	return s.RefreshStore.RevokeToken(ctx, refreshToken)
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

func (s *Service) issueSession(ctx context.Context, user repo.User) (*Session, error) {
	access, exp, err := s.Issuer.IssueAccess(user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	refresh, err := s.RefreshStore.Issue(ctx, user.ID, user.Role)
	if err != nil {
		return nil, err
	}
	return &Session{AccessToken: access, RefreshToken: refresh, ExpiresAt: exp, User: user}, nil
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
