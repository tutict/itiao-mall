package service_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// seedApprovedMerchant 建一个**已通过入驻审核**的商户。
//
// 为什么必须走完整流程而不是直接插一行 APPROVED：merchantStatusGate
// 与商品接口的可用性都挂在这个状态上，绕过流程造出来的数据
// 可能与真实流程造出来的不一致（比如漏了 merchant_profiles 之外的字段），
// 于是测试绿了、线上却 403。
func seedApprovedMerchant(t *testing.T, svc *service.Service, pool *pgxpool.Pool) string {
	t.Helper()
	phone := uniquePhone()
	registerCleanup(t, pool, phone)

	ctx := context.Background()
	sess, err := svc.RegisterMerchant(ctx, service.MerchantRegisterInput{
		Phone: phone, Password: "merchant-secret-1", ShopName: "商品测试店",
	})
	if err != nil {
		t.Fatalf("注册商户失败: %v", err)
	}
	adminID, _ := seedAdminUser(t, pool)
	if _, err := svc.ReviewMerchant(ctx, adminID, sess.User.ID,
		service.MerchantApproved, ""); err != nil {
		t.Fatalf("通过入驻审核失败: %v", err)
	}
	// 商品的外键是 products.merchant_id → users(id)，**没有** ON DELETE CASCADE
	// （刻意如此：删商户时若他的商品被静默删掉，用户手里的购物车会指向不存在的 SKU）。
	// 于是清理必须先删商品 —— 否则 registerCleanup 的 DELETE users 会被外键挡住，
	// 而那条清理是所有商户用例共用的，它失败会连带污染后续用例的计数断言。
	merchantID := sess.User.ID
	t.Cleanup(func() {
		c, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := pool.Exec(c, `DELETE FROM products WHERE merchant_id = $1`, merchantID); err != nil {
			t.Logf("清理商户 %s 的商品失败: %v", merchantID, err)
		}
	})
	return merchantID
}

func sampleProduct(title string) service.ProductInput {
	return service.ProductInput{
		Title:       title,
		Description: "测试商品描述",
		Category:    "数码",
		CoverURL:    "https://example.com/cover.png",
		Skus: []service.SkuInput{
			{Name: "标准版", PriceCents: 9900, Stock: 100},
			{Name: "豪华版", PriceCents: 19900, Stock: 50},
		},
	}
}

// 把商品推进到指定状态，供状态流转相关的用例复用。
// 路径是唯一的合法路径：提交 → 审核通过 → （必要时）下架。
func driveProductTo(t *testing.T, svc *service.Service, merchantID, adminID, productID, target string) {
	t.Helper()
	ctx := context.Background()

	if target == service.ProductDraft || target == service.ProductRejected {
		return // 草稿与被驳回都是新建后的初始可达状态
	}
	if _, err := svc.SubmitProduct(ctx, merchantID, productID); err != nil {
		t.Fatalf("提交审核失败: %v", err)
	}
	if target == service.ProductPendingReview {
		return
	}
	if _, err := svc.ReviewProduct(ctx, adminID, productID, service.ProductOnSale, ""); err != nil {
		t.Fatalf("审核通过失败: %v", err)
	}
	if target == service.ProductOnSale {
		return
	}
	if _, err := svc.OffShelfProduct(ctx, merchantID, productID); err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	if target == service.ProductOffSale {
		return
	}
	t.Fatalf("未知目标状态 %q", target)
}

// ---------- 创建 ----------

func TestCreateProduct_StartsAsDraft(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("测试商品"))
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	// 新建必须落在 DRAFT。这条断言守的是"商户不能自己上架"这条产品需求。
	if view.Status != service.ProductDraft {
		t.Fatalf("新建商品状态应为 %s，实际 %s", service.ProductDraft, view.Status)
	}
	if view.MerchantID != merchantID {
		t.Errorf("merchant_id = %q，期望 %q", view.MerchantID, merchantID)
	}
	if len(view.Skus) != 2 {
		t.Fatalf("应有 2 个 SKU，实际 %d", len(view.Skus))
	}
	// 最低价取所有 SKU 的最小值，与用户端列表口径一致。
	if view.MinPriceCents != 9900 {
		t.Errorf("最低价应为 9900，实际 %d", view.MinPriceCents)
	}
	if view.TotalStock != 150 {
		t.Errorf("总库存应为 150，实际 %d", view.TotalStock)
	}
	if view.RejectReason != "" {
		t.Errorf("新商品不该带驳回理由，实际 %q", view.RejectReason)
	}
	// 用户端列表不该看到它
	rows, _, err := svc.ListProducts(ctx, "", "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == view.ID {
			t.Fatal("草稿商品出现在了用户端列表里")
		}
	}
}

