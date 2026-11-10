// Package driverquarktv 把「夸克网盘 TV 版开放接口」适配成本项目的 pan.Driver。
//
// 为什么选 TV 版：夸克网页/PC 接口对 Cookie、请求头、频率都敏感，下载直链基本
// 都要求会员；TV 版开放接口给的是能直接 302 的 download_url，播放器自己就能播，
// 服务端一次都不用中转——这正是这个项目「小 NAS 也能跑」的前提。
//
// 代价是能力不全：TV 接口只提供「列目录」和「取下载直链」，没有上传、移动、改名、
// 删除，更没有分享转存。所以 Stat 返回 pan.ErrUnsupported；移动/改名/删除/建目录
// 改走网页接口（见 web.go），那套要网页 Cookie。
//
// 分享转存走另一个包 internal/quark（夸克网页接口 + Cookie）。两条网页通道加上
// TV 接口，才是完整的夸克链路：列目录与播放用 TV，转存与整理用网页接口。
package driverquarktv

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/netproxy"
	"strm115/internal/pan"
)

const (
	// Kind 是账号表里的类型名。
	Kind = "quarktv"

	// 下面四个是 TV 客户端的固定身份。改任何一个都会让签名对不上。
	clientID = "d3194e61504e493eb6222857bccfed94"
	signKey  = "kw2dvtd7p4t3pjl2d9ed9yc8yej8kw2d"
	appVer   = "1.5.6"
	channel  = "CP"

	// ua 必须和 appVer 那一套对得上。夸克按 UA 判客户端，掺一个别的 UA
	// 会被当成「异常设备」要求重新登录。
	ua = "Mozilla/5.0 (Linux; U; Android 13; zh-cn; M2004J7AC Build/UKQ1.231108.001) " +
		"AppleWebKit/533.1 (KHTML, like Gecko) Mobile Safari/533.1"

	// 设备指纹：这些字段只参与「是不是同一台设备」的判断，值本身不重要，
	// 但同一次登录的 authorize/code/token/业务请求必须完全一致。
	deviceBrand  = "Xiaomi"
	platform     = "tv"
	deviceName   = "M2004J7AC"
	deviceModel  = "M2004J7AC"
	buildDevice  = "M2004J7AC"
	buildProduct = "M2004J7AC"
	deviceGpu    = "Adreno (TM) 550"
	activityRect = "{}"

	reqTimeout = 20 * time.Second
)

// 接口根声明成 var 而不是 const：既要让单测指到假网关，也要让设置页换反代。
// 对外的入口是 SetUpstream，别的地方不要直接改。
var (
	// DefaultBase 是 TV 版开放接口根（第三方 TV 客户端用的那一套）。
	DefaultBase = officialBase
	// DefaultCodeBase 是换 token 的独立网关。它和业务接口不同域，
	// 而且只认 code / refresh_token，不带签名头——所以单独一份地址。
	DefaultCodeBase = officialCodeBase
)

const (
	officialBase     = "https://open-api-drive.quark.cn"
	officialCodeBase = "http://api.extscreen.com/quarkdrive"
)

// SetUpstream 换接口根。留空 = 恢复官方地址。
//
// 和 tmdb.SetUpstream 同一套语义：进程级配置，启动时设一次，设置页改完再设一次。
// 只在启动/改设置时调用，不要在请求路径上调用 —— 并发改包级变量有竞态。
func SetUpstream(base, codeBase string) {
	if v := strings.TrimRight(strings.TrimSpace(base), "/"); v != "" {
		DefaultBase = v
	} else {
		DefaultBase = officialBase
	}
	if v := strings.TrimRight(strings.TrimSpace(codeBase), "/"); v != "" {
		DefaultCodeBase = v
	} else {
		DefaultCodeBase = officialCodeBase
	}
}

// Credential 是 accounts.credential 里存的那段 JSON。
//
// AccessToken 刻意不存：它一小时左右就过期，存了也会立刻变成废纸，
// 反而给「数据库里的 token 为什么和实际用的不一样」这种问题留了坑。
type Credential struct {
	RefreshToken string `json:"refresh_token"`
	DeviceID     string `json:"device_id"`
	// WebCookie 是夸克网页版的 Cookie，只给 internal/quark 的分享转存用。
	// TV 接口不需要它，它也刷不出 refresh_token —— 两者是两条独立通道，
	// 放在同一个凭证里只是因为「对用户来说就是一个夸克账号」。
	WebCookie string `json:"web_cookie,omitempty"`
	// QueryToken 只在「扫了码但还没换到 refresh_token」时有意义，
	// 留着是为了让扫码会话能被重启后的进程接上。
	QueryToken string `json:"query_token,omitempty"`
}

