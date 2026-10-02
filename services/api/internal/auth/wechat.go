package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// WechatUser 是从微信换回来的用户标识。
type WechatUser struct {
	OpenID    string
	UnionID   string
	Nickname  string
	AvatarURL string
}

// WechatExchanger 把 App 传来的一次性 code 换成 openid/unionid。
// mock 与真实实现都满足它，靠 AUTH_PROVIDER 切换，业务代码不用改。
type WechatExchanger interface {
	Exchange(ctx context.Context, code string) (WechatUser, error)
}

// ---------- mock ----------

// MockExchanger 不做任何网络请求：把 code 哈希成稳定的 openid。
//
// 这个"稳定"很关键 —— 压测时用 code="load_user_1".."load_user_N" 就能拿到
// 固定的一批用户，每次跑的数字才有可比性。
type MockExchanger struct{}

func (MockExchanger) Exchange(_ context.Context, code string) (WechatUser, error) {
	code = strings.TrimSpace(code)
	if code == "" {
		return WechatUser{}, fmt.Errorf("%w: code 为空", ErrInvalidCredentials)
	}
	sum := sha256.Sum256([]byte(code))
	return WechatUser{
		OpenID:    "mock_openid_" + hex.EncodeToString(sum[:8]),
		UnionID:   "mock_unionid_" + hex.EncodeToString(sum[8:16]),
		Nickname:  "用户_" + code,
		AvatarURL: "",
	}, nil
}

// ---------- 真实微信 ----------

// WechatExchangerImpl 调微信开放平台的 code2Session 接口。
// 注意：AppSecret 只在服务端使用，绝不下发客户端。
type WechatExchangerImpl struct {
	AppID      string
	AppSecret  string
	BaseURL    string // 可覆盖，便于测试时指向假服务器
	HTTPClient *http.Client
}

func NewWechatExchanger(appID, appSecret string) *WechatExchangerImpl {
	return &WechatExchangerImpl{
		AppID:      appID,
		AppSecret:  appSecret,
		BaseURL:    "https://api.weixin.qq.com",
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	}
}

type wechatTokenResp struct {
	AccessToken  string `json:"access_token"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	OpenID       string `json:"openid"`
	Scope        string `json:"scope"`
	UnionID      string `json:"unionid"`
	ErrCode      int    `json:"errcode"`
	ErrMsg       string `json:"errmsg"`
}

func (w *WechatExchangerImpl) Exchange(ctx context.Context, code string) (WechatUser, error) {
	if strings.TrimSpace(code) == "" {
		return WechatUser{}, fmt.Errorf("%w: code 为空", ErrInvalidCredentials)
	}

	q := url.Values{}
	q.Set("appid", w.AppID)
	q.Set("secret", w.AppSecret)
	q.Set("code", code)
	q.Set("grant_type", "authorization_code")

	endpoint := w.BaseURL + "/sns/oauth2/access_token?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return WechatUser{}, fmt.Errorf("构造微信请求失败: %w", err)
	}

	resp, err := w.HTTPClient.Do(req)
	if err != nil {
		return WechatUser{}, fmt.Errorf("请求微信失败: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	// 微信即使出错也返回 200，错误在 body 的 errcode 里 —— 不能只看状态码。
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return WechatUser{}, fmt.Errorf("读取微信响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return WechatUser{}, fmt.Errorf("微信返回状态 %d: %s", resp.StatusCode, string(body))
	}

	var parsed wechatTokenResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return WechatUser{}, fmt.Errorf("解析微信响应失败: %w", err)
	}
	if parsed.ErrCode != 0 {
		return WechatUser{}, fmt.Errorf("%w: 微信返回 errcode=%d errmsg=%s",
			ErrInvalidCredentials, parsed.ErrCode, parsed.ErrMsg)
	}
	if parsed.OpenID == "" {
		return WechatUser{}, fmt.Errorf("%w: 微信响应缺少 openid", ErrInvalidCredentials)
	}

	return WechatUser{
		OpenID:  parsed.OpenID,
		UnionID: parsed.UnionID, // 未绑定开放平台时可能为空，落库时兜底用 openid
	}, nil
}
