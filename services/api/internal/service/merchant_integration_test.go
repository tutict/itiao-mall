package service_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/tutic/itiao-mall/services/api/internal/auth"
	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/payment"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
	"github.com/tutic/itiao-mall/services/api/internal/service"
)

// newMerchantTestService 与 newTestService 的区别只有一个：**真的连 Redis**。
//
// 商户注册这条路径要签发 refresh token，而 refresh token 存在 Redis 里。
// 用那个指向 127.0.0.1:1 的占位客户端会让所有注册用例在 issueSession 处炸掉 ——
// 而炸的位置离真正要验证的东西很远，排查起来很不值。
//
// Redis 侧默认走 miniredis（见 newRedisForTest）。原来"连不上就 Skip"会让
// 整个商户注册/审核套件在无 Redis 的机器上静默通过。
func newMerchantTestService(t *testing.T) (*service.Service, *pgxpool.Pool) {
	t.Helper()

	pool := newTestPool(t)
	rdb := newRedisForTest(t)

	svc := service.New(service.Deps{
		Q:            repo.New(pool),
		Pool:         pool,
		Redis:        rdb,
		Issuer:       auth.NewTokenIssuer("test-secret", time.Minute),
		RefreshStore: auth.NewRefreshStore(rdb, time.Hour),
		Wechat:       auth.MockExchanger{},
		Payment:      payment.NewMock(),
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		PendingTTL:   15 * time.Minute,
	})
	return svc, pool
}

// ---------- 注册 ----------

func TestRegisterMerchant_CreatesUserAndProfile(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	phone := uniquePhone()
	shopName := "原子性测试店 " + uuid.NewString()[:8]
	registerCleanup(t, pool, phone)

	sess, err := svc.RegisterMerchant(ctx, service.MerchantRegisterInput{
		Phone:       phone,
		Password:    "merchant-secret-1",
		ShopName:    shopName,
		ContactName: "张三",
	})
	if err != nil {
		t.Fatalf("注册失败: %v", err)
	}

	// 1) 会话里的身份是 merchant，且令牌不是空串
	if sess.User.Role != "merchant" {
		t.Errorf("注册返回的角色应为 merchant，实际 %q", sess.User.Role)
	}
	if len(sess.AccessToken) < 20 || len(sess.RefreshToken) < 20 {
		t.Errorf("注册应签发可用的 access/refresh token，实际长度 %d/%d",
			len(sess.AccessToken), len(sess.RefreshToken))
	}

	// 2) 入驻资料已建且是 PENDING —— **不能**直接给 APPROVED，
	//    否则自助注册就等于自助开通了经营权限。
	profile, err := svc.GetMerchantProfile(ctx, sess.User.ID)
	if err != nil {
		t.Fatalf("注册后应能读到入驻资料，实际: %v", err)
	}
	if profile.Status != service.MerchantPending {
		t.Errorf("自助注册后状态应为 %s，实际 %s（注册即通过等于自助开通经营权限）",
			service.MerchantPending, profile.Status)
	}
	if profile.ShopName != shopName {
		t.Errorf("店铺名 = %q，期望 %q", profile.ShopName, shopName)
	}
	if profile.RejectReason != "" {
		t.Errorf("新注册的申请不该带驳回理由，实际 %q", profile.RejectReason)
	}

	// 3) 密码必须是哈希，且能通过 bcrypt 校验。
	//    直接比对明文是没意义的 —— 要证明的是"存进去的东西能被 bcrypt 验回来"。
	var storedHash *string
	if err := pool.QueryRow(ctx,
		`SELECT password_hash FROM users WHERE phone = $1`, phone).Scan(&storedHash); err != nil {
		t.Fatalf("读取密码哈希失败: %v", err)
	}
	if storedHash == nil || *storedHash == "" {
		t.Fatal("password_hash 为空 —— 明文密码可能直接落库了")
	}
	if *storedHash == "merchant-secret-1" {
		t.Fatal("password_hash 与明文相同 —— 密码没被哈希")
	}

	// 4) 刚注册就能用自己的凭证登录（同一入口）
	if _, err := svc.LoginMerchant(ctx, phone, "merchant-secret-1"); err != nil {
		t.Fatalf("注册后应能用同一套凭证登录，实际: %v", err)
	}
}