func init() { pan.Register(Kind, Open) }

// Open 由 pan.Open 调用。
func Open(ctx context.Context, acct pan.Account) (pan.Driver, error) {
	var cr Credential
	if err := json.Unmarshal([]byte(acct.Credential), &cr); err != nil {
		return nil, pan.NewErr(pan.ErrAuth, "打开账号", "夸克凭证格式错误，请重新扫码登录", err)
	}
	if strings.TrimSpace(cr.RefreshToken) == "" {
		return nil, pan.NewErr(pan.ErrAuth, "打开账号", "夸克凭证里没有 refresh_token，请重新扫码登录", nil)
	}
	if cr.DeviceID == "" {
		cr.DeviceID = newDeviceID()
	}
	c := New(cr)
	// 建客户端时就换一次 access_token：凭证失效要在这里立刻暴露，
	// 而不是等到用户点「同步」时才发现。
	if err := c.ensureToken(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// New 用已有凭证建客户端。base/codeBase 留空则用官方地址（测试会指到假服务器）。
func New(cr Credential) *Client {
	return &Client{
		cred:     cr,
		base:     DefaultBase,
		codeBase: DefaultCodeBase,
		hc:       &http.Client{Timeout: reqTimeout, Transport: &http.Transport{Proxy: netproxy.URL}},
	}
}

type Client struct {
	base     string
	codeBase string
	cred     Credential
	hc       *http.Client

	mu     sync.Mutex
	access string
}

// Credential 返回当前凭证（刷新后 refresh_token 会变，调用方要写回账号表）。
func (c *Client) Credential() Credential {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cred
}

func (c *Client) Kind() string { return Kind }

func (c *Client) Close() error {
	c.hc.CloseIdleConnections()
	return nil
}

// ==================== 签名与请求 ====================

// sign 生成 TV 接口要求的三件套：时间戳、x-pan-token、req_id。
//
// token 是 sha256("METHOD&/path&ts&signKey")：所以 path 必须和真实请求路径
// 完全一致（不含 query），method 也必须一致，否则网关直接拒。
func (c *Client) sign(method, pathname string) (ts, token, reqID string) {
	ts = strconv.FormatInt(time.Now().UnixMilli(), 10)
	if c.cred.DeviceID == "" {
		c.cred.DeviceID = newDeviceID()
	}
	sum := md5.Sum([]byte(c.cred.DeviceID + ts))
	reqID = hex.EncodeToString(sum[:])
	raw := method + "&" + pathname + "&" + ts + "&" + signKey
	h := sha256.Sum256([]byte(raw))
	return ts, hex.EncodeToString(h[:]), reqID
}

// request 发一次业务请求，把 envelope 里的 data 解到 out。
//
// token 过期（status=-1 且 errno=10001）时自动刷新并重放一次；只重放一次，
// 免得 refresh 本身有问题时打成死循环。
func (c *Client) request(ctx context.Context, method, pathname string, q url.Values, out any) error {
	err := c.do(ctx, method, pathname, q, out)
	if !errors.Is(err, errTokenExpired) {
		return err
	}
	if err := c.refresh(ctx); err != nil {
		return err
	}
	return c.do(ctx, method, pathname, q, out)
}

func (c *Client) do(ctx context.Context, method, pathname string, q url.Values, out any) error {
	body, err := c.rawRequest(ctx, method, pathname, q)
	if err != nil {
		return err
	}
	return decodeEnvelope(body, pathname, out)
}

// rawRequest 发一次带签名的请求，返回原始 body。
//
// 单独抽出来是因为 /oauth/* 那几个接口把字段放在最外层（qr_data / query_token /
// code），而不是像业务接口那样套一层 data——解 envelope 会一个字段都取不到。
func (c *Client) rawRequest(ctx context.Context, method, pathname string, q url.Values) ([]byte, error) {
	if q == nil {
		q = url.Values{}
	}
	c.mu.Lock()
	access := c.access
	c.mu.Unlock()

	ts, token, reqID := c.sign(method, pathname)
	q.Set("req_id", reqID)
	q.Set("access_token", access)
	q.Set("app_ver", appVer)
	q.Set("device_id", c.cred.DeviceID)
	q.Set("device_brand", deviceBrand)
	q.Set("platform", platform)
	q.Set("device_name", deviceName)
	q.Set("device_model", deviceModel)
	q.Set("build_device", buildDevice)
	q.Set("build_product", buildProduct)
	q.Set("device_gpu", deviceGpu)
	q.Set("activity_rect", activityRect)
	q.Set("channel", channel)

	u := c.base + pathname + "?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return nil, pan.NewErr(pan.ErrTransient, "夸克请求", "构造请求失败", err)
	}
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-pan-tm", ts)
	req.Header.Set("x-pan-token", token)
	req.Header.Set("x-pan-client-id", clientID)

	return c.send(req)
}

// env 是所有业务响应的外壳。
type env struct {
	Status    int             `json:"status"`
	Errno     int             `json:"errno"`
	ErrorInfo string          `json:"error_info"`
	ReqID     string          `json:"req_id"`
	Data      json.RawMessage `json:"data"`
	// token 网关那套是 {code,message,data}，字段名不一样，这里一并认了。
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// errTokenExpired 是内部信号：只用来触发一次刷新，不会漏到上层。
var errTokenExpired = errors.New("access_token 过期")

func decodeEnvelope(body []byte, op string, out any) error {
	var e env
	if err := json.Unmarshal(body, &e); err != nil {
		return pan.NewErr(pan.ErrTransient, op, "夸克返回的不是 JSON（可能被网关拦了）", err)
	}
	if e.Status == -1 && e.Errno == 10001 {
		return errTokenExpired
	}
	if e.Errno != 0 || e.Status >= 400 {
		return classifyAPI(op, e.Status, e.Errno, e.ErrorInfo)
	}
	if out != nil && len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, out); err != nil {
			return pan.NewErr(pan.ErrTransient, op, "解析夸克返回失败", err)
		}
	}
	return nil
}

