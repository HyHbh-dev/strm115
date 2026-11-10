package driverquarktv

// 扫码登录。
//
// 夸克 TV 的登录是三步设备码流程，和 115 的二维码轮询很像，但少一个「已扫码」
// 中间态：没有 code 就是还没确认，拿到 code 就说明已经点了确认。
//
//	GET  /oauth/authorize?auth_type=code&client_id=…&scope=netdisk&qrcode=1
//	      → qr_data（base64 的 JPEG，直接塞进 <img>）+ query_token
//	GET  /oauth/code?client_id=…&scope=netdisk&query_token=…
//	      → code（没确认时为空）
//	POST {codeApi}/token  {code, device…}
//	      → access_token + refresh_token
//
// 全程 device_id 必须一致：它决定了 token 归属哪台「设备」，中途换了就等于
// 换了一台机器，前面扫的码白扫。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"strm115/internal/pan"
)

// QRLogin 是一次扫码会话。
type QRLogin struct {
	c          *Client
	QueryToken string
	// QR 是 data:image/jpeg;base64,… 形式，前端可以直接给 <img src>。
	QR       string
	StartAt  time.Time
	LastErr  string
	consumed bool
}

// NewQRLogin 开一次新会话。base/codeBase 留空用官方地址（测试会指到假服务器）。
func NewQRLogin(base, codeBase string) *QRLogin {
	cr := Credential{DeviceID: newDeviceID()}
	c := New(cr)
	if strings.TrimSpace(base) != "" {
		c.base = base
	}
	if strings.TrimSpace(codeBase) != "" {
		c.codeBase = codeBase
	}
	return &QRLogin{c: c, StartAt: time.Now()}
}

// DeviceID 给账号凭证用。
func (q *QRLogin) DeviceID() string { return q.c.cred.DeviceID }

// Authorize 取二维码。返回的字符串是裸 base64（JPEG），前端拼 data URL 用。
func (q *QRLogin) Authorize(ctx context.Context) (string, error) {
	q2 := url.Values{}
	q2.Set("auth_type", "code")
	q2.Set("client_id", clientID)
	q2.Set("scope", "netdisk")
	q2.Set("qrcode", "1")
	q2.Set("qr_width", "460")
	q2.Set("qr_height", "460")

	body, err := q.c.rawRequest(ctx, http.MethodGet, "/oauth/authorize", q2)
	if err != nil {
		return "", err
	}
	var out struct {
		Status     int    `json:"status"`
		Errno      int    `json:"errno"`
		ErrorInfo  string `json:"error_info"`
		QRData     string `json:"qr_data"`
		QueryToken string `json:"query_token"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", pan.NewErr(pan.ErrTransient, "夸克扫码", "返回的不是 JSON", err)
	}
	if out.Errno != 0 || out.Status >= 400 {
		return "", classifyAPI("夸克扫码", out.Status, out.Errno, out.ErrorInfo)
	}
	if strings.TrimSpace(out.QRData) == "" {
		return "", pan.NewErr(pan.ErrRejected, "夸克扫码", "夸克没返回二维码，可能是接口变更", nil)
	}
	q.QueryToken = out.QueryToken
	return out.QRData, nil
}

// Poll 查一次扫码结果。
// 返回 (credential, true, nil) 表示拿到 refresh_token，可以建账号了。
// 返回 (_, false, nil) 表示还没确认，继续轮询。
//
// 注意：这里刻意不把「还没确认」当错误 —— 前端是长轮询，每次都拿错误会让
// 页面上一秒弹一个红条。
func (q *QRLogin) Poll(ctx context.Context) (Credential, bool, error) {
	if q.QueryToken == "" {
		return Credential{}, false, pan.NewErr(pan.ErrRejected, "夸克扫码", "会话里没有 query_token，请刷新二维码", nil)
	}
	q2 := url.Values{}
	q2.Set("client_id", clientID)
	q2.Set("scope", "netdisk")
	q2.Set("query_token", q.QueryToken)

	body, err := q.c.rawRequest(ctx, http.MethodGet, "/oauth/code", q2)
	if err != nil {
		return Credential{}, false, err
	}
	var out struct {
		Status    int    `json:"status"`
		Errno     int    `json:"errno"`
		ErrorInfo string `json:"error_info"`
		Code      string `json:"code"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Credential{}, false, pan.NewErr(pan.ErrTransient, "夸克扫码", "返回的不是 JSON", err)
	}
	// 没确认时夸克会回各种错误码（有的说「等待确认」，有的直接 4xx），
	// 只要没给 code 都按「继续等」处理；真出问题会在换 token 那步暴露。
	if strings.TrimSpace(out.Code) == "" {
		return Credential{}, false, nil
	}
	if q.consumed {
		return Credential{}, false, nil
	}

	td, err := q.c.exchange(ctx, map[string]string{"code": out.Code})
	if err != nil {
		return Credential{}, false, err
	}
	q.consumed = true
	// 存下 access_token，好让紧随其后的 /user 取昵称不至于再刷一次。
	q.c.mu.Lock()
	q.c.access = td.AccessToken
	q.c.cred.RefreshToken = td.RefreshToken
	q.c.mu.Unlock()

	return Credential{
		RefreshToken: td.RefreshToken,
		DeviceID:     q.DeviceID(),
		QueryToken:   q.QueryToken,
	}, true, nil
}

// Nickname 用刚换到的 token 取昵称，失败返回空串（不影响建账号）。
func (q *QRLogin) Nickname(ctx context.Context) string { return q.c.Nickname(ctx) }
