package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// RefreshRecord 是存在 Redis 里的 refresh token 载荷。
type RefreshRecord struct {
	UserID   string `json:"uid"`
	Role     string `json:"role"`
	FamilyID string `json:"fid"`
}

// RefreshStore 用 Redis 存不透明 refresh token，支持：
//   - 一次性轮换（用完即删、发新的）
//   - 重放检测（已消费的 token 再次出现 → 吊销整个 family，把攻击者踢下线）
//   - 即时吊销（登出/封号立刻生效，这是 JWT 做不到的）
type RefreshStore struct {
	rdb    *redis.Client
	ttl    time.Duration
	prefix string
}

func NewRefreshStore(rdb *redis.Client, ttl time.Duration) *RefreshStore {
	return &RefreshStore{rdb: rdb, ttl: ttl, prefix: "refresh"}
}

func (s *RefreshStore) tokKey(token string) string  { return s.prefix + ":tok:" + token }
func (s *RefreshStore) usedKey(token string) string { return s.prefix + ":used:" + token }
func (s *RefreshStore) famKey(familyID string) string {
	return s.prefix + ":fam:" + familyID
}

// Issue 开一个新的 token family（登录时调用）。
func (s *RefreshStore) Issue(ctx context.Context, userID, role string) (string, error) {
	return s.issueInFamily(ctx, userID, role, mustRandom(16))
}

func (s *RefreshStore) issueInFamily(ctx context.Context, userID, role, familyID string) (string, error) {
	token := newRefreshToken()
	rec := RefreshRecord{UserID: userID, Role: role, FamilyID: familyID}
	payload, err := json.Marshal(rec)
	if err != nil {
		return "", fmt.Errorf("序列化 refresh 记录失败: %w", err)
	}

	pipe := s.rdb.TxPipeline()
	pipe.Set(ctx, s.tokKey(token), payload, s.ttl)
	// family 集合记录该家族下所有 token，用于一次性吊销
	pipe.SAdd(ctx, s.famKey(familyID), token)
	pipe.Expire(ctx, s.famKey(familyID), s.ttl)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", fmt.Errorf("写入 refresh token 失败: %w", err)
	}
	return token, nil
}

// Rotate 消费一个 refresh token 并签发新的。
//
// 返回值里的 reused=true 表示检测到重放：此时整个 family 已被吊销，
// 调用方应当让用户重新登录。
func (s *RefreshStore) Rotate(ctx context.Context, token string) (newToken string, rec RefreshRecord, reused bool, err error) {
	if token == "" {
		return "", rec, false, ErrRefreshInvalid
	}

	raw, getErr := s.rdb.Get(ctx, s.tokKey(token)).Bytes()
	if errors.Is(getErr, redis.Nil) {
		// token 不在：要么过期，要么已经被用过。用"已用"标记来区分这两种情况。
		famID, uErr := s.rdb.Get(ctx, s.usedKey(token)).Result()
		switch {
		case uErr == nil:
			// 已经消费过的 token 又出现了 —— 重放。整个家族一并吊销。
			_ = s.RevokeFamily(ctx, famID)
			return "", rec, true, ErrRefreshReused
		case errors.Is(uErr, redis.Nil):
			return "", rec, false, ErrRefreshInvalid
		default:
			return "", rec, false, fmt.Errorf("读取重放标记失败: %w", uErr)
		}
	}
	if getErr != nil {
		return "", rec, false, fmt.Errorf("读取 refresh token 失败: %w", getErr)
	}

	if err := json.Unmarshal(raw, &rec); err != nil {
		return "", rec, false, fmt.Errorf("解析 refresh 记录失败: %w", err)
	}

	// 消费：删掉旧的，打上"已用"标记（TTL 与 token 相同，覆盖重放检测窗口）
	pipe := s.rdb.TxPipeline()
	pipe.Del(ctx, s.tokKey(token))
	pipe.Set(ctx, s.usedKey(token), rec.FamilyID, s.ttl)
	pipe.SRem(ctx, s.famKey(rec.FamilyID), token)
	if _, err := pipe.Exec(ctx); err != nil {
		return "", rec, false, fmt.Errorf("轮换 refresh token 失败: %w", err)
	}

	newToken, err = s.issueInFamily(ctx, rec.UserID, rec.Role, rec.FamilyID)
	if err != nil {
		return "", rec, false, err
	}
	return newToken, rec, false, nil
}

// RevokeFamily 吊销整个家族的所有 token（登出、封号、检测到重放时调用）。
func (s *RefreshStore) RevokeFamily(ctx context.Context, familyID string) error {
	tokens, err := s.rdb.SMembers(ctx, s.famKey(familyID)).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return fmt.Errorf("读取 token family 失败: %w", err)
	}

	pipe := s.rdb.TxPipeline()
	for _, t := range tokens {
		pipe.Del(ctx, s.tokKey(t))
	}
	pipe.Del(ctx, s.famKey(familyID))
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("吊销 token family 失败: %w", err)
	}
	return nil
}

// RevokeToken 登出：删掉该 token 本身并吊销其家族。
func (s *RefreshStore) RevokeToken(ctx context.Context, token string) error {
	raw, err := s.rdb.Get(ctx, s.tokKey(token)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil // 已经不存在，视作已登出
	}
	if err != nil {
		return fmt.Errorf("读取 refresh token 失败: %w", err)
	}
	var rec RefreshRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("解析 refresh 记录失败: %w", err)
	}
	return s.RevokeFamily(ctx, rec.FamilyID)
}
