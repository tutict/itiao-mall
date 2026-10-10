package httpapi

import (
	"math"
	"strings"
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

// derefInt32 把契约的 *int 收窄成 int32。
//
// 收窄必须显式判范围：契约里 page 只有 minimum 没有 maximum（业务上页码没有
// 上限），而 int64 → int32 的无条件转换在 page=2147483648 时会截断成 -2147483648，
// 随后被 normalizePage 悄悄改写成 1 —— 客户端请求第 2147483648 页，拿到的是第 1 页，
// 而且**没有任何报错**。这种静默错页比报错更难查。
//
// 越界时回落到默认值（等价于"参数没填"），与 normalizePage 对非法值的既有处理
// 保持一致：分页参数从来不是强约束，越界不该让整个请求失败。
func derefInt32(p *int, def int32) int32 {
	if p == nil {
		return def
	}
	// 用 int64 中转做比较，避免在 int32 边界上比较溢出。
	if n := int64(*p); n >= math.MinInt32 && n <= math.MaxInt32 {
		return int32(n)
	}
	return def
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

// ---- 商户入驻 ----

// toGenMerchantProfile 把领域视图翻成契约类型。
//
// 空串 → 省略字段（而不是回一个空字符串）：
// 契约里这些字段都是 optional，省略能让客户端用 `?? 兜底` 这种惯用法，
// 而回空串会让它无法区分「没有值」和「值就是空的」。
func toGenMerchantProfile(v *service.MerchantProfileView) gen.MerchantProfile {
	out := gen.MerchantProfile{
		UserId:     toUUID(v.UserID),
		ShopName:   v.ShopName,
		Status:     gen.MerchantProfileStatus(v.Status),
		CreatedAt:  v.CreatedAt,
		UpdatedAt:  v.UpdatedAt,
		ReviewedAt: v.ReviewedAt,
	}
	if v.ContactName != "" {
		out.ContactName = &v.ContactName
	}
	if v.ContactPhone != "" {
		out.ContactPhone = &v.ContactPhone
	}
	if v.Description != "" {
		out.Description = &v.Description
	}
	if v.LogoURL != "" {
		out.LogoUrl = &v.LogoURL
	}
	if v.RejectReason != "" {
		out.RejectReason = &v.RejectReason
	}
	if v.Phone != "" {
		out.Phone = &v.Phone
	}
	return out
}

// ---- 商户商品 ----

// toGenMerchantProduct 把领域视图翻成契约类型。
//
// 空串 → 省略字段，理由与 toGenMerchantProfile 一致：
// 客户端要能区分「没有值」与「值就是空的」。
//
// 注意 MinPriceCents 与 TotalStock 是**指针**：契约把它们声明为 optional
// （allOf + 没有 required），而"0 元"是一个合法的商品价格 ——
// 用值类型的话 omitempty 会把 0 抹掉，客户端就分不清"免费"和"没填"。
func toGenMerchantProduct(v *service.ProductView) gen.MerchantProduct {
	minPrice := gen.Money(v.MinPriceCents)
	totalStock := v.TotalStock
	out := gen.MerchantProduct{
		Id:            toUUID(v.ID),
		MerchantId:    toUUID(v.MerchantID),
		Title:         v.Title,
		Status:        gen.ProductStatus(v.Status),
		MinPriceCents: &minPrice,
		TotalStock:    &totalStock,
		SubmittedAt:   v.SubmittedAt,
		ReviewedAt:    v.ReviewedAt,
		CreatedAt:     v.CreatedAt,
		UpdatedAt:     v.UpdatedAt,
		Skus:          toGenSkus(v.Skus),
	}
	if v.Description != "" {
		out.Description = &v.Description
	}
	if v.Category != "" {
		out.Category = &v.Category
	}
	if v.CoverURL != "" {
		out.CoverUrl = &v.CoverURL
	}
	if v.RejectReason != "" {
		out.RejectReason = &v.RejectReason
	}
	return out
}

// toGenReviewProduct 在商户视图之上补一个店铺名。
// 刻意不复用 toGenMerchantProduct 再补字段：那样要先把结果取出来改，
// 而 allOf 生成的类型不是同一个类型，没有继承可走。
func toGenReviewProduct(v *service.ReviewProductView) gen.ReviewProduct {
	base := toGenMerchantProduct(&v.ProductView)
	out := gen.ReviewProduct{
		Id:            base.Id,
		MerchantId:    base.MerchantId,
		Title:         base.Title,
		Description:   base.Description,
		Category:      base.Category,
		CoverUrl:      base.CoverUrl,
		Status:        base.Status,
		RejectReason:  base.RejectReason,
		MinPriceCents: base.MinPriceCents,
		TotalStock:    base.TotalStock,
		SubmittedAt:   base.SubmittedAt,
		ReviewedAt:    base.ReviewedAt,
		CreatedAt:     base.CreatedAt,
		UpdatedAt:     base.UpdatedAt,
		Skus:          base.Skus,
	}
	if v.ShopName != "" {
		out.ShopName = &v.ShopName
	}
	return out
}

func toGenSkus(skus []repo.Sku) []gen.Sku {
	// 刻意回空切片而不是 nil：契约里 skus 是 required，nil 会序列化成 null，
	// 而客户端的 `skus.map(...)` 会因此炸掉。空数组才是"没有规格"。
	out := make([]gen.Sku, 0, len(skus))
	for _, s := range skus {
		out = append(out, toGenSku(s))
	}
	return out
}

// toServiceProductInput 把契约请求体翻成 service 的输入。
//
// 单独抽出来是因为创建与编辑共用它 —— 两个入口的校验规则必须完全一致，
// 而"两边各写一份 trim"是最容易出现的行为分叉的地方。
func toServiceProductInput(body *gen.MerchantProductRequest) service.ProductInput {
	in := service.ProductInput{
		Title:       strings.TrimSpace(body.Title),
		Description: strings.TrimSpace(derefStr(body.Description)),
		Category:    strings.TrimSpace(derefStr(body.Category)),
		CoverURL:    strings.TrimSpace(derefStr(body.CoverUrl)),
		Skus:        make([]service.SkuInput, 0, len(body.Skus)),
	}
	for _, s := range body.Skus {
		in.Skus = append(in.Skus, service.SkuInput{
			Name:       strings.TrimSpace(s.Name),
			PriceCents: int64(s.PriceCents),
			Stock:      int32(s.Stock),
		})
	}
	return in
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