func TestRegisterMerchant_DuplicatePhoneIsRejected(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	phone := uniquePhone()
	registerCleanup(t, pool, phone)

	in := service.MerchantRegisterInput{
		Phone: phone, Password: "merchant-secret-1", ShopName: "重复注册店",
	}
	if _, err := svc.RegisterMerchant(ctx, in); err != nil {
		t.Fatalf("首次注册失败: %v", err)
	}

	_, err := svc.RegisterMerchant(ctx, in)
	if !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("同手机号重复注册应返回 ErrConflict，实际: %v", err)
	}

	// 失败的那一次不能留下**任何**痕迹。用户表 1 行、资料表 1 行，多一行都说明
	// 「先建用户再建资料」的事务没兜住 —— 那会留下一个既进不了商户端、
	// 又因为手机号被占用而无法重新注册的死账号。
	var userCount, profileCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE phone = $1`, phone).Scan(&userCount); err != nil {
		t.Fatalf("统计用户失败: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM merchant_profiles mp
		JOIN users u ON u.id = mp.user_id WHERE u.phone = $1`, phone).Scan(&profileCount); err != nil {
		t.Fatalf("统计入驻资料失败: %v", err)
	}
	if userCount != 1 || profileCount != 1 {
		t.Fatalf("重复注册失败后应仍是 1 用户 + 1 资料，实际 %d + %d（有半成品残留）",
			userCount, profileCount)
	}
}

// 校验规则必须挡在写库之前。特别是 72 字节那条：
// bcrypt 生成时对超长密码报错、比对时却静默截断，两者不一致会造出
// 「注册被拒的密码，登录反而能过」这种最难解释的组合。
func TestRegisterMerchant_Validation(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	longPassword := strings.Repeat("a", 73) // 73 字节 > bcrypt 的 72

	cases := []struct {
		name string
		in   service.MerchantRegisterInput
	}{
		{"密码太短", service.MerchantRegisterInput{Phone: uniquePhone(), Password: "short", ShopName: "店"}},
		{"密码超 72 字节", service.MerchantRegisterInput{Phone: uniquePhone(), Password: longPassword, ShopName: "店"}},
		{"手机号空", service.MerchantRegisterInput{Phone: "", Password: "merchant-secret-1", ShopName: "店"}},
		{"手机号带字母", service.MerchantRegisterInput{Phone: "138abcd0000", Password: "merchant-secret-1", ShopName: "店"}},
		{"手机号太长", service.MerchantRegisterInput{Phone: strings.Repeat("1", 21), Password: "merchant-secret-1", ShopName: "店"}},
		{"店铺名为空白", service.MerchantRegisterInput{Phone: uniquePhone(), Password: "merchant-secret-1", ShopName: "   "}},
		{"店铺名超 100 字", service.MerchantRegisterInput{Phone: uniquePhone(), Password: "merchant-secret-1",
			ShopName: strings.Repeat("店", 101)}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := countUsers(t, pool)
			_, err := svc.RegisterMerchant(ctx, tc.in)
			if !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("应返回 ErrValidation，实际: %v", err)
			}
			if after := countUsers(t, pool); after != before {
				t.Fatalf("校验失败却写了库：用户数 %d -> %d", before, after)
			}
		})
	}
}

// ---------- 资料更新与状态机 ----------

// ★ 状态机的四种输入各走一遍。规则写在 SQL 的 CASE 里（为了原子性），
// 所以**只有真数据库能验它** —— 纯函数单测覆盖不到。
func TestUpsertMerchantProfile_StatusMachine(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	cases := []struct {
		from     string
		want     string
		wantWhy  string
		reasonIn string
	}{
		{service.MerchantRejected, service.MerchantPending, "改了资料就是一次重新提交", "资料不全"},
		{service.MerchantPending, service.MerchantPending, "仍在审核中，保持不动", ""},
		{service.MerchantApproved, service.MerchantApproved, "已通过后改联系方式不该触发重审", ""},
		{service.MerchantSuspended, service.MerchantSuspended, "一次编辑不能把平台停用洗白", "违规"},
	}

	for _, tc := range cases {
		t.Run(tc.from+"→"+tc.want, func(t *testing.T) {
			userID, phone := seedMerchantUser(t, pool)
			registerCleanup(t, pool, phone)
			setProfileStatus(t, pool, userID, tc.from, tc.reasonIn)

			got, err := svc.UpdateMerchantProfile(ctx, userID, service.MerchantProfileInput{
				ShopName:    "改过的店名",
				ContactName: "李四",
			})
			if err != nil {
				t.Fatalf("更新资料失败: %v", err)
			}
			if got.Status != tc.want {
				t.Errorf("%s 状态下更新资料应变为 %s，实际 %s（%s）",
					tc.from, tc.want, got.Status, tc.wantWhy)
			}
			if got.ShopName != "改过的店名" {
				t.Errorf("店铺名没更新，实际 %q", got.ShopName)
			}
			// 重新提交时必须清空驳回理由，否则"已重新提交"的记录上还挂着上次的驳回文案
			if tc.from == service.MerchantRejected && got.RejectReason != "" {
				t.Errorf("重新提交后应清空驳回理由，实际 %q", got.RejectReason)
			}
			if tc.from == service.MerchantSuspended && got.RejectReason != "违规" {
				t.Errorf("SUSPENDED 的理由不该被编辑清掉，实际 %q", got.RejectReason)
			}
		})
	}
}

