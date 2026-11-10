package quark

// 夸克「网页版」扫码登录：用手机夸克扫一次，直接换回网页 Cookie。
//
// 之前配转存 Cookie 要用户开 F12 从 Network 里复制，既麻烦又容易漏字段。
// 这套是夸克网页自己在用的 CAS 扫码流程（没有官方文档）：
//
//	GET https://uop.quark.cn/cas/ajax/getTokenForQrcodeLogin?client_id=532&v=1.2&request_id=<UUID>
//	    → status=2000000, data.members.token
//	二维码内容：https://su.quark.cn/4_eMHBJ?token=<token>   ← 只能带 token
//	轮询 GET .../getServiceTicketByQrcodeToken?client_id=532&v=1.2&token=<token>&request_id=<同一个 UUID>
//	    → status=2000000 时 data.members.service_ticket
//	GET https://pan.quark.cn/account/info?st=<ticket>&lw=scan
//	    → 响应 Set-Cookie 就是登录态，收进 cookiejar 再拼成字符串
//
// 三个以前踩过的坑，都在这里钉死：
//  1. request_id 必须生成一次、全程复用（取 token 与轮询同一个值）。
//     之前每次 Poll 重新生成一个，会话对不上，手机端就会报「登录请求过期」。
//  2. 二维码 URL 只带 token。之前多拼了 client_id / ssb，那是旧版网页参数，
//     夸克 App 扫到会走一个已经下线的入口。
//  3. 取 token 要带 Origin，且要校验 status；否则接口改版时画出来的是个废码。
//
// 拿到的 Cookie 和手贴的完全等价，交给 RefreshPuus 保活即可。

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"strm115/internal/netproxy"
)

const (
	quarkLoginClientID = "532"
	quarkLoginVersion  = "1.2"

	// QRTokenTTL 是本地给二维码划的窗口。夸克服务端约 120 秒作废，
	// 宁可提前 10 秒换一张新码，也别让用户扫到一张会报「登录请求过期」的旧码。
	QRTokenTTL = 110 * time.Second
)

// ErrQRExpired 表示上游已经把这张码判作废，调用方应立即换码而不是继续轮询。
var ErrQRExpired = errors.New("夸克扫码登录请求已过期")

// 三个接口根用 var：单测要把它们指到假网关，不出网也能验完整流程。
var (
	quarkCASBase     = "https://uop.quark.cn/cas/ajax"
	quarkScanURL     = "https://su.quark.cn/4_eMHBJ"
	quarkAccountInfo = "https://pan.quark.cn/account/info"
)

// QRLogin 是一次网页扫码会话。一个实例对应一个二维码，不要并发复用。
type QRLogin struct {
	hc        *http.Client
	token     string
	requestID string
	url       string
}

func NewQRLogin() *QRLogin {
	jar, _ := cookiejar.New(nil)
	return &QRLogin{hc: &http.Client{
		Timeout:   15 * time.Second,
		Jar:       jar,
		Transport: &http.Transport{Proxy: netproxy.URL},
	}}
}

