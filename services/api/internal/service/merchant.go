package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"

	"github.com/tutic/itiao-mall/services/api/internal/domain"
	"github.com/tutic/itiao-mall/services/api/internal/repo"
)

// 商户入驻状态。与 DB 的 merchant_profiles_status_check 一一对应 ——
// 改这里必须同时改约束，反之亦然，否则会出现「代码认为合法、数据库插入失败」。
const (
	MerchantPending   = "PENDING"
	MerchantApproved  = "APPROVED"
	MerchantRejected  = "REJECTED"
	MerchantSuspended = "SUSPENDED"
)

// IsMerchantStatus 判断一个状态字面量是否是已知状态。
// 用于把 URL query 里的任意字符串挡在 SQL 之前 —— 直接把它透传给 SQL 不会出错，
// 但会静默返回空列表，调用方会以为"没有数据"而不是"你传错了"。
func IsMerchantStatus(s string) bool {
	switch s {
	case MerchantPending, MerchantApproved, MerchantRejected, MerchantSuspended:
		return true
	}
	return false
}

// 手机号：宽松校验（6–20 位数字，可带一个前导 +）。
//
// 刻意**不**写死 `^1[3-9]\d{9}$`：这个商城将来可能收海外号，
// 而把校验写死在正则里的代价是「线上来了一个合法号却注册不了」，
// 比"放宽一点"更难收拾。真正的唯一性由数据库唯一约束保证，正则只负责挡明显垃圾。
var phoneRe = regexp.MustCompile(`^\+?\d{6,20}$`)

// ★ bcrypt 只使用密码的前 **72 字节**，超出部分被静默丢弃。
//
// 这不只是"截断"：`GenerateFromPassword` 对 >72 字节会直接报错（x/crypto 的行为），
// 但 `CompareHashAndPassword` **不报错、静默按前 72 字节比对**。两者行为不一致，
// 结果是"注册时被拒的密码，登录时反而能通过"这一对诡异组合。
// 所以在这里一次性卡住，让注册与登录的边界完全一致。
const maxPasswordBytes = 72

// MerchantRegisterInput 是商户自助注册的输入。
type MerchantRegisterInput struct {
	Phone        string
	Password     string
	ShopName     string
	ContactName  string
	ContactPhone string
}

// MerchantProfileInput 是入驻资料的更新输入。
type MerchantProfileInput struct {
	ShopName     string
	ContactName  string
	ContactPhone string
	Description  string
	LogoURL      string
}