// 校验必须挡在写库之前，且失败不留任何痕迹。
func TestCreateProduct_Validation(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	cases := []struct {
		name string
		in   service.ProductInput
	}{
		{"标题为空", service.ProductInput{Title: "  ", Skus: []service.SkuInput{{Name: "标准", PriceCents: 1, Stock: 1}}}},
		{"标题超 100 字", service.ProductInput{Title: strings.Repeat("品", 101),
			Skus: []service.SkuInput{{Name: "标准", PriceCents: 1, Stock: 1}}}},
		{"描述超 2000 字", service.ProductInput{Title: "ok", Description: strings.Repeat("述", 2001),
			Skus: []service.SkuInput{{Name: "标准", PriceCents: 1, Stock: 1}}}},
		{"没有 SKU", service.ProductInput{Title: "ok"}},
		{"SKU 名为空", service.ProductInput{Title: "ok",
			Skus: []service.SkuInput{{Name: "  ", PriceCents: 1, Stock: 1}}}},
		{"价格为负", service.ProductInput{Title: "ok",
			Skus: []service.SkuInput{{Name: "标准", PriceCents: -1, Stock: 1}}}},
		{"库存为负", service.ProductInput{Title: "ok",
			Skus: []service.SkuInput{{Name: "标准", PriceCents: 1, Stock: -1}}}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.CreateProduct(ctx, merchantID, tc.in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("应返回 ErrValidation，实际: %v", err)
			}
		})
	}

	// 断言只数**本商户**的商品。
	// 写成 `SELECT count(*) FROM products` 的话，并行跑或前序用例留下的
	// 数据会让这个断言变成"整张表有几行"—— 它的失败与被测行为完全无关。
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM products WHERE merchant_id = $1`, merchantID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("校验失败却写库了：%d 个商品", n)
	}
}

// ★ 跨租户隔离：商户 B 绝不能看到也不能改商户 A 的商品。
//
// 这是本模块最承重的一条。每一条商户侧 SQL 都带 merchant_id 条件，
// 少一个就是全平台商品列表，而 200 响应不会有任何异常特征。
func TestMerchantProduct_CrossTenantIsolation(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantA := seedApprovedMerchant(t, svc, pool)
	merchantB := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantA, sampleProduct("A店的商品"))
	if err != nil {
		t.Fatalf("创建失败: %v", err)
	}

	// B 读不到
	if _, err := svc.GetMerchantProduct(ctx, merchantB, view.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("B 读 A 的商品应得 404，实际: %v", err)
	}
	// B 的列表里没有
	views, total, err := svc.ListMerchantProducts(ctx, merchantB, "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(views) != 0 {
		t.Fatalf("B 的列表应为空，实际 %d 条", total)
	}
	// B 改不了
	if _, err := svc.UpdateProduct(ctx, merchantB, view.ID, sampleProduct("篡改")); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("B 改 A 的商品应得 404，实际: %v", err)
	}
	// B 删不了
	if err := svc.DeleteProduct(ctx, merchantB, view.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("B 删 A 的商品应得 404，实际: %v", err)
	}
	// B 不能提交审核 / 上下架
	for name, fn := range map[string]func(context.Context, string, string) (*service.ProductView, error){
		"提交审核": svc.SubmitProduct,
		"撤回":   svc.WithdrawProduct,
		"下架":   svc.OffShelfProduct,
		"上架":   svc.OnShelfProduct,
	} {
		if _, err := fn(ctx, merchantB, view.ID); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("B 对 A 的商品执行「%s」应得 404，实际: %v", name, err)
		}
	}
	// A 的商品确实没被改动
	after, err := svc.GetMerchantProduct(ctx, merchantA, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "A店的商品" || after.Status != service.ProductDraft {
		t.Fatalf("A 的商品被 B 改了：title=%q status=%s", after.Title, after.Status)
	}
	_ = adminID
}

// ---------- 状态流转 ----------

// ★ 完整生命周期：DRAFT → PENDING_REVIEW → ON_SALE → OFF_SALE → ON_SALE。
//
// 每一步都断言**用户端是否可见**，而不只是断言状态字符串：
// 状态对了但列表查不出来，是最难查的一类不一致。
func TestProduct_FullLifecycle(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("生命周期商品"))
	if err != nil {
		t.Fatal(err)
	}
	id := view.ID

	visibleOnSale := func(want bool, when string) {
		t.Helper()
		rows, _, err := svc.ListProducts(ctx, "", "", 1, 100)
		if err != nil {
			t.Fatal(err)
		}
		got := false
		for _, r := range rows {
			if r.ID == id {
				got = true
			}
		}
		if got != want {
			t.Fatalf("%s：用户端可见性应为 %v，实际 %v", when, want, got)
		}
	}

	visibleOnSale(false, "草稿时")

	// 提交审核
	v, err := svc.SubmitProduct(ctx, merchantID, id)
	if err != nil {
		t.Fatalf("提交审核失败: %v", err)
	}
	if v.Status != service.ProductPendingReview {
		t.Fatalf("提交后状态应为 PENDING_REVIEW，实际 %s", v.Status)
	}
	if v.SubmittedAt == nil {
		t.Error("提交后应有 submitted_at —— 审核队列按它排序")
	}
	visibleOnSale(false, "待审时")

	// 审核通过（平台视角返回的是 ReviewProductView，所以单独一个变量）
	rv, err := svc.ReviewProduct(ctx, adminID, id, service.ProductOnSale, "")
	if err != nil {
		t.Fatalf("审核通过失败: %v", err)
	}
	if rv.Status != service.ProductOnSale {
		t.Fatalf("通过后状态应为 ON_SALE，实际 %s", rv.Status)
	}
	if rv.ReviewedAt == nil {
		t.Error("审核后应有 reviewed_at")
	}
	if rv.RejectReason != "" {
		t.Errorf("通过后不该留驳回理由，实际 %q", rv.RejectReason)
	}
	visibleOnSale(true, "在售时")

	// 下架
	if _, err := svc.OffShelfProduct(ctx, merchantID, id); err != nil {
		t.Fatalf("下架失败: %v", err)
	}
	visibleOnSale(false, "下架后")

	// 重新上架：不需要重新审核
	v, err = svc.OnShelfProduct(ctx, merchantID, id)
	if err != nil {
		t.Fatalf("重新上架失败: %v", err)
	}
	if v.Status != service.ProductOnSale {
		t.Fatalf("上架后状态应为 ON_SALE，实际 %s", v.Status)
	}
	visibleOnSale(true, "重新上架后")
}

// 非法流转必须被状态机挡住。
//
// 这组用例的价值在于**穷举**：状态机一共 5 个状态、若干条边，
// 手写"应该拦住"的清单永远不完整，而表驱动可以把 25 个组合全列出来。
func TestProductStateMachine_Exhaustive(t *testing.T) {
	all := []domain.ProductStatus{
		domain.ProductDraft, domain.ProductPendingReview,
		domain.ProductOnSale, domain.ProductOffSale, domain.ProductRejected,
	}
	// 期望能流转的边。与 domain/product.go 的表一一对应。
	want := map[[2]domain.ProductStatus]bool{
		{domain.ProductDraft, domain.ProductPendingReview}:    true,
		{domain.ProductDraft, domain.ProductOffSale}:          true,
		{domain.ProductPendingReview, domain.ProductOnSale}:   true,
		{domain.ProductPendingReview, domain.ProductRejected}: true,
		{domain.ProductPendingReview, domain.ProductDraft}:    true,
		{domain.ProductOnSale, domain.ProductOffSale}:         true,
		{domain.ProductOnSale, domain.ProductDraft}:           true,
		{domain.ProductOffSale, domain.ProductOnSale}:         true,
		{domain.ProductOffSale, domain.ProductDraft}:          true,
		{domain.ProductRejected, domain.ProductPendingReview}: true,
		{domain.ProductRejected, domain.ProductDraft}:         true,
	}

	for _, from := range all {
		for _, to := range all {
			err := func() error { _, e := from.TransitionTo(to); return e }()
			expectOK := want[[2]domain.ProductStatus{from, to}]
			if expectOK && err != nil {
				t.Errorf("%s → %s 应允许，实际被拒: %v", from, to, err)
			}
			if !expectOK && err == nil {
				t.Errorf("%s → %s 应被拒，实际允许 —— 状态机多了一条边", from, to)
			}
			if !expectOK && err != nil && !errors.Is(err, domain.ErrInvalidTransition) {
				t.Errorf("%s → %s 的错误应为 ErrInvalidTransition，实际 %v", from, to, err)
			}
		}
	}
}

// ★ DRAFT 不能直接到 ON_SALE。
//
// 单独拎出来断言是因为它是本模块最重要的那条产品约束：
// 商户不能自己把商品变成在售。加一条捷径就等于把平台审核架空。
func TestProductStateMachine_NoDraftToOnSale(t *testing.T) {
	if domain.ProductDraft.CanTransitionTo(domain.ProductOnSale) {
		t.Fatal("DRAFT → ON_SALE 必须被禁止：上架必须经平台审核")
	}
}

// 只有草稿与被驳回的可编辑。
func TestProductStateMachine_EditableStates(t *testing.T) {
	cases := map[domain.ProductStatus]bool{
		domain.ProductDraft:         true,
		domain.ProductRejected:      true,
		domain.ProductPendingReview: false,
		domain.ProductOnSale:        false,
		domain.ProductOffSale:       false,
	}
	for st, want := range cases {
		if got := st.IsMerchantEditable(); got != want {
			t.Errorf("%s 的可编辑性应为 %v，实际 %v", st, want, got)
		}
	}
}

// 在售商品不能编辑 —— 商户要改必须先下架。
func TestUpdateProduct_RejectsNonEditableStates(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("在售商品"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductOnSale)

	_, err = svc.UpdateProduct(ctx, merchantID, view.ID, sampleProduct("偷偷改价"))
	if !errors.Is(err, domain.ErrInvalidTransition) {
		t.Fatalf("编辑在售商品应被拒（ErrInvalidTransition），实际: %v", err)
	}

	// 拒绝之后内容没变
	after, err := svc.GetMerchantProduct(ctx, merchantID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Title != "在售商品" {
		t.Fatalf("被拒的编辑不该生效，标题实际为 %q", after.Title)
	}
	if after.Status != service.ProductOnSale {
		t.Fatalf("被拒的编辑不该改状态，实际 %s", after.Status)
	}
}

// 被驳回后编辑 = 重新提交，且驳回理由被清空。
func TestUpdateProduct_RejectedResubmits(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("会被驳回"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductPendingReview)

	if _, err := svc.ReviewProduct(ctx, adminID, view.ID,
		service.ProductRejected, "图片与实物不符"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}

	v, err := svc.GetMerchantProduct(ctx, merchantID, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != service.ProductRejected {
		t.Fatalf("驳回后状态应为 REJECTED，实际 %s", v.Status)
	}
	if v.RejectReason != "图片与实物不符" {
		t.Fatalf("驳回理由应为审核员给的那句，实际 %q", v.RejectReason)
	}

	// 商户改完自动重新提交
	v, err = svc.UpdateProduct(ctx, merchantID, view.ID, sampleProduct("改了标题"))
	if err != nil {
		t.Fatalf("编辑被驳回的商品失败: %v", err)
	}
	if v.Status != service.ProductPendingReview {
		t.Fatalf("编辑被驳回的商品应自动重新提交（→ PENDING_REVIEW），实际 %s", v.Status)
	}
	if v.RejectReason != "" {
		t.Errorf("重新提交后驳回理由应被清空，实际 %q", v.RejectReason)
	}
}

// SKU 整体替换：编辑后是新的那批，不是追加。
func TestUpdateProduct_ReplacesSkus(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("多规格商品"))
	if err != nil {
		t.Fatal(err)
	}
	oldSkuIDs := map[string]bool{}
	for _, s := range view.Skus {
		oldSkuIDs[s.ID] = true
	}

	in := sampleProduct("多规格商品")
	in.Skus = []service.SkuInput{{Name: "唯一规格", PriceCents: 100, Stock: 1}}
	updated, err := svc.UpdateProduct(ctx, merchantID, view.ID, in)
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}

	if len(updated.Skus) != 1 {
		t.Fatalf("SKU 应被整体替换为 1 个，实际 %d", len(updated.Skus))
	}
	if oldSkuIDs[updated.Skus[0].ID] {
		t.Error("SKU 应是新建的而不是复用旧的 —— 那说明是先删后插没生效")
	}
	// 旧 SKU 应当真的从库里消失了（不是只从响应里消失）
	for id := range oldSkuIDs {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM skus WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("旧 SKU %s 仍留在库里 —— 整体替换变成了追加", id)
		}
	}
}

// ---------- 审核 ----------

// ★ 审核 CAS：两个管理员并发裁决同一条待审商品，只能有一个生效。
//
// 与商户入驻审核是同一个道理。不做 CAS 的话最终状态取决于谁的网络快，
// 而日志里两条都写着"成功" —— 事后完全无法判断发生了什么。
func TestReviewProduct_ConcurrentOnlyOneWins(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminA, _ := seedAdminUser(t, pool)
	adminB, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("并发审核"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminA, view.ID, service.ProductPendingReview)

	const n = 10
	var success, notFound atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			adminID, decision := adminA, service.ProductOnSale
			if i%2 == 1 {
				adminID, decision = adminB, service.ProductRejected
			}
			_, err := svc.ReviewProduct(ctx, adminID, view.ID, decision, "并发驳回理由")
			switch {
			case err == nil:
				success.Add(1)
			case errors.Is(err, domain.ErrNotFound):
				notFound.Add(1)
			default:
				t.Errorf("并发审核出现意外错误: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := success.Load(); got != 1 {
		t.Fatalf("并发审核成功了 %d 次，期望恰好 1 次。404 的有 %d 次", got, notFound.Load())
	}
	// 最终状态必须是两者之一，且不是中间态
	final, err := svc.GetProductForReview(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.Status != service.ProductOnSale && final.Status != service.ProductRejected {
		t.Fatalf("最终状态应是 ON_SALE 或 REJECTED，实际 %s", final.Status)
	}
}

// 驳回必须给理由；通过时理由被清空。
func TestReviewProduct_RejectRequiresReason(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("驳回理由"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductPendingReview)

	if _, err := svc.ReviewProduct(ctx, adminID, view.ID, service.ProductRejected, "  "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("空理由驳回应被拒，实际: %v", err)
	}
	// 被拒的审核不该改变状态
	cur, err := svc.GetProductForReview(ctx, view.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.Status != service.ProductPendingReview {
		t.Fatalf("被拒的审核不该改状态，实际 %s", cur.Status)
	}

	if _, err := svc.ReviewProduct(ctx, adminID, view.ID,
		service.ProductRejected, "   "); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("空白理由驳回应被拒，实际: %v", err)
	}

	// 先驳回（带理由），再重新提交 + 通过，理由应被清空
	if _, err := svc.ReviewProduct(ctx, adminID, view.ID,
		service.ProductRejected, "标题夸大"); err != nil {
		t.Fatalf("驳回失败: %v", err)
	}
	if _, err := svc.UpdateProduct(ctx, merchantID, view.ID, sampleProduct("改好了")); err != nil {
		t.Fatal(err)
	}
	v, err := svc.ReviewProduct(ctx, adminID, view.ID, service.ProductOnSale, "")
	if err != nil {
		t.Fatal(err)
	}
	if v.RejectReason != "" {
		t.Fatalf("通过后应清空上一轮的驳回理由，实际 %q —— 商户会以为没通过", v.RejectReason)
	}
}

// 强制下架：在售与已下架都能被强制下掉。
//
// 这是平台的事后处置（违规、投诉）。它存在的意义是立刻停止销售，
// 所以必须能作用于在售商品 —— 而通过/驳回只作用于待审商品。
func TestReviewProduct_ForceOffShelf(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("违规商品"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductOnSale)

	v, err := svc.ReviewProduct(ctx, adminID, view.ID, service.ProductOffSale, "")
	if err != nil {
		t.Fatalf("强制下架失败: %v", err)
	}
	if v.Status != service.ProductOffSale {
		t.Fatalf("强制下架后状态应为 OFF_SALE，实际 %s", v.Status)
	}

	// 用户端不该再看到它
	rows, _, err := svc.ListProducts(ctx, "", "", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == view.ID {
			t.Fatal("被强制下架的商品仍在用户端列表里")
		}
	}

	// 再强制下架一次应该是幂等的（OFF_SALE 也能被强制下架）
	if _, err := svc.ReviewProduct(ctx, adminID, view.ID, service.ProductOffSale, ""); err != nil {
		t.Fatalf("重复强制下架应成功（幂等），实际: %v", err)
	}
}

// 非法裁决结论必须被拒。
func TestReviewProduct_RejectsInvalidDecision(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("非法裁决"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductPendingReview)

	for _, bad := range []string{"APPROVED", "approved", "ON_SALE ", "", "PENDING_REVIEW", "DRAFT"} {
		if _, err := svc.ReviewProduct(ctx, adminID, view.ID, bad, "理由"); !errors.Is(err, domain.ErrValidation) {
			t.Errorf("裁决结论 %q 应被拒，实际: %v", bad, err)
		}
	}
}

// ---------- 删除 ----------

// 在售商品删不掉；从架上消失用下架表达。
func TestDeleteProduct_RejectsOnSale(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("在售不可删"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, view.ID, service.ProductOnSale)

	if err := svc.DeleteProduct(ctx, merchantID, view.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("删除在售商品应被拒（404），实际: %v", err)
	}
	// 确认还在
	if _, err := svc.GetMerchantProduct(ctx, merchantID, view.ID); err != nil {
		t.Fatalf("商品被误删了: %v", err)
	}
}

// 草稿可以删，且 SKU 一起清掉。
func TestDeleteProduct_RemovesSkus(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	view, err := svc.CreateProduct(ctx, merchantID, sampleProduct("待删商品"))
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteProduct(ctx, merchantID, view.ID); err != nil {
		t.Fatalf("删除草稿失败: %v", err)
	}

	var products, skus int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM products WHERE id = $1`, view.ID).Scan(&products); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM skus WHERE product_id = $1`, view.ID).Scan(&skus); err != nil {
		t.Fatal(err)
	}
	if products != 0 {
		t.Errorf("商品仍在库里（%d 行）", products)
	}
	if skus != 0 {
		t.Errorf("SKU 仍在库里（%d 行）—— 级联删除没生效", skus)
	}
}

// ---------- 列表与分页 ----------

// 列表按状态过滤，total 与过滤条件一致。
func TestListMerchantProducts_FilterByStatus(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)
	adminID, _ := seedAdminUser(t, pool)

	for i := 0; i < 3; i++ {
		if _, err := svc.CreateProduct(ctx, merchantID,
			sampleProduct("草稿商品"+string(rune('A'+i)))); err != nil {
			t.Fatal(err)
		}
	}
	onsale, err := svc.CreateProduct(ctx, merchantID, sampleProduct("在售商品"))
	if err != nil {
		t.Fatal(err)
	}
	driveProductTo(t, svc, merchantID, adminID, onsale.ID, service.ProductOnSale)

	all, total, err := svc.ListMerchantProducts(ctx, merchantID, "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 4 || len(all) != 4 {
		t.Fatalf("全部应 4 条，实际 total=%d len=%d", total, len(all))
	}
	// 每条都要带 SKU
	for _, v := range all {
		if len(v.Skus) != 2 {
			t.Errorf("商品 %s 的 SKU 数应为 2，实际 %d", v.Title, len(v.Skus))
		}
	}

	drafts, total, err := svc.ListMerchantProducts(ctx, merchantID, service.ProductDraft, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 || len(drafts) != 3 {
		t.Fatalf("草稿应 3 条，实际 total=%d len=%d", total, len(drafts))
	}
	for _, v := range drafts {
		if v.Status != service.ProductDraft {
			t.Errorf("状态过滤失效：返回了 %s", v.Status)
		}
	}

	onSale, total, err := svc.ListMerchantProducts(ctx, merchantID, service.ProductOnSale, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(onSale) != 1 || onSale[0].ID != onsale.ID {
		t.Fatalf("在售应恰好 1 条且是那条，实际 total=%d", total)
	}

	// 未知状态必须被拒，而不是静默返回空列表
	if _, _, err := svc.ListMerchantProducts(ctx, merchantID, "NOT_A_STATUS", 1, 20); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("未知状态应被拒，实际: %v", err)
	}
}

// 审核队列带店铺名，且按提交时间倒序。
func TestListProductsForReview(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	var submitted []string
	for i := 0; i < 3; i++ {
		v, err := svc.CreateProduct(ctx, merchantID, sampleProduct("待审商品"+string(rune('A'+i))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SubmitProduct(ctx, merchantID, v.ID); err != nil {
			t.Fatal(err)
		}
		submitted = append(submitted, v.ID)
	}

	views, total, err := svc.ListProductsForReview(ctx, service.ProductPendingReview, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	// 审核队列是**全平台**的，所以别的用例留下的待审商品也会出现在这里。
	// 断言"恰好 3 条"会把用例间的耦合变成随机的失败。
	// 改为：本用例提交的 3 条都在队列里，且相对顺序是倒序。
	if total < 3 {
		t.Fatalf("待审队列 total=%d，本用例提交了 3 条却少于 3", total)
	}
	pos := map[string]int{}
	submittedAt := map[string]time.Time{}
	for i, v := range views {
		pos[v.ID] = i
		if v.SubmittedAt != nil {
			submittedAt[v.ID] = *v.SubmittedAt
		}
	}
	for _, id := range submitted {
		if _, ok := pos[id]; !ok {
			t.Fatalf("提交的商品 %s 不在待审队列里", id)
		}
	}
	// 倒序断言只对**时间戳确实不同**的相邻两条做。
	//
	// 为什么不无条件断言：`now()` 的精度依赖平台，同一事务里连续提交
	// 可能拿到完全相同的 submitted_at（实测 Windows 上 3 次里有 2 次相同）。
	// 那时它们的相对顺序由 tie-break 决定，而 tie-break 的方向是任意的 ——
	// 对它断言"必须倒序"等于断言一个实现细节。
	//
	// 但"时间戳不同时必须严格倒序"是硬要求，且这条断言有牙齿：
	// 把 ORDER BY 改成 ASC 就会立刻失败。
	for i := 1; i < len(submitted); i++ {
		prev, cur := submitted[i-1], submitted[i]
		pt, pok := submittedAt[prev]
		ct, cok := submittedAt[cur]
		if !pok || !cok {
			continue
		}
		if ct.Equal(pt) {
			continue // 时间戳相同，顺序由 tie-break 决定，不断言
		}
		if !ct.After(pt) {
			t.Fatalf("提交时间未倒序：后提交的 %s(%v) 不晚于先提交的 %s(%v)",
				cur, ct, prev, pt)
		}
		if pos[cur] >= pos[prev] {
			t.Fatalf("提交时间 %v 晚于 %v，但在队列里的位置反而更靠后（%d vs %d）",
				ct, pt, pos[cur], pos[prev])
		}
	}
	for _, v := range views {
		if v.ShopName == "" {
			t.Errorf("审核队列必须带店铺名（%s 缺了）", v.Title)
		}
		if v.SubmittedAt == nil {
			t.Errorf("%s 缺 submitted_at", v.Title)
		}
		if len(v.Skus) != 2 {
			t.Errorf("%s 的 SKU 数应为 2，实际 %d", v.Title, len(v.Skus))
		}
	}

	// 不存在的商品
	if _, err := svc.GetProductForReview(ctx, uuid.NewString()); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("读不存在的商品应得 404，实际: %v", err)
	}
}

// 分页：超大页码与页大小都必须被夹住，不能打穿数据库。
func TestListMerchantProducts_PageClamping(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()
	merchantID := seedApprovedMerchant(t, svc, pool)

	if _, err := svc.CreateProduct(ctx, merchantID, sampleProduct("分页商品")); err != nil {
		t.Fatal(err)
	}
	// page=0 / page_size=0 / page_size=100000 / 极大 page 都不能报错
	for _, c := range [][2]int32{{0, 0}, {-1, -1}, {1, 100000}, {2147483647, 100}, {1, 2147483647}} {
		if _, _, err := svc.ListMerchantProducts(ctx, merchantID, "", c[0], c[1]); err != nil {
			t.Errorf("page=%d page_size=%d 应被夹住而不是报错: %v", c[0], c[1], err)
		}
	}
}
