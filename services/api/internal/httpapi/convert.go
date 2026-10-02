package httpapi

import (
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/tutic/itiao-mall/services/api/internal/httpapi/gen"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// 这一层只做一件事：把 service/repo 的领域类型翻译成契约生成的传输类型。
// 不掺业务判断 —— 一旦这里出现 if/else 业务逻辑，说明放错地方了。

func toUUID(s string) openapi_types.UUID {
	u, err := uuid.Parse(s)
	if err != nil {
		// ID 全部来自数据库（uuid 列），正常不会解析失败。
		// 真出现了说明数据被外力破坏，返回零值让上层表现为"资源不存在"，比 panic 好。
		return openapi_types.UUID{}
	}
	return openapi_types.UUID(u)
}

func derefStr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefInt32(p *int, def int32) int32 {
	if p == nil {
		return def
	}
	return int32(*p)
}

func derefInt64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

func toGenUser(u repo.User) gen.User {
	avatar := u.AvatarUrl
	user := gen.User{
		Id:        toUUID(u.ID),
		Role:      gen.UserRole(u.Role),
		Nickname:  u.Nickname,
		AvatarUrl: &avatar,
	}
	if u.Phone != nil {
		user.Phone = u.Phone
	}
	return user
}

func toGenSession(s *service.Session) gen.AuthSession {
	return gen.AuthSession{
		AccessToken:  s.AccessToken,
		RefreshToken: s.RefreshToken,
		ExpiresAt:    s.ExpiresAt,
		User:         toGenUser(s.User),
	}
}

func toGenProductSummary(row repo.ListProductsRow) gen.ProductSummary {
	cat := row.Category
	cover := row.CoverUrl
	return gen.ProductSummary{
		Id:         toUUID(row.ID),
		Title:      row.Title,
		CoverUrl:   &cover,
		Category:   &cat,
		PriceCents: gen.Money(row.MinPriceCents),
		Status:     gen.ProductSummaryStatus(row.Status),
	}
}

func toGenSku(s repo.Sku) gen.Sku {
	return gen.Sku{
		Id:         toUUID(s.ID),
		ProductId:  toUUID(s.ProductID),
		Name:       s.Name,
		PriceCents: gen.Money(s.PriceCents),
		Stock:      int(s.Stock),
	}
}

// toGenProductDetail 的展示价取所有 SKU 里的最低价，与列表页口径保持一致。
func toGenProductDetail(p repo.Product, skus []repo.Sku) gen.ProductDetail {
	var minPrice int64
	for i, s := range skus {
		if i == 0 || s.PriceCents < minPrice {
			minPrice = s.PriceCents
		}
	}
	cat := p.Category
	cover := p.CoverUrl
	genSkus := make([]gen.Sku, 0, len(skus))
	for _, s := range skus {
		genSkus = append(genSkus, toGenSku(s))
	}
	return gen.ProductDetail{
		Id:          toUUID(p.ID),
		Title:       p.Title,
		Description: p.Description,
		CoverUrl:    &cover,
		Category:    &cat,
		PriceCents:  gen.Money(minPrice),
		Status:      gen.ProductDetailStatus(p.Status),
		Skus:        genSkus,
	}
}

func toGenCart(rows []repo.ListCartItemsRow, total int64) gen.Cart {
	items := make([]gen.CartItem, 0, len(rows))
	for _, r := range rows {
		cover := r.CoverUrl
		items = append(items, gen.CartItem{
			SkuId:          toUUID(r.SkuID),
			ProductId:      toUUID(r.ProductID),
			Title:          r.Title,
			SkuName:        r.SkuName,
			CoverUrl:       &cover,
			UnitPriceCents: gen.Money(r.PriceCents),
			Qty:            int(r.Qty),
			Stock:          int(r.Stock),
		})
	}
	return gen.Cart{Items: items, TotalCents: gen.Money(total)}
}

func toGenOrderItem(it repo.OrderItem) gen.OrderItem {
	return gen.OrderItem{
		SkuId:          toUUID(it.SkuID),
		TitleSnapshot:  it.TitleSnapshot,
		UnitPriceCents: gen.Money(it.UnitPriceCents),
		Qty:            int(it.Qty),
	}
}

func toGenOrder(v *service.OrderView) gen.Order {
	items := make([]gen.OrderItem, 0, len(v.Items))
	for _, it := range v.Items {
		items = append(items, toGenOrderItem(it))
	}
	var paidAt *time.Time
	if v.Order.PaidAt.Valid {
		t := v.Order.PaidAt.Time
		paidAt = &t
	}
	return gen.Order{
		Id:         toUUID(v.Order.ID),
		OrderNo:    v.Order.OrderNo,
		Status:     gen.OrderStatus(v.Order.Status),
		TotalCents: gen.Money(v.Order.TotalCents),
		ExpiresAt:  v.Order.ExpiresAt.Time,
		PaidAt:     paidAt,
		CreatedAt:  v.Order.CreatedAt.Time,
		Items:      items,
	}
}