// classifyAPI 把夸克的中文报错翻译成 pan 的哨兵错误。
// 分错类的后果很具体：把风控当临时故障重试，就是在帮夸克封自己的号。
func classifyAPI(op string, status, errno int, info string) error {
	msg := strings.TrimSpace(info)
	if msg == "" {
		msg = fmt.Sprintf("夸克返回 status=%d errno=%d", status, errno)
	}
	switch {
	case status == 401 || status == 403,
		strings.Contains(msg, "登录"), strings.Contains(msg, "token"),
		strings.Contains(msg, "未授权"):
		return pan.NewErr(pan.ErrAuth, op, msg, nil)
	case strings.Contains(msg, "频繁"), strings.Contains(msg, "限流"),
		strings.Contains(msg, "稍后"), strings.Contains(msg, "风控"):
		return pan.NewErr(pan.ErrRisk, op, msg, nil)
	case status == 404, strings.Contains(msg, "不存在"), strings.Contains(msg, "已删除"):
		return pan.NewErr(pan.ErrNotFound, op, msg, nil)
	default:
		return pan.NewErr(pan.ErrRejected, op, msg, nil)
	}
}

func (c *Client) send(req *http.Request) ([]byte, error) {
	res, err := c.hc.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return nil, req.Context().Err()
		}
		return nil, pan.NewErr(pan.ErrTransient, "夸克请求", "连接失败", err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, pan.NewErr(pan.ErrTransient, "夸克请求", "读响应失败", err)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return nil, pan.NewErr(pan.ErrRisk, "夸克请求", "夸克限流（HTTP 429）", nil)
	}
	return body, nil
}

// ensureToken 保证手里有一个能用的 access_token。
func (c *Client) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	ok := c.access != ""
	c.mu.Unlock()
	if ok {
		return nil
	}
	return c.refresh(ctx)
}

func (c *Client) refresh(ctx context.Context) error {
	c.mu.Lock()
	rt := c.cred.RefreshToken
	c.mu.Unlock()
	if strings.TrimSpace(rt) == "" {
		return pan.NewErr(pan.ErrAuth, "刷新夸克令牌", "没有 refresh_token，请重新扫码登录", nil)
	}
	cr, err := c.exchange(ctx, map[string]string{"refresh_token": rt})
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.cred.RefreshToken = cr.RefreshToken
	if cr.AccessToken != "" {
		c.access = cr.AccessToken
	}
	c.mu.Unlock()
	return nil
}

