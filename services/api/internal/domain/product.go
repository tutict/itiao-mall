package domain

import "fmt"

// ProductStatus 是商品状态机的状态集合。
//
// 与 DB 的 products_status_check 一一对应 —— 改这里必须同时改
// 0002 迁移里的约束，反之亦然，否则会出现「代码认为合法、数据库插入失败」。
type ProductStatus string

const (
	// ProductDraft 草稿：只有商户自己看得到，不出现在任何公开列表里。
	ProductDraft ProductStatus = "DRAFT"
	// ProductPendingReview 已提交待审：平台审核队列里的待办。
	ProductPendingReview ProductStatus = "PENDING_REVIEW"
	// ProductOnSale 在售：出现在用户端列表里，可下单。
	ProductOnSale ProductStatus = "ON_SALE"
	// ProductOffSale 已下架：曾经通过审核，但当前不卖。
	ProductOffSale ProductStatus = "OFF_SALE"
	// ProductRejected 审核驳回：商户改了之后可重新提交。
	ProductRejected ProductStatus = "REJECTED"
)

// allowedProductTransitions 是商品状态机唯一的真源。
//
//	DRAFT ──提交审核──> PENDING_REVIEW ──通过──> ON_SALE <──上架── OFF_SALE
//	  │                      │                      │
//	  └──改资料后重新提交──>  └──驳回──> REJECTED ────┘
//	                            REJECTED ──改资料后重新提交──> PENDING_REVIEW
//	                            ON_SALE ──下架──> OFF_SALE
//
// 三个刻意的设计决定：
//
//  1. **没有 DRAFT → ON_SALE 这条边。** 商户不能自己把商品变成在售，
//     必须经平台审核。这是"平台对商品有控制力"这条产品需求的全部实现 ——
//     加一条捷径就等于把它架空。
//  2. **OFF_SALE → ON_SALE 允许直接上架。** 下架不等于下架待审：
//     已经审过一次的商品，商户自己下架又上架不需要平台再看一遍。
//     如果连这条边都去掉，商户的日常运营就变成了每次改价都要等审核，
//     那平台会被审核量压垮，而商户会倾向于不改价（于是数据更陈旧）。
//  3. **REJECTED → PENDING_REVIEW 需要经过一次编辑。** 边上没有直接的
//     "重新提交"：不给出理由的驳回不该能被原样再提交一次。
//     实现上"编辑即重新提交"（见 UpsertProduct 的 SQL 注释）。
var allowedProductTransitions = map[ProductStatus][]ProductStatus{
	ProductDraft:         {ProductPendingReview, ProductOffSale},
	ProductPendingReview: {ProductOnSale, ProductRejected, ProductDraft},
	ProductOnSale:        {ProductOffSale, ProductDraft},
	ProductOffSale:       {ProductOnSale, ProductDraft},
	ProductRejected:      {ProductPendingReview, ProductDraft},
}

// IsTerminal 判断是否终态。商品状态机当前没有真正的终态
// （被驳回总能改完再提交），保留这个方法是为了将来接入"违规下架且不可恢复"。
func (s ProductStatus) IsTerminal() bool {
	return len(allowedProductTransitions[s]) == 0
}

// Valid 判断是否是已知状态。
func (s ProductStatus) Valid() bool {
	_, ok := allowedProductTransitions[s]
	return ok
}

// CanTransitionTo 判断能否从当前状态流转到 next。
func (s ProductStatus) CanTransitionTo(next ProductStatus) bool {
	for _, allowed := range allowedProductTransitions[s] {
		if allowed == next {
			return true
		}
	}
	return false
}

// TransitionTo 校验并返回下一个状态；非法流转返回 ErrInvalidTransition。
func (s ProductStatus) TransitionTo(next ProductStatus) (ProductStatus, error) {
	if !s.Valid() {
		return s, fmt.Errorf("%w: 未知状态 %q", ErrInvalidTransition, s)
	}
	if !next.Valid() {
		return s, fmt.Errorf("%w: 未知目标状态 %q", ErrInvalidTransition, next)
	}
	if !s.CanTransitionTo(next) {
		return s, fmt.Errorf("%w: 商品状态 %s → %s", ErrInvalidTransition, s, next)
	}
	return next, nil
}

// IsPublic 判断该状态的商品是否出现在用户端的公开列表里。
//
// 单独抽出来是因为"能不能被用户看到"这条知识会被 SQL（WHERE status='ON_SALE'）
// 与状态机（CanTransitionTo）分别用到。集中在这里是为了将来加状态时
// 只需改一处 —— 两边各写一份的话，最可能的漏法是"新状态能上架但列表查不出来"。
func (s ProductStatus) IsPublic() bool { return s == ProductOnSale }

// IsMerchantEditable 判断商户能否编辑该状态商品的字段。
//
// 规则：只有 DRAFT 与 REJECTED 可编辑。ON_SALE 不可编辑是因为
// 已经有人在买，改标题会造成"下单时看到 A、付款后看到 B"；
// PENDING_REVIEW 不可编辑是因为审核员正在看这一版，改了等于
// "审的不是我提交的那份"。
//
// 想要改在售商品的商户，路径是：下架（ON_SALE → OFF_SALE）→ 编辑（→ DRAFT）
// → 重新提交审核。这条路径多两步，但每一步的语义都是清楚的。
func (s ProductStatus) IsMerchantEditable() bool {
	return s == ProductDraft || s == ProductRejected
}

// IsAdminDecidable 判断该状态能否被平台裁决。
//
// 只有 PENDING_REVIEW。这是 CAS 的前提：只有待审的能被裁决，
// 于是"两个管理员同时点通过/驳回"里第二个人的影响行数必然为 0。
func (s ProductStatus) IsAdminDecidable() bool { return s == ProductPendingReview }
