package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// 商品状态的对外别名。service 层对外只暴露这四个常量，
// 免得 handler 层去 import domain —— 那会让 HTTP 层知道状态机的内部表示。
const (
	ProductDraft         = string(domain.ProductDraft)
	ProductPendingReview = string(domain.ProductPendingReview)
	ProductOnSale        = string(domain.ProductOnSale)
	ProductOffSale       = string(domain.ProductOffSale)
	ProductRejected      = string(domain.ProductRejected)
)

// IsProductStatus 判断一个状态字面量是否已知。
// 作用是把 URL query 里的任意字符串挡在 SQL 之前：直接透传不会出错，
// 但会静默返回空列表，调用方会以为"没有数据"而不是"你传错了"。
func IsProductStatus(s string) bool {
	return domain.ProductStatus(s).Valid()
}

// 字段上限。与契约的 maxLength 一一对应 —— 契约挡不下来的（内部调用）
// 由这里挡。两处各留一半会导致"契约说有上限、直接调 service 却能超"。
const (
	maxProductTitleLen       = 100
	maxProductDescLen        = 2000
	maxProductCategoryLen    = 50
	maxProductCoverURLLen    = 500
	maxSkuNameLen            = 50
	maxSkusPerProduct        = 50
	maxRejectReasonLen       = 500
	maxProductTitleRunesNote = "按字符数而非字节数计：中文标题占的字节多得多"
)

// SkuInput 是商户提交的一个 SKU。
type SkuInput struct {
	Name       string
	PriceCents int64
	Stock      int32
}

// ProductInput 是商户提交的商品与它的全部 SKU。
type ProductInput struct {
	Title       string
	Description string
	Category    string
	CoverURL    string
	Skus        []SkuInput
}

// ProductView 是商户视角的商品视图。
//
// 为什么多这一层：sqlc 为「列表查询」与「单条查询」生成了两个字段不同
// 的行类型，它们不能互相赋值。让它们直接流到 HTTP 层的话，转换逻辑就有两份 ——
// 将来加字段极容易只改一处，表现为"列表里有、详情里没有"这种最难查的不一致。
// 与 MerchantProfileView 是同一个理由。
type ProductView struct {
	ID            string
	MerchantID    string
	Title         string
	Description   string
	Category      string
	CoverURL      string
	Status        string
	RejectReason  string
	MinPriceCents int64
	TotalStock    int64
	SubmittedAt   *time.Time
	ReviewedAt    *time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
	Skus          []repo.Sku
}

// ReviewProductView 是平台视角的商品视图，比商户视角多一个店铺名。
// 审核员要靠店名对上号 —— 没有它，一个商品列表里混着几十家店的商品，
// 审核就变成了猜。
type ReviewProductView struct {
	ProductView
	ShopName string
}

// ---- 创建 ----