// Start 取二维码 token，返回应该画进二维码的那个 URL。
func (q *QRLogin) Start(ctx context.Context) (string, error) {
	q.requestID = randomID()
	params := url.Values{
		"client_id":  {quarkLoginClientID},
		"v":          {quarkLoginVersion},
		"request_id": {q.requestID},
	}
	body, err := q.casGet(ctx, "getTokenForQrcodeLogin", params)
	if err != nil {
		return "", err
	}
	var out struct {
		Status  int    `json:"status"`
		Message string `json:"message"`
		Data    struct {
			Members struct {
				Token string `json:"token"`
			} `json:"members"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("夸克扫码响应不是 JSON: %w", err)
	}
	q.token = out.Data.Members.Token
	// status 缺失时（老网关/测试夹具）只看 token；明确报错时把 message 带出来。
	if out.Status != 0 && out.Status != 2000000 {
		msg := strings.TrimSpace(out.Message)
		if msg == "" {
			msg = fmt.Sprintf("status=%d", out.Status)
		}
		return "", fmt.Errorf("夸克没给扫码 token：%s", msg)
	}
	if q.token == "" {
		return "", errors.New("夸克没给扫码 token（接口可能变了）")
	}
	// 只带 token。多带任何参数都可能让夸克 App 走旧版入口。
	q.url = quarkScanURL + "?" + url.Values{"token": {q.token}}.Encode()
	return q.url, nil
}

// Poll 查一次扫码状态。没扫/没确认时 done=false；上游判作废时返回 ErrQRExpired。
func (q *QRLogin) Poll(ctx context.Context) (cookie string, done bool, err error) {
	if q.token == "" {
		return "", false, errors.New("还没生成二维码")
	}
	params := url.Values{
		"client_id":  {quarkLoginClientID},
		"v":          {quarkLoginVersion},
		"token":      {q.token},
		"request_id": {q.requestID}, // 与 Start 同一个，不能重新生成
	}
	body, err := q.casGet(ctx, "getServiceTicketByQrcodeToken", params)
	if err != nil {
		return "", false, err
	}
	var out struct {
		Status int `json:"status"`
		Data   struct {
			Members struct {
				ServiceTicket string `json:"service_ticket"`
			} `json:"members"`
		} `json:"data"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", false, fmt.Errorf("夸克扫码状态不是 JSON: %w", err)
	}
	if out.Status != 2000000 {
		if qrExpired(out.Status, out.Message) {
			return "", false, ErrQRExpired
		}
		return "", false, nil // 50004001 "Query result is empty" = 还没扫，继续等
	}
	ticket := out.Data.Members.ServiceTicket
	if ticket == "" {
		return "", false, errors.New("扫码成功但没给 service_ticket")
	}
	cookie, err = q.exchange(ctx, ticket)
	if err != nil {
		return "", false, err
	}
	return cookie, true, nil
}

// qrExpired 把「真的作废」和「还没人扫」分开。
//
// 真机实测（2026-11）：token 生成后 0~120s 轮询稳定返回
// 50004001 / "Query result is empty" = 还没扫；约 130s 起变成
// 50004002 / "Token Not Found" = 服务端已把码作废。
// 把前者误判成过期会让二维码每两秒自己刷新一次，根本没法扫。
func qrExpired(status int, msg string) bool {
	switch status {
	case 50004002, 50004003, 50004004, 50001001, 50001002:
		return true
	}
	low := strings.ToLower(msg)
	for _, k := range []string{
		"过期", "失效", "超时", "已取消",
		"expired", "expire", "timeout", "invalid",
		"not found", "not exist", "不存在",
	} {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// exchange 用 service ticket 换登录态，把 jar 里 quark.cn 的 Cookie 拼成字符串。
func (q *QRLogin) exchange(ctx context.Context, ticket string) (string, error) {
	u := quarkAccountInfo + "?" + url.Values{"st": {ticket}, "lw": {"scan"}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://pan.quark.cn/")
	res, err := q.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("换夸克登录态失败: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 1<<20))

	parsed, _ := url.Parse(quarkAccountInfo)
	cookies := q.hc.Jar.Cookies(parsed)
	parts := make([]string, 0, len(cookies))
	seen := map[string]bool{}
	for _, c := range cookies {
		if c.Name == "" || c.Value == "" || seen[c.Name] {
			continue
		}
		seen[c.Name] = true
		parts = append(parts, c.Name+"="+c.Value)
	}
	if len(parts) == 0 {
		return "", errors.New("扫码成功但夸克没下发 Cookie，请改用手动粘贴")
	}
	// 至少要能认出是登录态，否则把一串匿名 Cookie 存进去更糟。
	joined := strings.Join(parts, "; ")
	if !strings.Contains(joined, "__pus") {
		return "", errors.New("扫码结果里没有登录字段（__pus / __puus），请重试或手动粘贴")
	}
	return joined, nil
}

func (q *QRLogin) casGet(ctx context.Context, action string, params url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		quarkCASBase+"/"+action+"?"+params.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://pan.quark.cn/")
	req.Header.Set("Origin", "https://pan.quark.cn")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	res, err := q.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上夸克登录服务: %w", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("夸克登录服务返回 HTTP %d", res.StatusCode)
	}
	return io.ReadAll(io.LimitReader(res.Body, 1<<20))
}

// randomID 生成标准 UUID 形状的字符串，和夸克网页自己发的一致。
func randomID() string {
	b := make([]byte, 16)
	h := ""
	if _, err := rand.Read(b); err == nil {
		h = hex.EncodeToString(b)
	} else {
		h = fmt.Sprintf("%032x", uint64(time.Now().UnixNano()))
	}
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}