// 没有资料的商户 PUT 资料应该把它建出来（upsert），而不是报 404。
// 种子户 / 迁移前就存在的商户会走到这条路径。
func TestUpdateMerchantProfile_CreatesWhenMissing(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	userID, phone := seedMerchantUser(t, pool)
	registerCleanup(t, pool, phone)

	// 先证明确实没有资料
	if _, err := svc.GetMerchantProfile(ctx, userID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("前置条件不成立：本应没有入驻资料，实际 err=%v", err)
	}

	got, err := svc.UpdateMerchantProfile(ctx, userID, service.MerchantProfileInput{ShopName: "补建的店"})
	if err != nil {
		t.Fatalf("缺失资料时 PUT 应建出来，实际: %v", err)
	}
	if got.Status != service.MerchantPending {
		t.Errorf("补建的资料应为 PENDING，实际 %s", got.Status)
	}
}

// ---------- 平台审核 ----------

// ★ CAS：一条申请只能被裁决一次。两个管理员并发点击时，
// 后一个必须拿到 404 而不是把前一个的结论覆盖掉。
func TestReviewMerchant_SecondReviewIsRejected(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	adminID, adminPhone := seedAdminUser(t, pool)
	registerCleanup(t, pool, adminPhone)
	userID, phone := seedMerchantUser(t, pool)
	registerCleanup(t, pool, phone)
	setProfileStatus(t, pool, userID, service.MerchantPending, "")

	first, err := svc.ReviewMerchant(ctx, adminID, userID, service.MerchantApproved, "")
	if err != nil {
		t.Fatalf("首次审核失败: %v", err)
	}
	if first.Status != service.MerchantApproved {
		t.Fatalf("首次审核应为 APPROVED，实际 %s", first.Status)
	}
	if first.ReviewedAt == nil {
		t.Error("审核后 reviewed_at 应有值")
	}

	// 第二个管理员来「驳回」，必须落空
	if _, err := svc.ReviewMerchant(ctx, adminID, userID, service.MerchantRejected, "反悔了"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("已裁决的申请再次审核应返回 ErrNotFound，实际: %v", err)
	}

	// 库里必须还是 APPROVED —— 这条断言才是重点：
	// 光断言返回错误，抓不到"返回错误但状态被改了"这种更糟的形态。
	var status, reason string
	if err := pool.QueryRow(ctx,
		`SELECT status, reject_reason FROM merchant_profiles WHERE user_id = $1`, userID).
		Scan(&status, &reason); err != nil {
		t.Fatalf("回读状态失败: %v", err)
	}
	if status != service.MerchantApproved {
		t.Errorf("第二次审核不该改变状态，实际 %s（CAS 失效，管理员之间会互相覆盖结论）", status)
	}
	if reason != "" {
		t.Errorf("第二次审核不该写入驳回理由，实际 %q", reason)
	}
}

func TestReviewMerchant_Validation(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	adminID, adminPhone := seedAdminUser(t, pool)
	registerCleanup(t, pool, adminPhone)
	userID, phone := seedMerchantUser(t, pool)
	registerCleanup(t, pool, phone)
	setProfileStatus(t, pool, userID, service.MerchantPending, "")

	cases := []struct {
		name     string
		decision string
		reason   string
	}{
		{"驳回不给理由", service.MerchantRejected, "   "},
		{"未知结论", "MAYBE", ""},
		{"结论是 SUSPENDED（不是审核动作）", service.MerchantSuspended, ""},
		{"驳回理由超 500 字", service.MerchantRejected, strings.Repeat("理", 501)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.ReviewMerchant(ctx, adminID, userID, tc.decision, tc.reason); !errors.Is(err, domain.ErrValidation) {
				t.Fatalf("应返回 ErrValidation，实际: %v", err)
			}
			// 校验失败必须不落库，否则"填错一次就把申请消耗掉"
			var status string
			if err := pool.QueryRow(ctx,
				`SELECT status FROM merchant_profiles WHERE user_id = $1`, userID).Scan(&status); err != nil {
				t.Fatalf("回读状态失败: %v", err)
			}
			if status != service.MerchantPending {
				t.Fatalf("校验失败不该消耗申请，状态变成了 %s", status)
			}
		})
	}

	// 通过时把上一次的驳回理由清掉，避免"已通过"的记录上还挂着驳回文案
	approved, err := svc.ReviewMerchant(ctx, adminID, userID, service.MerchantApproved, "这条理由应被忽略")
	if err != nil {
		t.Fatalf("审核通过失败: %v", err)
	}
	if approved.RejectReason != "" {
		t.Errorf("通过时应清空驳回理由，实际 %q", approved.RejectReason)
	}
}