// RegisterMerchant 商户自助注册。
//
// 用户与入驻资料在**同一个事务**里创建。分两步写的话，进程在中间挂掉就留下一个
// 「有账号、没资料」的商户 —— 他既进不了商户端（查不到 profile → 未通过），
// 也无法重新注册（手机号已占用），彻底卡死，只能人工介入。
func (s *Service) RegisterMerchant(ctx context.Context, in MerchantRegisterInput) (*Session, error) {
	if err := validateRegisterInput(in); err != nil {
		return nil, err
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		// 走到这里说明长度绕过上面的校验，或 bcrypt 的 cost 出了问题
		return nil, fmt.Errorf("密码哈希失败: %w", err)
	}
	hashStr := string(hash)

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("开启事务失败: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	q := s.Q.WithTx(tx)

	user, err := q.CreateMerchantUser(ctx, repo.CreateMerchantUserParams{
		Phone:        &in.Phone,
		PasswordHash: &hashStr,
		// 昵称取店名：商户在列表里显示的是店铺，不是个人昵称。
		// 之后 PUT /merchant/profile 改的是资料表，不动 users.nickname —— 两者刻意不联动，
		// 免得"改了店名，历史订单里的商户名跟着变"。
		Nickname: in.ShopName,
	})
	if err != nil {
		if isUniqueViolation(err) {
			return nil, fmt.Errorf("%w: 该手机号已注册", domain.ErrConflict)
		}
		return nil, fmt.Errorf("创建商户账号失败: %w", err)
	}

	if _, err := q.CreateMerchantProfile(ctx, repo.CreateMerchantProfileParams{
		UserID:       user.ID,
		ShopName:     in.ShopName,
		ContactName:  in.ContactName,
		ContactPhone: in.ContactPhone,
	}); err != nil {
		return nil, fmt.Errorf("创建入驻资料失败: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("提交事务失败: %w", err)
	}

	return s.issueSession(ctx, user)
}

func validateRegisterInput(in MerchantRegisterInput) error {
	if !phoneRe.MatchString(in.Phone) {
		return fmt.Errorf("%w: 手机号格式不正确", domain.ErrValidation)
	}
	if len(in.Password) < 8 {
		return fmt.Errorf("%w: 密码至少 8 位", domain.ErrValidation)
	}
	if len(in.Password) > maxPasswordBytes {
		// 用字节数而不是字符数：bcrypt 的 72 是字节。中文密码 24 个字就到顶了。
		return fmt.Errorf("%w: 密码不能超过 %d 字节（bcrypt 的输入上限）",
			domain.ErrValidation, maxPasswordBytes)
	}
	if err := validateProfileInput(MerchantProfileInput{
		ShopName:     in.ShopName,
		ContactName:  in.ContactName,
		ContactPhone: in.ContactPhone,
	}); err != nil {
		return err
	}
	return nil
}

func validateProfileInput(in MerchantProfileInput) error {
	if strings.TrimSpace(in.ShopName) == "" {
		return fmt.Errorf("%w: 店铺名不能为空", domain.ErrValidation)
	}
	if len([]rune(in.ShopName)) > 100 {
		return fmt.Errorf("%w: 店铺名不能超过 100 字", domain.ErrValidation)
	}
	if len([]rune(in.ContactName)) > 50 {
		return fmt.Errorf("%w: 联系人不能超过 50 字", domain.ErrValidation)
	}
	if len(in.ContactPhone) > 32 {
		return fmt.Errorf("%w: 联系电话过长", domain.ErrValidation)
	}
	if len([]rune(in.Description)) > 1000 {
		return fmt.Errorf("%w: 店铺简介不能超过 1000 字", domain.ErrValidation)
	}
	if len(in.LogoURL) > 500 {
		return fmt.Errorf("%w: logo 地址过长", domain.ErrValidation)
	}
	return nil
}

// MerchantProfileView 是入驻资料的领域视图。
//
// 为什么要多这一层：sqlc 为「单条查询」和「带分页的列表查询」生成了两个字段完全相同的
// 行类型（GetMerchantProfileRow / ListMerchantProfilesRow），它们不能互相赋值。
// 让它们直接流到 HTTP 层的话，转换逻辑就会有两份 —— 将来加一个字段，
// 极容易只改其中一条，表现为「列表里有、详情里没有」这种最难查的不一致。
// 在 service 层收敛成一个类型，转换点就只剩一个。
type MerchantProfileView struct {
	UserID       string
	ShopName     string
	ContactName  string
	ContactPhone string
	Description  string
	LogoURL      string
	Status       string
	RejectReason string
	Phone        string
	ReviewedAt   *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// MerchantStatus 读某商户的入驻状态。查不到行 = 从未入驻，返回 ErrNotFound。
//
// 这是唯一的「查库判审核」入口，authGate 之后的 merchantStatusGate 每个请求都会调它。
func (s *Service) MerchantStatus(ctx context.Context, userID string) (string, error) {
	status, err := s.Q.GetMerchantStatus(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", domain.ErrNotFound
		}
		return "", fmt.Errorf("查询入驻状态失败: %w", err)
	}
	return status, nil
}

// GetMerchantProfile 返回自己的入驻资料（含审核状态与驳回理由）。
func (s *Service) GetMerchantProfile(ctx context.Context, userID string) (*MerchantProfileView, error) {
	row, err := s.Q.GetMerchantProfile(ctx, userID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("查询入驻资料失败: %w", err)
	}
	return viewFromGetRow(row), nil
}

// UpdateMerchantProfile 更新入驻资料；被驳回时顺带重新提交（状态回 PENDING）。
// 状态流转规则写在 SQL 的 CASE 里（原子），见 db/queries/merchant.sql 的说明。
//
// 写完再读一次而不是用 RETURNING 的结果直接拼响应：原因是 RETURNING 拿不到 users.phone
// （那张表没参与 UPDATE），而响应里必须有 phone 才能和 GET 的形状一致。
// 顺带的好处是响应反映的是**库里的真实状态**，而不是我们认为写进去的东西。
func (s *Service) UpdateMerchantProfile(
	ctx context.Context, userID string, in MerchantProfileInput,
) (*MerchantProfileView, error) {
	if err := validateProfileInput(in); err != nil {
		return nil, err
	}
	if _, err := s.Q.UpsertMerchantProfile(ctx, repo.UpsertMerchantProfileParams{
		UserID:       userID,
		ShopName:     strings.TrimSpace(in.ShopName),
		ContactName:  in.ContactName,
		ContactPhone: in.ContactPhone,
		Description:  in.Description,
		LogoUrl:      in.LogoURL,
	}); err != nil {
		return nil, fmt.Errorf("更新入驻资料失败: %w", err)
	}
	return s.GetMerchantProfile(ctx, userID)
}

// ListMerchantProfiles 平台审核队列。
func (s *Service) ListMerchantProfiles(
	ctx context.Context, status string, page, pageSize int32,
) ([]MerchantProfileView, int64, error) {
	if status != "" && !IsMerchantStatus(status) {
		return nil, 0, fmt.Errorf("%w: 未知的入驻状态 %q", domain.ErrValidation, status)
	}
	lim, off := normalizePage(page, pageSize)
	statusArg := normalizeSearch(status)

	rows, err := s.Q.ListMerchantProfiles(ctx, repo.ListMerchantProfilesParams{
		Status: statusArg, Lim: lim, Off: off,
	})
	if err != nil {
		return nil, 0, fmt.Errorf("查询商户列表失败: %w", err)
	}
	total, err := s.Q.CountMerchantProfiles(ctx, statusArg)
	if err != nil {
		return nil, 0, fmt.Errorf("统计商户数失败: %w", err)
	}

	views := make([]MerchantProfileView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewFromListRow(row))
	}
	return views, total, nil
}

// ReviewMerchant 平台裁决一条入驻申请。
//
// CAS 语义：只有 PENDING 能被裁决，影响行数为 0 一律当作 404（不存在或已被处理）。
func (s *Service) ReviewMerchant(
	ctx context.Context, adminID, merchantUserID, decision, rejectReason string,
) (*MerchantProfileView, error) {
	switch decision {
	case MerchantApproved, MerchantRejected:
	default:
		return nil, fmt.Errorf("%w: 审核结论只能是 %s 或 %s",
			domain.ErrValidation, MerchantApproved, MerchantRejected)
	}

	rejectReason = strings.TrimSpace(rejectReason)
	if decision == MerchantRejected && rejectReason == "" {
		// 理由必填不是为了走流程：没有理由的驳回，商户只能靠猜，然后反复盲提交，
		// 平台的审核量反而被推高。
		return nil, fmt.Errorf("%w: 驳回必须给出理由", domain.ErrValidation)
	}
	if decision == MerchantApproved {
		// 通过时清空理由，避免上一次驳回的文案留在「已通过」的记录上误导商户。
		rejectReason = ""
	}
	if len([]rune(rejectReason)) > 500 {
		return nil, fmt.Errorf("%w: 驳回理由不能超过 500 字", domain.ErrValidation)
	}

	if _, err := s.Q.ReviewMerchantProfile(ctx, repo.ReviewMerchantProfileParams{
		UserID:       merchantUserID,
		Status:       decision,
		RejectReason: rejectReason,
		ReviewedBy:   adminID,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// 不存在，或已被其他管理员处理过。统一 404 —— 区分这两者对调用方没有价值，
			// 却会泄露「这条申请存在」。
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("审核商户失败: %w", err)
	}
	return s.GetMerchantProfile(ctx, merchantUserID)
}

// ---- 行 → 视图 ----

func viewFromGetRow(r repo.GetMerchantProfileRow) *MerchantProfileView {
	return &MerchantProfileView{
		UserID:       r.UserID,
		ShopName:     r.ShopName,
		ContactName:  r.ContactName,
		ContactPhone: r.ContactPhone,
		Description:  r.Description,
		LogoURL:      r.LogoUrl,
		Status:       r.Status,
		RejectReason: r.RejectReason,
		Phone:        derefOrEmpty(r.Phone),
		ReviewedAt:   pgTimePtr(r.ReviewedAt),
		CreatedAt:    pgTime(r.CreatedAt),
		UpdatedAt:    pgTime(r.UpdatedAt),
	}
}

func viewFromListRow(r repo.ListMerchantProfilesRow) MerchantProfileView {
	return MerchantProfileView{
		UserID:       r.UserID,
		ShopName:     r.ShopName,
		ContactName:  r.ContactName,
		ContactPhone: r.ContactPhone,
		Description:  r.Description,
		LogoURL:      r.LogoUrl,
		Status:       r.Status,
		RejectReason: r.RejectReason,
		Phone:        derefOrEmpty(r.Phone),
		ReviewedAt:   pgTimePtr(r.ReviewedAt),
		CreatedAt:    pgTime(r.CreatedAt),
		UpdatedAt:    pgTime(r.UpdatedAt),
	}
}

func derefOrEmpty(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}
