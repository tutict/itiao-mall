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
	"math"
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
	// RefreshTTL 是 refresh token 的有效期。必须由配置注入而不是在 service 里
	// 写死：它要同时决定 Redis 缓存的 TTL 与数据库行的 expires_at，
	// 两处一旦不一致就会出现"缓存先过期"或"库里的行还在但缓存没了"的窗口。
	RefreshTTL time.Duration
}

type Service struct {
	Deps
}

func New(d Deps) *Service { return &Service{Deps: d} }

// Logger 返回绑定了本服务的 logger，允许为空（此时不记日志）。
//
// 之所以要这个访问器而不是到处直接用 s.Logger 字段：
// Deps.Logger 可能在测试里是 nil，而 token.go 里有十几处 s.Logger().Warn ——
// 直接用字段的话每处都要写 `if s.Logger != nil`，
// 而那种防御性判空一旦有一处漏掉就是 panic，测试期偶发、本地难复现。
// 收在这里一处判，全代码路径都不会漏。
func (s *Service) Logger() *slog.Logger {
	if s.Deps.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Deps.Logger
}

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
//
// 第二道防线（第一道是 httpapi 层的 derefInt32，拦 int64→int32 截断）。
// 任何调用方绕过 oapi-codegen 就只剩这一道，所以它必须自己能兜住。
//
// 溢出是这里唯一真正危险的地方：契约里 page 只有 minimum 没有 maximum，客户端可以
// 传任意 int32，于是 (page-1)*pageSize 会溢出：
//
//	(2147483647-1) * 100 = -200   ← int32 溢出成负数
//
// 负 OFFSET 送到 PostgreSQL 会让整条查询报错（不是返回空集），也就是一个 500。
// 而客户端那边看到的是"翻到很后面时接口开始报错了"——极难定位。
//
// 所以先除后乘并做上界判断：翻到第 21,474,837 页（offset 接近 int32 上界）就直接
// 夹到最后一页。这么深的分页本来就没人翻，夹住不损失任何真实场景。
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
	// 先除后乘：page-1 的上限是 (math.MaxInt32-1)/pageSize，乘回去就不可能溢出。
	// 这样算出的 maxPage 本身就是可表示的最大页码，不需要再单独判溢出。
	maxPage := math.MaxInt32/pageSize + 1
	if page > maxPage {
		page = maxPage
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
