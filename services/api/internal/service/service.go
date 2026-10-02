// Package service 承载用例编排：事务、状态机、幂等都在这一层，
// 它不知道 HTTP，也不知道 SQL 细节。
package service

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// Deps 是业务层的全部依赖。
type Deps struct {
	Q      *repo.Queries
	Pool   *pgxpool.Pool
	Redis  *redis.Client
	Issuer *auth.TokenIssuer
	// 字段名刻意不叫 Refresh —— 否则与 Service.Refresh 方法同名，Go 不允许字段与方法同名。
	RefreshStore *auth.RefreshStore
	Wechat       auth.WechatExchanger
	Payment      payment.Provider
	Logger       *slog.Logger
	PendingTTL   time.Duration
}

type Service struct {
	Deps
}

func New(d Deps) *Service { return &Service{Deps: d} }

// Session 是登录/刷新后返回给客户端的凭证组合。
type Session struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	User         repo.User
}

// OrderLineInput 是下单明细。刻意不复用生成的 HTTP 类型 —— service 层不应依赖传输层类型。
type OrderLineInput struct {
	SkuID string
	Qty   int32
}

// OrderView 是订单及其明细。
type OrderView struct {
	Order repo.Order
	Items []repo.OrderItem
}

// ---- 通用小工具 ----

// pgTime 把 pgtype.Timestamptz 转成 time.Time，NULL 返回零值。
func pgTime(ts pgtype.Timestamptz) time.Time {
	if !ts.Valid {
		return time.Time{}
	}
	return ts.Time
}

// pgTimePtr 转成 *time.Time，NULL 返回 nil。
func pgTimePtr(ts pgtype.Timestamptz) *time.Time {
	if !ts.Valid {
		return nil
	}
	t := ts.Time
	return &t
}

// isUniqueViolation 判断是否违反了唯一约束（Postgres 23505）。
// 用于幂等兜底：并发重复下单时，靠数据库唯一约束做最终裁决。
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

var orderNoEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// newOrderNo 生成订单号：14 位时间戳 + 8 位随机，可读且足够唯一。
func newOrderNo(now time.Time) string {
	b := make([]byte, 5)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand 不可用: %v", err))
	}
	return now.Format("20060102150405") + orderNoEncoding.EncodeToString(b)
}

// normalizePage 把分页参数收进合法区间，避免 page=0 或 page_size=100000 把库打穿。
func normalizePage(page, pageSize int32) (limit, offset int32) {
	if page < 1 {
		page = 1
	}
	switch {
	case pageSize < 1:
		pageSize = 20
	case pageSize > 100:
		pageSize = 100
	}
	return pageSize, (page - 1) * pageSize
}

// normalizeSearch 把空串转成 nil，这样 sqlc.narg 的"条件不生效"才能正确工作。
func normalizeSearch(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

// pgtypeTimestamptz 把非空 time.Time 包成 pgtype.Timestamptz。
func pgtypeTimestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}

// mustJSON 序列化事件原文用于留档。序列化失败不阻断主流程 —— 留档比业务更重要，
// 但也不能因为留档失败就把一笔已成功的支付搞失败。
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{}`)
	}
	return b
}