// CreateProduct 新建商品（草稿）。
//
// 商品与 SKU 在**同一个事务**里创建。分两步写的话，进程在中间挂掉就留下一个
// 「有商品、没有 SKU」的空壳 —— 它能提交审核、能被通过，然后用户点进去发现
// 买不了任何规格。审核队列里出现这种商品，审核员只能手工下架。
func (s *Service) CreateProduct(ctx context.Context, merchantID string, in ProductInput) (*ProductView, error) {
	if err := validateProductInput(in); err != nil {
		return nil, err
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Q.WithTx(tx)

	p, err := q.CreateProduct(ctx, repo.CreateProductParams{
		MerchantID:  merchantID,
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Category:    strings.TrimSpace(in.Category),
		CoverUrl:    strings.TrimSpace(in.CoverURL),
	})
	if err != nil {
		return nil, fmt.Errorf("创建商品失败: %w", err)
	}

	for _, sku := range in.Skus {
		if _, err := q.CreateSku(ctx, repo.CreateSkuParams{
			ProductID:  p.ID,
			Name:       strings.TrimSpace(sku.Name),
			PriceCents: sku.PriceCents,
			Stock:      sku.Stock,
		}); err != nil {
			return nil, fmt.Errorf("创建 SKU 失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return s.GetMerchantProduct(ctx, merchantID, p.ID)
}

// ---- 读 ----

// ListMerchantProducts 我的商品列表。
func (s *Service) ListMerchantProducts(
	ctx context.Context, merchantID, status string, page, pageSize int32,
) ([]ProductView, int64, error) {
	if status != "" && !IsProductStatus(status) {
		return nil, 0, domain.UserFacingf(
			fmt.Sprintf("未知的商品状态 %q", status), domain.ErrValidation)
	}
	lim, off := normalizePage(page, pageSize)
	statusArg := normalizeSearch(status)

	rows, err := s.Q.ListMerchantProducts(ctx, repo.ListMerchantProductsParams{
		MerchantID: merchantID,
		Status:     statusArg,
		Lim:        lim,
		Off:        off,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("查询商品列表失败: %w", err)
	}
	total, err := s.Q.CountMerchantProducts(ctx, repo.CountMerchantProductsParams{
		MerchantID: merchantID,
		Status:     statusArg,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("统计商品数失败: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	// 一次 IN 查询把所有 SKU 取回来，而不是每个商品查一次。
	//
	// 分页 20 个商品时前者是 1 次往返 + 1 次 IN，后者是 20 次往返 ——
	// 而这一页是商户后台的主列表，每次打开都要走一遍。
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	skusByProduct, err := s.skusByProductID(ctx, ids)
	if err != nil {
		return nil, 0, err
	}

	views := make([]ProductView, 0, len(rows))
	for _, row := range rows {
		v := productViewFromListRow(row)
		v.Skus = skusByProduct[row.ID]
		views = append(views, v)
	}
	return views, total, nil
}

// skusByProductID 把「一批商品 id」变成「商品 id → SKU 列表」。
//
// 抽出来是因为它有三处调用方（商户列表、审核队列、以及将来的导出），
// 而它有两个值得说清的决定：
//
//  1. **id 为空时直接返回空 map，不打数据库。** SQL 里的 `= ANY('{}')`
//     虽合法，但空数组会让应用层与数据库之间多一次无意义的往返；
//     更重要的是它逼着调用方去处理"一页 0 条"这个真实存在的分支 ——
//     而 total==0 的早退已经在上层做了。
//  2. **查不到 SKU 的商品给空切片而不是 nil。** JSON 里 null 与 []
//     对前端是两件事：null 要额外判空，[] 可以直接 map。
func (s *Service) skusByProductID(ctx context.Context, productIDs []string) (map[string][]repo.Sku, error) {
	out := make(map[string][]repo.Sku, len(productIDs))
	if len(productIDs) == 0 {
		return out, nil
	}
	skus, err := s.Q.ListSkusByProductIDs(ctx, productIDs)
	if err != nil {
		return nil, fmt.Errorf("查询 SKU 失败: %w", err)
	}
	for _, sku := range skus {
		out[sku.ProductID] = append(out[sku.ProductID], sku)
	}
	return out, nil
}

// GetMerchantProduct 读自己的某个商品（含 SKU）。
//
// 跨租户访问在这里表现为 404：SQL 里 merchant_id 与 id 一起作为条件，
// 于是"别人的商品"与"不存在的商品"在库里就无法区分，响应自然也就一样。
// 这是全项目统一的跨租户语义（见 router.go 的注释）。
func (s *Service) GetMerchantProduct(ctx context.Context, merchantID, productID string) (*ProductView, error) {
	p, err := s.Q.GetMerchantProduct(ctx, repo.GetMerchantProductParams{
		ID:         productID,
		MerchantID: merchantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("查询商品失败: %w", err)
	}
	skus, err := s.Q.ListSkusByProduct(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("查询 SKU 失败: %w", err)
	}
	return productViewFromProduct(p, skus), nil
}

// ---- 编辑 ----

// UpdateProduct 编辑商品（仅 DRAFT / REJECTED）。
//
// SKU 整体替换：先删后建，与商品更新在同一个事务里。
//
// 状态流转写在 SQL 的 CASE 里（原子），这里只负责校验与事务边界。
func (s *Service) UpdateProduct(
	ctx context.Context, merchantID, productID string, in ProductInput,
) (*ProductView, error) {
	if err := validateProductInput(in); err != nil {
		return nil, err
	}

	// 先确认商品存在且属于该商户。
	// 为什么要先查一遍而不是直接 UPDATE：区分"不存在/不是你的"（404）
	// 与"存在但状态不对"（409）。商户端需要知道是哪个 ——
	// 前者要去检查 URL，后者要去改状态，提示完全不同。
	existing, err := s.Q.GetMerchantProduct(ctx, repo.GetMerchantProductParams{
		ID: productID, MerchantID: merchantID,
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("查询商品失败: %w", err)
	}
	if !domain.ProductStatus(existing.Status).IsMerchantEditable() {
		return nil, domain.UserFacingf(
			fmt.Sprintf("当前状态为 %s，不能编辑。请先下架或撤回", existing.Status),
			domain.ErrInvalidTransition)
	}

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	q := s.Q.WithTx(tx)

	updated, err := q.UpdateMerchantProduct(ctx, repo.UpdateMerchantProductParams{
		ID:          productID,
		MerchantID:  merchantID,
		Title:       strings.TrimSpace(in.Title),
		Description: strings.TrimSpace(in.Description),
		Category:    strings.TrimSpace(in.Category),
		CoverUrl:    strings.TrimSpace(in.CoverURL),
	})
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 走到这里说明状态在"先查后写"的那个窗口里被别人改了。
			// 回 409 而不是 404：资源确实存在，只是状态不匹配。
			return nil, domain.UserFacingf("商品状态已变化，请刷新后重试", domain.ErrConflict)
		}
		return nil, fmt.Errorf("更新商品失败: %w", err)
	}

	if err := q.DeleteSkusByProduct(ctx, productID); err != nil {
		return nil, fmt.Errorf("清理旧 SKU 失败: %w", err)
	}
	for _, sku := range in.Skus {
		if _, err := q.CreateSku(ctx, repo.CreateSkuParams{
			ProductID:  updated.ID,
			Name:       strings.TrimSpace(sku.Name),
			PriceCents: sku.PriceCents,
			Stock:      sku.Stock,
		}); err != nil {
			return nil, fmt.Errorf("创建 SKU 失败: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}
	return s.GetMerchantProduct(ctx, merchantID, productID)
}

// DeleteProduct 删除商品（仅 DRAFT / REJECTED）。
//
// skus 与 cart_items 靠 ON DELETE CASCADE 一起清掉。
// 已存在的订单不受影响：order_items 存的是下单时的快照，不是外键引用。
func (s *Service) DeleteProduct(ctx context.Context, merchantID, productID string) error {
	if _, err := s.Q.DeleteMerchantProduct(ctx, repo.DeleteMerchantProductParams{
		ID: productID, MerchantID: merchantID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return domain.ErrNotFound
		}
		return fmt.Errorf("删除商品失败: %w", err)
	}
	return nil
}

// ---- 状态流转 ----

// SubmitProduct 提交审核（DRAFT → PENDING_REVIEW）。
//
// CAS 未命中时回 409 而不是 404：商品确实存在（商户自己建的），
// 只是状态已经不是草稿。回 404 会让商户以为是 URL 写错了。
func (s *Service) SubmitProduct(ctx context.Context, merchantID, productID string) (*ProductView, error) {
	return s.merchantTransition(ctx, merchantID, productID,
		func() (repo.Product, error) {
			return s.Q.SubmitProductForReview(ctx, repo.SubmitProductForReviewParams{
				ID: productID, MerchantID: merchantID,
			})
		},
		"只有草稿状态的商品能提交审核")
}

// WithdrawProduct 撤回审核（PENDING_REVIEW → DRAFT）。
func (s *Service) WithdrawProduct(ctx context.Context, merchantID, productID string) (*ProductView, error) {
	return s.merchantTransition(ctx, merchantID, productID,
		func() (repo.Product, error) {
			return s.Q.WithdrawProduct(ctx, repo.WithdrawProductParams{
				ID: productID, MerchantID: merchantID,
			})
		},
		"只有待审核状态的商品能撤回")
}

// OffShelfProduct 下架（ON_SALE → OFF_SALE）。
func (s *Service) OffShelfProduct(ctx context.Context, merchantID, productID string) (*ProductView, error) {
	return s.merchantTransition(ctx, merchantID, productID,
		func() (repo.Product, error) {
			return s.Q.OffShelfProduct(ctx, repo.OffShelfProductParams{
				ID: productID, MerchantID: merchantID,
			})
		},
		"只有在售状态的商品能下架")
}

// OnShelfProduct 上架（OFF_SALE → ON_SALE）。
//
// 刻意不要求重新审核。理由见 allowedProductTransitions 的说明。
func (s *Service) OnShelfProduct(ctx context.Context, merchantID, productID string) (*ProductView, error) {
	return s.merchantTransition(ctx, merchantID, productID,
		func() (repo.Product, error) {
			return s.Q.OnShelfProduct(ctx, repo.OnShelfProductParams{
				ID: productID, MerchantID: merchantID,
			})
		},
		"只有已下架状态的商品能上架")
}

// merchantTransition 是四个状态流转动作的共用骨架。
//
// 它们形状完全一样（一条 CAS UPDATE + 读回带 SKU 的详情），
// 只有 CAS 语句与提示文案不同。抽出来是为了让"回读"这一步
// 只存在一份 —— 之前入驻审核的 CAS 分散在两处，
// 那种重复的代价是"有一处忘了读回"，而响应里就会缺字段。
//
// # 为什么 CAS 未命中要先查一次再决定回 404 还是 409
//
// CAS 语句的条件里同时有 id、merchant_id 与旧状态，所以它未命中有**两种**成因：
//   - 商品不存在，或不属于本商户 → 跨租户 / 不存在
//   - 商品是自己的，但状态不对（如重复提交、对着在售商品点上架）
//
// 直接一律回 409 会泄露信息：B 商户拿一个随机 UUID 来试，
// 若商品存在（属于别人）得到 409、不存在得到 404 ——
// 于是 409/404 的差别成了一条"这条商品存在"的侧信道。
// 跨租户访问必须与"不存在"同形，这是全项目统一语义。
//
// 而一律回 404 则让商户看不到有用的信息："你点提交，系统说找不到" ——
// 他会先去检查 URL，而不是意识到"你已经提交过了"。
//
// 所以多一次主键 + merchant_id 查询，把两种成因分开。
// 这次查询不在热路径上（状态流转是低频操作），代价可以接受。
func (s *Service) merchantTransition(
	ctx context.Context, merchantID, productID string,
	cas func() (repo.Product, error),
	hint string,
) (*ProductView, error) {
	if _, err := cas(); err != nil {
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("状态流转失败: %w", err)
		}
		// CAS 未命中：先分清"不是我的 / 不存在"与"状态不对"。
		if _, ownErr := s.Q.GetMerchantProduct(ctx, repo.GetMerchantProductParams{
			ID: productID, MerchantID: merchantID,
		}); ownErr != nil {
			if errors.Is(ownErr, pgx.ErrNoRows) {
				return nil, domain.ErrNotFound
			}
			return nil, fmt.Errorf("查询商品失败: %w", ownErr)
		}
		return nil, domain.UserFacingf(hint, domain.ErrInvalidTransition)
	}
	return s.GetMerchantProduct(ctx, merchantID, productID)
}

// ---- 平台侧 ----

// ListProductsForReview 平台的商品审核队列。
func (s *Service) ListProductsForReview(
	ctx context.Context, status string, page, pageSize int32,
) ([]ReviewProductView, int64, error) {
	if status != "" && !IsProductStatus(status) {
		return nil, 0, domain.UserFacingf(
			fmt.Sprintf("未知的商品状态 %q", status), domain.ErrValidation)
	}
	lim, off := normalizePage(page, pageSize)
	statusArg := normalizeSearch(status)

	rows, err := s.Q.ListProductsForReview(ctx, repo.ListProductsForReviewParams{
		Status: statusArg, Lim: lim, Off: off,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("查询商品列表失败: %w", err)
	}
	total, err := s.Q.CountProductsForReview(ctx, statusArg)
	if err != nil {
		return nil, 0, fmt.Errorf("统计商品数失败: %w", err)
	}
	if total == 0 {
		return nil, 0, nil
	}

	skus, err := s.skusByProductID(ctx, reviewProductIDs(rows))
	if err != nil {
		return nil, 0, err
	}

	views := make([]ReviewProductView, 0, len(rows))
	for _, row := range rows {
		v := ReviewProductView{
			ProductView: productViewFromReviewRow(row),
			ShopName:    row.ShopName,
		}
		v.Skus = skus[row.ID]
		views = append(views, v)
	}
	return views, total, nil
}

// reviewProductIDs 从审核队列的行里抽出商品 id。
// 与商户列表那处抽 id 的代码形状相同但类型不同（行类型不同，不能共用一个泛型），
// 所以各写一份 —— 为此引入泛型或接口的收益不足以抵消读懂成本。
func reviewProductIDs(rows []repo.ListProductsForReviewRow) []string {
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// GetProductForReview 平台读任意商品详情。
func (s *Service) GetProductForReview(ctx context.Context, productID string) (*ReviewProductView, error) {
	p, err := s.Q.GetProductForReview(ctx, productID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("查询商品失败: %w", err)
	}
	skus, err := s.Q.ListSkusByProduct(ctx, p.ID)
	if err != nil {
		return nil, fmt.Errorf("查询 SKU 失败: %w", err)
	}
	view := &ReviewProductView{ProductView: *productViewFromProduct(p, skus)}
	// 店铺名读不到不致命：审核照常进行，只是少了对照信息。
	// 刻意**不**因此返回 500 —— 那会让一个"顺带查不到"的字段
	// 卡住整个审核动作。
	if name, err := s.Q.GetMerchantShopName(ctx, p.MerchantID); err == nil {
		view.ShopName = name
	}
	return view, nil
}

// ReviewProduct 平台裁决一条商品审核。
//
// 三种裁决：
//   - ON_SALE 通过（仅 PENDING_REVIEW）
//   - REJECTED 驳回（仅 PENDING_REVIEW，理由必填）
//   - OFF_SALE 强制下架（ON_SALE / OFF_SALE 都可）
//
// CAS 语义与商户入驻审核一致：影响行数 0 一律 404。
func (s *Service) ReviewProduct(
	ctx context.Context, adminID, productID, decision, rejectReason string,
) (*ReviewProductView, error) {
	switch decision {
	case ProductOnSale, ProductRejected, ProductOffSale:
	default:
		return nil, domain.UserFacingf(
			fmt.Sprintf("审核结论只能是 %s、%s 或 %s（强制下架）",
				ProductOnSale, ProductRejected, ProductOffSale),
			domain.ErrValidation)
	}

	rejectReason = strings.TrimSpace(rejectReason)
	if decision == ProductRejected {
		// 理由必填不是为了走流程：没有理由的驳回，商户只能靠猜，
		// 然后反复盲提交，平台的审核量反而被推高。
		if rejectReason == "" {
			return nil, domain.UserFacingf("驳回必须给出理由", domain.ErrValidation)
		}
	}
	if decision != ProductRejected {
		// 通过或强制下架时清空理由，避免上一次驳回的文案
		// 留在记录上误导商户（"已通过 + 上次的驳回理由"会被读成没通过）。
		rejectReason = ""
	}
	if len([]rune(rejectReason)) > maxRejectReasonLen {
		return nil, domain.UserFacingf(
			fmt.Sprintf("驳回理由不能超过 %d 字", maxRejectReasonLen), domain.ErrValidation)
	}

	if _, err := s.Q.ReviewProduct(ctx, repo.ReviewProductParams{
		ID:           productID,
		Status:       decision,
		RejectReason: rejectReason,
		ReviewedBy:   adminID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 不存在，或不在可裁决的状态。统一 404 ——
			// 区分这两者对调用方没有价值，却会泄露"这条审核存在"。
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("审核商品失败: %w", err)
	}
	return s.GetProductForReview(ctx, productID)
}

// ---- 校验 ----

// validateCoverURL 校验商品封面地址。
//
// 为什么不能只校验长度：封面地址是一个**会进入前端渲染路径**的自由文本。
// 它最终会出现在 App/Web 的 <Image source> 或 <img src> 里（将来也可能有人
// 顺手把它塞进 <a href> 做"点击看大图"）。而 URL 的 scheme 决定了浏览器
// 拿它当什么处理：
//
//   - javascript:alert(1)   → 在 href 位置上就是标准 XSS
//   - data:text/html,<script>…  → 内联文档，取决于渲染位置
//   - file:///C:/…          → 本地文件读取面
//   - //evil.com/x.png      → 协议相对 URL，会指向外部域名
//
// 只做一个黑名单（"禁止 javascript:"）是不够的：scheme 的大小写不敏感
// （JaVaScRiPt:）、可以带前导空白与控制字符（"\tjavascript:"），
// 黑名单永远追不上变体。**白名单只有两个值**，穷举得完，也就关得死。
//
// 顺带把长度校验收进来：它和格式校验是同一件事的两面
// （"这个字段能不能进渲染路径"），分开写在两处迟早会漏掉一处。
func validateCoverURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		// 封面是可选的。空字符串不进入渲染路径，放行。
		return nil
	}
	if len(raw) > maxProductCoverURLLen {
		return domain.UserFacingf(
			fmt.Sprintf("封面地址过长（上限 %d 字节）", maxProductCoverURLLen), domain.ErrValidation)
	}

	u, err := url.Parse(raw)
	if err != nil {
		return domain.UserFacingf("封面地址格式不正确", domain.ErrValidation)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		// 文案里点出期望的形态，而不是只说"非法"：商户最长遇到的场景是
		// 从系统资源管理器复制了一个本地路径（C:\... 或 file:///...）粘进来，
		// 告诉他要"以 http(s):// 开头"比告诉他"地址非法"有用得多。
		return domain.UserFacingf("封面地址必须以 http:// 或 https:// 开头", domain.ErrValidation)
	}
	if u.Host == "" {
		// "http:///x.png" 这种能通过 scheme 校验但没有主机名，
		// 落到前端就是一个永远加载失败的图。在这里挡住，比让商户在真机上
		// 看到一个白框再回来问要好。
		return domain.UserFacingf("封面地址缺少域名", domain.ErrValidation)
	}
	return nil
}

func validateProductInput(in ProductInput) error {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return domain.UserFacingf("商品标题不能为空", domain.ErrValidation)
	}
	// 长度按**字符数**而不是字节数：中文标题 60 个字就接近 180 字节，
	// 用 len() 会把"标题稍微长了点"误判成超长。
	if len([]rune(title)) > maxProductTitleLen {
		return domain.UserFacingf(
			fmt.Sprintf("商品标题不能超过 %d 字", maxProductTitleLen), domain.ErrValidation)
	}
	if len([]rune(in.Description)) > maxProductDescLen {
		return domain.UserFacingf(
			fmt.Sprintf("商品描述不能超过 %d 字", maxProductDescLen), domain.ErrValidation)
	}
	if len([]rune(in.Category)) > maxProductCategoryLen {
		return domain.UserFacingf(
			fmt.Sprintf("类目不能超过 %d 字", maxProductCategoryLen), domain.ErrValidation)
	}
	if err := validateCoverURL(in.CoverURL); err != nil {
		return err
	}

	if len(in.Skus) == 0 {
		return domain.UserFacingf("至少要有一个 SKU", domain.ErrValidation)
	}
	if len(in.Skus) > maxSkusPerProduct {
		return domain.UserFacingf(
			fmt.Sprintf("SKU 数量不能超过 %d 个", maxSkusPerProduct), domain.ErrValidation)
	}
	for i, sku := range in.Skus {
		if strings.TrimSpace(sku.Name) == "" {
			return domain.UserFacingf(fmt.Sprintf("第 %d 个 SKU 的规格名不能为空", i+1),
				domain.ErrValidation)
		}
		if len([]rune(sku.Name)) > maxSkuNameLen {
			return domain.UserFacingf(
				fmt.Sprintf("第 %d 个 SKU 的规格名不能超过 %d 字", i+1, maxSkuNameLen),
				domain.ErrValidation)
		}
		// 负价格与负库存都有 CHECK 约束兜底，但那些错误会带着表名与列名
		// 一路走到 WriteProblem。这里提前挡住，给出用户看得懂的提示。
		if sku.PriceCents < 0 {
			return domain.UserFacingf(fmt.Sprintf("第 %d 个 SKU 的价格不能为负", i+1),
				domain.ErrValidation)
		}
		if sku.Stock < 0 {
			return domain.UserFacingf(fmt.Sprintf("第 %d 个 SKU 的库存不能为负", i+1),
				domain.ErrValidation)
		}
	}
	return nil
}

// ---- 行 → 视图 ----

func productViewFromProduct(p repo.Product, skus []repo.Sku) *ProductView {
	minPrice := int64(0)
	var totalStock int64
	for i, s := range skus {
		totalStock += int64(s.Stock)
		if i == 0 || s.PriceCents < minPrice {
			minPrice = s.PriceCents
		}
	}
	if skus == nil {
		skus = []repo.Sku{}
	}
	return &ProductView{
		ID:            p.ID,
		MerchantID:    p.MerchantID,
		Title:         p.Title,
		Description:   p.Description,
		Category:      p.Category,
		CoverURL:      p.CoverUrl,
		Status:        p.Status,
		RejectReason:  p.RejectReason,
		MinPriceCents: minPrice,
		TotalStock:    totalStock,
		SubmittedAt:   pgTimePtr(p.SubmittedAt),
		ReviewedAt:    pgTimePtr(p.ReviewedAt),
		CreatedAt:     pgTime(p.CreatedAt),
		UpdatedAt:     pgTime(p.UpdatedAt),
		Skus:          skus,
	}
}

func productViewFromListRow(r repo.ListMerchantProductsRow) ProductView {
	return ProductView{
		ID:            r.ID,
		MerchantID:    r.MerchantID,
		Title:         r.Title,
		Description:   r.Description,
		Category:      r.Category,
		CoverURL:      r.CoverUrl,
		Status:        r.Status,
		RejectReason:  r.RejectReason,
		MinPriceCents: r.MinPriceCents,
		TotalStock:    r.TotalStock,
		SubmittedAt:   pgTimePtr(r.SubmittedAt),
		ReviewedAt:    pgTimePtr(r.ReviewedAt),
		CreatedAt:     pgTime(r.CreatedAt),
		UpdatedAt:     pgTime(r.UpdatedAt),
	}
}

func productViewFromReviewRow(r repo.ListProductsForReviewRow) ProductView {
	return ProductView{
		ID:            r.ID,
		MerchantID:    r.MerchantID,
		Title:         r.Title,
		Description:   r.Description,
		Category:      r.Category,
		CoverURL:      r.CoverUrl,
		Status:        r.Status,
		RejectReason:  r.RejectReason,
		MinPriceCents: r.MinPriceCents,
		TotalStock:    r.TotalStock,
		SubmittedAt:   pgTimePtr(r.SubmittedAt),
		ReviewedAt:    pgTimePtr(r.ReviewedAt),
		CreatedAt:     pgTime(r.CreatedAt),
		UpdatedAt:     pgTime(r.UpdatedAt),
	}
}