// ==================== 能力实现 ====================

type fileItem struct {
	Fid       string `json:"fid"`
	ParentFid string `json:"parent_fid"`
	Filename  string `json:"filename"`
	Size      int64  `json:"size"`
	IsDir     int    `json:"isdir"`
	Category  int    `json:"category"`
	FileType  string `json:"file_type"`
	UpdatedAt int64  `json:"updated_at"`
}

// quarkMaxPageSize 是夸克 TV /file?method=list 的硬限制：page_size 只收 1–100。
// 传 200 会直接被拒（"/file: page_size 范围: 1-100"）。
const quarkMaxPageSize = 100

// MaxPageSize 让 pan.Manager.ListAll 把统一的分页大小收敛到夸克能接受的值。
func (c *Client) MaxPageSize() int { return quarkMaxPageSize }

// ListPage 列 dirID 下的一页。TV 接口是页码制，这里换算成 offset。
func (c *Client) ListPage(ctx context.Context, dirID string, offset, limit int) ([]pan.Entry, int, error) {
	// 直接调 ListPage（不经过 Manager）时也要自己守住上限。
	if limit <= 0 || limit > quarkMaxPageSize {
		limit = quarkMaxPageSize
	}
	if strings.TrimSpace(dirID) == "" {
		dirID = "0"
	}
	page := offset / limit
	skip := offset % limit

	var out struct {
		TotalCount int64      `json:"total_count"`
		Files      []fileItem `json:"files"`
	}
	q := url.Values{}
	q.Set("method", "list")
	q.Set("parent_fid", dirID)
	q.Set("order_by", "3") // 3 = 按文件名，稳定顺序才能稳定翻页
	q.Set("desc", "1")
	q.Set("category", "")
	q.Set("source", "")
	q.Set("ex_source", "")
	q.Set("list_all", "0")
	q.Set("page_size", strconv.Itoa(limit))
	q.Set("page_index", strconv.Itoa(page))
	if err := c.request(ctx, http.MethodGet, "/file", q, &out); err != nil {
		return nil, 0, err
	}

	items := out.Files
	if skip > 0 {
		if skip >= len(items) {
			items = nil
		} else {
			items = items[skip:]
		}
	}
	entries := make([]pan.Entry, 0, len(items))
	for _, f := range items {
		entries = append(entries, pan.Entry{
			ID:       f.Fid,
			ParentID: f.ParentFid,
			Name:     f.Filename,
			IsDir:    f.IsDir == 1,
			Size:     f.Size,
			// PickCode 对夸克就是 fid：播放链路（play.Proxy）按它反查账号，
			// 再调 DownloadURL 换 302 直链。
			PickCode: f.Fid,
			ModTime:  unixMilli(f.UpdatedAt),
		})
	}
	return entries, int(out.TotalCount), nil
}

// DownloadURL 取播放直链。
//
// UA 不参与这里的请求：夸克的直链不按 UA 签名，而 API 侧又要求 UA 必须和
// 登录时那台「TV 设备」一致，所以这里固定用 TV UA，不把播放器的 UA 透传进来。
// （115 正好相反——那里空 UA 必须原样透传，见 driver115 的注释。）
func (c *Client) DownloadURL(ctx context.Context, pickCode, _ string) (string, error) {
	if strings.TrimSpace(pickCode) == "" {
		return "", pan.NewErr(pan.ErrNotFound, "取直链", "文件 id 为空", nil)
	}
	var out struct {
		Fid         string `json:"fid"`
		FileName    string `json:"file_name"`
		Size        int64  `json:"size"`
		DownloadURL string `json:"download_url"`
	}
	q := url.Values{}
	q.Set("method", "download")
	q.Set("group_by", "source")
	q.Set("fid", pickCode)
	q.Set("resolution", "low,normal,high,super,2k,4k")
	q.Set("support", "dolby_vision")
	if err := c.request(ctx, http.MethodGet, "/file", q, &out); err != nil {
		return "", err
	}
	if strings.TrimSpace(out.DownloadURL) == "" {
		return "", pan.NewErr(pan.ErrNotFound, "取直链",
			"夸克没给下载地址（文件可能已删除、违规，或该文件不支持 TV 端播放）", nil)
	}
	return out.DownloadURL, nil
}