func TestListMerchantProfiles_FiltersAndValidatesRequest(t *testing.T) {
	svc, pool := newMerchantTestService(t)
	ctx := context.Background()

	userID, phone := seedMerchantUser(t, pool)
	registerCleanup(t, pool, phone)
	setProfileStatus(t, pool, userID, service.MerchantRejected, "营业执照模糊")

	// 未知状态必须报错，而不是静默返回空列表 ——
	// 静默返回空会让调用方以为"没有待审商户"，而实际上是它传错了参数。
	if _, _, err := svc.ListMerchantProfiles(ctx, "PENDIGN", 1, 20); !errors.Is(err, domain.ErrValidation) {
		t.Fatalf("未知状态应返回 ErrValidation，实际: %v", err)
	}

	// 按 REJECTED 过滤必须能查到这一条（同时验证 join 出来的 phone 不为空）
	rows, total, err := svc.ListMerchantProfiles(ctx, service.MerchantRejected, 1, 100)
	if err != nil {
		t.Fatalf("查询审核队列失败: %v", err)
	}
	if total < 1 {
		t.Fatalf("REJECTED 队列应有至少 1 条，实际 %d", total)
	}
	var found *service.MerchantProfileView
	for i := range rows {
		if rows[i].UserID == userID {
			found = &rows[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("按 REJECTED 过滤没查到自己刚造的那条（userID=%s）", userID)
	}
	if found.RejectReason != "营业执照模糊" {
		t.Errorf("驳回理由 = %q，期望 %q", found.RejectReason, "营业执照模糊")
	}
	if found.Phone != phone {
		t.Errorf("列表里的 phone = %q，期望 %q（join 没接上）", found.Phone, phone)
	}

	// 传空状态 = 不过滤，条数不应少于按状态过滤的条数
	if _, allTotal, err := svc.ListMerchantProfiles(ctx, "", 1, 1); err != nil || allTotal < total {
		t.Errorf("不过滤时总数 %d 不应小于过滤后 %d（err=%v）", allTotal, total, err)
	}
}

// ---------- 辅助 ----------

func uniquePhone() string {
	// 固定 11 位、以 1 开头，落进 phoneRe 的合法区间。
	// 加一个进程内自增，避免同一纳秒内的两次调用撞号。
	n := (time.Now().UnixNano() + phoneSeq.Add(1)) % 1e10
	return fmt.Sprintf("1%010d", n)
}

var phoneSeq atomic.Int64

func countUsers(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("统计用户数失败: %v", err)
	}
	return n
}

func seedMerchantUser(t *testing.T, pool *pgxpool.Pool) (userID, phone string) {
	t.Helper()
	userID = uuid.NewString()
	phone = uniquePhone()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, role, phone, password_hash, nickname)
		 VALUES ($1, 'merchant', $2, 'x', '审核测试商户')`, userID, phone); err != nil {
		t.Fatalf("准备商户失败: %v", err)
	}
	return userID, phone
}

func seedAdminUser(t *testing.T, pool *pgxpool.Pool) (userID, phone string) {
	t.Helper()
	userID = uuid.NewString()
	phone = uniquePhone()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO users (id, role, phone, password_hash, nickname)
		 VALUES ($1, 'admin', $2, 'x', '审核测试管理员')`, userID, phone); err != nil {
		t.Fatalf("准备管理员失败: %v", err)
	}
	return userID, phone
}

// setProfileStatus 直接改状态，绕过业务流程 —— 测状态机需要能摆出任意起点。
func setProfileStatus(t *testing.T, pool *pgxpool.Pool, userID, status, reason string) {
	t.Helper()
	tag, err := pool.Exec(context.Background(), `
		INSERT INTO merchant_profiles (user_id, shop_name, status, reject_reason)
		VALUES ($1, '测试店铺', $2, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET status = EXCLUDED.status, reject_reason = EXCLUDED.reject_reason, updated_at = now()`,
		userID, status, reason)
	if err != nil {
		t.Fatalf("设置入驻状态失败: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("设置入驻状态影响 %d 行，期望 1", tag.RowsAffected())
	}
}

// registerCleanup 按手机号注册清理。用手机号而不是 id 定位：
// 注册用例里 id 是服务端生成的，测试事先并不知道。
func registerCleanup(t *testing.T, pool *pgxpool.Pool, phone string) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// merchant_profiles 是 ON DELETE CASCADE，删 users 即可
		if _, err := pool.Exec(ctx, `DELETE FROM users WHERE phone = $1`, phone); err != nil {
			t.Logf("清理手机号 %s 失败: %v", phone, err)
		}
	})
}