// Stat 查单个条目。TV 接口没有「按 fid 查」的方法，只能列父目录再筛，
// 而 Driver 接口又不给父目录——所以如实返回不支持，调用方会退回用 id 本身。
func (c *Client) Stat(ctx context.Context, id string) (pan.Entry, error) {
	return pan.Entry{}, pan.NewErr(pan.ErrUnsupported, "查条目",
		"夸克 TV 接口不支持按 id 查条目（只能列目录）", nil)
}

func unsupported(op string) error {
	return pan.NewErr(pan.ErrUnsupported, op,
		"夸克 TV 接口只开放了列目录与下载，不能"+op+"；请用夸克 App 处理", nil)
}

// Ping 校验凭证。只读 user_info，没有副作用（不会踢掉其它设备）。
func (c *Client) Ping(ctx context.Context) error {
	q := url.Values{}
	q.Set("method", "user_info")
	if err := c.request(ctx, http.MethodGet, "/user", q, nil); err != nil {
		return err
	}
	return nil
}

// Nickname 顺手取一次昵称，给账号页显示用。取不到不算错。
func (c *Client) Nickname(ctx context.Context) string {
	var out struct {
		Nickname string `json:"nickname"`
		Nick     string `json:"nick_name"`
	}
	q := url.Values{}
	q.Set("method", "user_info")
	if err := c.request(ctx, http.MethodGet, "/user", q, &out); err != nil {
		return ""
	}
	if out.Nickname != "" {
		return out.Nickname
	}
	return out.Nick
}

// ==================== token 网关 ====================

type tokenData struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// exchange 拿 code 或 refresh_token 换一对新 token。
//
// 这个网关（api.extscreen.com）不校验签名头，只认 body 里的设备字段；
// 但 device_id 必须和 authorize 那次一模一样，否则换出来的 token 用不了。
func (c *Client) exchange(ctx context.Context, body map[string]string) (tokenData, error) {
	_, _, reqID := c.sign(http.MethodPost, "/token")
	body["req_id"] = reqID
	body["app_ver"] = appVer
	body["device_id"] = c.cred.DeviceID
	body["device_brand"] = deviceBrand
	body["platform"] = platform
	body["device_name"] = deviceName
	body["device_model"] = deviceModel
	body["build_device"] = buildDevice
	body["build_product"] = buildProduct
	body["device_gpu"] = deviceGpu
	body["activity_rect"] = activityRect
	body["channel"] = channel

	raw, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.codeBase+"/token", bytes.NewReader(raw))
	if err != nil {
		return tokenData{}, pan.NewErr(pan.ErrTransient, "换夸克令牌", "构造请求失败", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", ua)

	resp, err := c.send(req)
	if err != nil {
		return tokenData{}, err
	}
	var e env
	if err := json.Unmarshal(resp, &e); err != nil {
		return tokenData{}, pan.NewErr(pan.ErrTransient, "换夸克令牌", "网关返回的不是 JSON", err)
	}
	if e.Code != 200 && e.Code != 0 {
		msg := e.Message
		if msg == "" {
			msg = e.ErrorInfo
		}
		if strings.TrimSpace(msg) == "" {
			msg = fmt.Sprintf("网关返回 code=%d", e.Code)
		}
		kind := pan.ErrRejected
		if strings.Contains(msg, "过期") || strings.Contains(msg, "失效") || strings.Contains(msg, "登录") {
			kind = pan.ErrAuth
		}
		return tokenData{}, pan.NewErr(kind, "换夸克令牌", msg, nil)
	}
	var td tokenData
	if len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, &td); err != nil {
			return tokenData{}, pan.NewErr(pan.ErrTransient, "换夸克令牌", "解析 token 失败", err)
		}
	}
	if td.RefreshToken == "" {
		return tokenData{}, pan.NewErr(pan.ErrAuth, "换夸克令牌", "夸克没给 refresh_token，请重新扫码登录", nil)
	}
	return td, nil
}

// ==================== 小工具 ====================

func newDeviceID() string {
	sum := md5.Sum([]byte(time.Now().String() + strconv.FormatInt(time.Now().UnixNano(), 10)))
	return hex.EncodeToString(sum[:])
}

func unixMilli(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	// 夸克给的是毫秒；万一是秒级（旧接口），按 1e12 的界分一下。
	if ms < 1_000_000_000_000 {
		return time.Unix(ms, 0)
	}
	return time.UnixMilli(ms)
}
