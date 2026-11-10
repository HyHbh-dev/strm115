// Package quark 是夸克网盘「网页接口」的最小客户端，只做一件事：
// 把别人的分享链接转存进自己的网盘。
//
// 为什么不能只靠 internal/pan/driverquarktv：TV 开放接口只给了列目录和取直链，
// 没有分享转存（App 里那个「保存到网盘」走的是网页/PC 接口）。所以夸克这条链路
// 由两个包拼起来：
//
//	internal/quark          转存（网页接口 + Cookie）
//	internal/pan/driverquarktv  播放（TV 开放接口 + refresh_token，302 直链）
//
// 代价是这里需要一份网页 Cookie：其中 __puus 大约 3 小时就会轮换。
// 客户端会自动合并响应里新下发的 __puus/__pus，并由 httpapi 的保活循环
// 每 ~100 分钟主动续一次期（见 RefreshPuus），只要进程不长期停机就不用重贴；
// 真失效时保存动作会明确报「Cookie 失效」，界面上给一键重贴的入口。
//
// 接口（都是夸克网页自己用的，没有官方文档）：
//
//	POST /share/sharepage/token    用 pwd_id + passcode 换 stoken
//	GET  /share/sharepage/detail   列分享里的文件（拿 fid 与 share_fid_token）
//	POST /share/sharepage/save     转存到 to_pdir_fid
//	GET  /task                     轮询异步任务
package quark

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"strm115/internal/netproxy"
)

const (
	// DefaultBase 是网页/PC 接口根。
	DefaultBase = "https://drive-pc.quark.cn/1/clouddrive"

	// 网页端固定带的两个参数，缺了会被当成第三方调用。
	prParam = "ucpro"
	frParam = "pc"

	reqTimeout    = 20 * time.Second
	taskTimeout   = 3 * time.Minute
	taskPollEvery = 700 * time.Millisecond
)

var userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

var (
	// ErrCookie 是「没配网页 Cookie」或「Cookie 已失效」。界面据此提示重新粘贴。
	// 文案要说清下一步：夸克TV 扫码登录拿到的是 refresh_token，只能播放，
	// 转存/容量要的是另一份网页 Cookie —— 用户最容易在这里以为「凭据出错」。
	ErrCookie = errors.New("夸克网页 Cookie 未配置或已失效：到「账号 → 夸克 → 转存设置」扫码登录或手动粘贴（TV 扫码不能代替）")
	// ErrShareInvalid 是分享链接本身有问题：过期、被删、提取码不对。
	ErrShareInvalid = errors.New("夸克分享无效")
	// ErrNoRefresh 表示这次续期服务端没下发新的 __puus。不是故障：Cookie
	// 还能用，保活循环下一轮再试，界面不该把它显示成红色。
	ErrNoRefresh = errors.New("夸克这次没有下发新的 __puus，保持现有 Cookie")
)

// Share 是一条解析出来的夸克分享。
type Share struct {
	// PwdID 是分享 id，也就是 /s/ 后面那一段。
	PwdID string
	// Passcode 是提取码，没有就空。
	Passcode string
	URL      string
}

// ParseShare 从一段文本里认夸克分享。
//
// 认的形态：
//
//	https://pan.quark.cn/s/abcdef123456
//	pan.quark.cn/s/abcdef123456?pwd=1234
//	「复制这段内容后打开夸克网盘…」这类整段分享文案
//	提取码：1234（在同一段文本里）
func ParseShare(raw string) (Share, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return Share{}, false
	}
	// 统一去掉可能的 "复制…" 前后缀，只从里面捞链接。
	low := strings.ToLower(s)
	idx := strings.Index(low, "pan.quark.cn/s/")
	if idx < 0 {
		return Share{}, false
	}
	rest := s[idx:]
	// 截到空白或引号为止
	if end := strings.IndexAny(rest, " \t\r\n\"'<>）)，,"); end >= 0 {
		rest = rest[:end]
	}
	u := rest
	if !strings.HasPrefix(strings.ToLower(u), "http") {
		u = "https://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil {
		return Share{}, false
	}
	id := strings.Trim(strings.TrimPrefix(parsed.Path, "/s/"), "/")
	if id == "" {
		return Share{}, false
	}
	sh := Share{PwdID: id, URL: "https://pan.quark.cn/s/" + id}
	sh.Passcode = parsed.Query().Get("pwd")
	if sh.Passcode == "" {
		sh.Passcode = passcodeInText(s)
	}
	return sh, true
}

var passcodeRe = regexp.MustCompile(`(?:提取码|访问码|密码)\s*[:：]?\s*([0-9a-zA-Z]{4})`)

// passcodeInText 从分享文案里抠提取码。夸克的提取码是 4 位字母数字。
func passcodeInText(s string) string {
	if m := passcodeRe.FindStringSubmatch(s); len(m) == 2 {
		return m[1]
	}
	return ""
}

// Result 是一次转存的结果。
type Result struct {
	Title string // 分享标题（拿不到就退成第一个文件名）
	Files int    // 转存了几个顶层条目
	FIDs  []string
}

type Client struct {
	// cookie 会被响应里的 Set-Cookie 更新（__puus 轮换），也会被保活循环读取，
	// 而转存与 Ping 可能同时跑，所以加一把锁。
	mu     sync.RWMutex
	cookie string
	base   string
	hc     *http.Client

	// 转存偶尔会遇到「任务排队」，这里保证同一个账号的写操作一次只跑一个，
	// 多发几次只会让夸克的风控更早亮灯。
	sem chan struct{}
}

// New 用网页 Cookie 建客户端。base 留空用官方地址（测试会指到假服务器）。
func New(cookie string) *Client { return NewWithBase(cookie, "") }

func NewWithBase(cookie, base string) *Client {
	if strings.TrimSpace(base) == "" {
		base = DefaultBase
	}
	return &Client{
		cookie: strings.TrimSpace(cookie),
		base:   strings.TrimRight(base, "/"),
		hc:     &http.Client{Timeout: reqTimeout, Transport: &http.Transport{Proxy: netproxy.URL}},
		sem:    make(chan struct{}, 1),
	}
}

// Configured 报告有没有配 Cookie。没配时所有方法都会返回 ErrCookie。
func (c *Client) Configured() bool { return c.Cookie() != "" }

// Cookie 返回当前这份（可能已被响应中的 Set-Cookie 更新过的）Cookie。
func (c *Client) Cookie() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cookie
}

// Ping 校验 Cookie 是否还有效。只看会员信息，没有副作用。
func (c *Client) Ping(ctx context.Context) error {
	if !c.Configured() {
		return ErrCookie
	}
	var out struct {
		Nickname string `json:"nickname"`
	}
	if err := c.get(ctx, "/member", nil, &out); err != nil {
		return err
	}
	return nil
}

// Quota 读夸克网页端的存储用量（GET /member）。
//
// 这个端点的字段各家客户端版本不太一样，所以只按「键名像容量、值像字节数」
// 宽松地找，找不到就返回 0 —— 首页显示「—」，总比整张卡片报错好。
// 没配 Cookie 时直接返回 ErrCookie。
func (c *Client) Quota(ctx context.Context) (used, total uint64, err error) {
	if !c.Configured() {
		return 0, 0, ErrCookie
	}
	var data json.RawMessage
	if err := c.get(ctx, "/member", nil, &data); err != nil {
		return 0, 0, err
	}
	var v any
	if json.Unmarshal(data, &v) != nil {
		return 0, 0, nil
	}
	var numbers []struct {
		key string
		n   float64
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				if f, ok := val.(float64); ok {
					numbers = append(numbers, struct {
						key string
						n   float64
					}{k, f})
				} else {
					walk(val)
				}
			}
		case []any:
			for _, val := range t {
				walk(val)
			}
		}
	}
	walk(v)
	for _, it := range numbers {
		k, n := strings.ToLower(it.key), it.n
		if n < 1<<30 { // 小于 1GB 的多半不是容量（会员等级、天数…）
			continue
		}
		switch {
		case strings.Contains(k, "use") && used == 0:
			used = uint64(n)
		case (strings.Contains(k, "total") || strings.Contains(k, "capacity") || strings.Contains(k, "quota")) && total == 0:
			total = uint64(n)
		}
	}
	if used > total && total > 0 {
		total = used // all_total 有时只给「已用 + 剩余」里的一个，别画出 >100%
	}
	return used, total, nil
}

// SaveShare 把分享转存到 toDirID（留空 = 根目录）。
func (c *Client) SaveShare(ctx context.Context, raw, passcode, toDirID string) (Result, error) {
	if !c.Configured() {
		return Result{}, ErrCookie
	}
	sh, ok := ParseShare(raw)
	if !ok {
		return Result{}, fmt.Errorf("%w：没认出 pan.quark.cn/s/ 链接", ErrShareInvalid)
	}
	if strings.TrimSpace(passcode) != "" {
		sh.Passcode = strings.TrimSpace(passcode)
	}
	if strings.TrimSpace(toDirID) == "" {
		toDirID = "0"
	}

	// 串行：同账号同时只跑一个转存。夸克对并发写很敏感，而且并发的转存
	// 任务会互相抢改名，最后两份都变成「xxx(1)」。
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	defer func() { <-c.sem }()

	// 1) 换 stoken。提取码不对时这一步就会失败，报错信息夸克原话已经够用。
	var tok struct {
		Stoken string `json:"stoken"`
		Title  string `json:"title"`
	}
	body := map[string]any{"pwd_id": sh.PwdID, "passcode": sh.Passcode}
	if err := c.post(ctx, "/share/sharepage/token", body, &tok); err != nil {
		if strings.Contains(err.Error(), ErrShareInvalid.Error()) {
			if sh.Passcode == "" {
				return Result{}, fmt.Errorf("%w：可能需要提取码（分享文案里没找到）", ErrShareInvalid)
			}
			return Result{}, fmt.Errorf("%w：提取码不对或分享已失效", ErrShareInvalid)
		}
		return Result{}, err
	}
	if tok.Stoken == "" {
		return Result{}, fmt.Errorf("%w：夸克没给 stoken", ErrShareInvalid)
	}

	// 2) 列分享里的顶层条目，拿 fid 与 fid_token。
	var det struct {
		List []struct {
			Fid        string `json:"fid"`
			FileName   string `json:"file_name"`
			ShareToken string `json:"share_fid_token"`
			Dir        bool   `json:"dir"`
		} `json:"list"`
		Count int `json:"count"`
		Share struct {
			Title string `json:"title"`
		} `json:"share"`
	}
	q := url.Values{}
	q.Set("pwd_id", sh.PwdID)
	q.Set("stoken", tok.Stoken)
	q.Set("pdir_fid", "0")
	q.Set("force", "0")
	q.Set("_page", "1")
	q.Set("_size", "50")
	q.Set("_fetch_banner", "0")
	q.Set("_fetch_share", "1")
	q.Set("_fetch_total", "1")
	q.Set("_sort", "file_type:asc,updated_at:desc")
	if err := c.get(ctx, "/share/sharepage/detail", q, &det); err != nil {
		return Result{}, err
	}
	if len(det.List) == 0 {
		return Result{}, fmt.Errorf("%w：分享里没有文件", ErrShareInvalid)
	}
	fids := make([]string, 0, len(det.List))
	tokens := make([]string, 0, len(det.List))
	for _, it := range det.List {
		if it.Fid == "" {
			continue
		}
		fids = append(fids, it.Fid)
		tokens = append(tokens, it.ShareToken)
	}
	if len(fids) == 0 {
		return Result{}, fmt.Errorf("%w：分享里没有可转存的文件", ErrShareInvalid)
	}

	// 3) 转存。夸克是异步的，返回 task_id 后要自己轮询。
	var saved struct {
		TaskID string `json:"task_id"`
	}
	saveBody := map[string]any{
		"fid_list":       fids,
		"fid_token_list": tokens,
		"to_pdir_fid":    toDirID,
		"pwd_id":         sh.PwdID,
		"stoken":         tok.Stoken,
		"pdir_fid":       "0",
		"scene":          "link",
	}
	if err := c.post(ctx, "/share/sharepage/save", saveBody, &saved); err != nil {
		return Result{}, err
	}
	if saved.TaskID != "" {
		if err := c.waitTask(ctx, saved.TaskID); err != nil {
			return Result{}, err
		}
	}

	title := strings.TrimSpace(det.Share.Title)
	if title == "" && len(det.List) > 0 {
		title = det.List[0].FileName
	}
	return Result{Title: title, Files: len(fids), FIDs: fids}, nil
}

// waitTask 轮询异步任务直到完成。夸克偶尔会一直排队，所以有总超时。
func (c *Client) waitTask(ctx context.Context, taskID string) error {
	deadline := time.Now().Add(taskTimeout)
	for {
		if time.Now().After(deadline) {
			return errors.New("夸克转存任务超时（文件可能已经进去，去夸克 App 看一眼）")
		}
		var out struct {
			Status     int    `json:"status"` // 0 等待 1 进行 2 完成
			TaskStatus int    `json:"task_status"`
			Message    string `json:"message"`
		}
		q := url.Values{}
		q.Set("task_id", taskID)
		q.Set("retry_index", "0")
		if err := c.get(ctx, "/task", q, &out); err != nil {
			return err
		}
		// 夸克的任务状态只有 status 可靠：实测完成时回
		//   {"status":2,"task_status":null,"message":"ok"}
		// 以前要求 status 和 task_status 同时为 2，于是每一次成功转存都要
		// 干等 3 分钟再报「超时」——文件其实早进网盘了，用户看到的是
		// 「转存失败」，回过头发现东西在，没人能判断到底成没成。
		if out.Status == 2 {
			// 结束态里 message 不是 ok 才算失败（失败时夸克会把原因写在这）
			if msg := strings.TrimSpace(out.Message); msg != "" && !strings.EqualFold(msg, "ok") {
				return fmt.Errorf("夸克转存失败：%s", msg)
			}
			return nil
		}
		// 有的接口版本会给 task_status，给到 2 同样算完成。
		if out.TaskStatus == 2 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(taskPollEvery):
		}
	}
}

// ==================== HTTP ====================

type envelope struct {
	Status  int             `json:"status"`
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) get(ctx context.Context, pathname string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, pathname, q, nil, out)
}

func (c *Client) post(ctx context.Context, pathname string, body any, out any) error {
	return c.do(ctx, http.MethodPost, pathname, nil, body, out)
}

func (c *Client) do(ctx context.Context, method, pathname string, q url.Values, body, out any) error {
	return c.doWithCookie(ctx, method, pathname, q, body, out, c.Cookie())
}

// doWithCookie 用指定的 Cookie 发一次请求。保活要故意不带 __puus，
// 所以把「用哪份 Cookie」做成参数；响应里新下发的会话字段仍会合并回 c.cookie。
func (c *Client) doWithCookie(ctx context.Context, method, pathname string, q url.Values,
	body, out any, cookie string) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("pr", prParam)
	q.Set("fr", frParam)
	u := c.base + pathname + "?" + q.Encode()

	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://pan.quark.cn/")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("连接夸克失败: %w", err)
	}
	defer res.Body.Close()
	// 夸克会在任意响应里轮换 __puus / __pus，收下就能让会话一直活着。
	c.mergeResponseCookies(res)
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("读夸克响应失败: %w", err)
	}
	if res.StatusCode == http.StatusTooManyRequests {
		return errors.New("夸克限流了（HTTP 429），过一会儿再试")
	}
	var e envelope
	if err := json.Unmarshal(raw, &e); err != nil {
		return fmt.Errorf("夸克返回的不是 JSON（HTTP %d，可能被风控拦了）", res.StatusCode)
	}
	// 夸克成功时 status=200 且 code=0；失败时 status 仍是 200、只有 code/message 变。
	if res.StatusCode != http.StatusOK || e.Code != 0 || (e.Status != 0 && e.Status != http.StatusOK) {
		msg := strings.TrimSpace(e.Message)
		if msg == "" {
			msg = fmt.Sprintf("夸克返回 status=%d code=%d", e.Status, e.Code)
		}
		return classify(msg)
	}
	if out != nil && len(e.Data) > 0 {
		if err := json.Unmarshal(e.Data, out); err != nil {
			return fmt.Errorf("解析夸克返回失败: %w", err)
		}
	}
	return nil
}

// classify 把夸克的中文报错翻译成两个哨兵：Cookie 失效 / 分享无效。
// 其余原样返回给用户看 —— 夸克的原话通常比我们的翻译更有用。
func classify(msg string) error {
	switch {
	case strings.Contains(msg, "登录"), strings.Contains(msg, "未授权"),
		strings.Contains(msg, "账号异常"), strings.Contains(msg, "身份"):
		return fmt.Errorf("%w：%s", ErrCookie, msg)
	case strings.Contains(msg, "分享"), strings.Contains(msg, "提取码"),
		strings.Contains(msg, "过期"), strings.Contains(msg, "失效"),
		strings.Contains(msg, "违规"), strings.Contains(msg, "不存在"):
		return fmt.Errorf("%w：%s", ErrShareInvalid, msg)
	default:
		return errors.New(msg)
	}
}

// SortFIDs 让结果稳定，便于日志比对（夸克返回顺序不保证）。
func SortFIDs(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// ==================== Cookie 保活 ====================

// parseCookies 把 "a=1; b=2" 拆成 map。浏览器粘过来的 Cookie 常带空格、
// 重复键和空段，这里顺手归一化。
func parseCookies(raw string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(raw, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		if k != "" {
			out[k] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func joinCookies(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if m[k] != "" {
			parts = append(parts, k+"="+m[k])
		}
	}
	return strings.Join(parts, "; ")
}

// mergeResponseCookies 把响应 Set-Cookie 里的会话字段并回 c.cookie。
//
// 只认 __puus / __pus：其它字段（__uid / __kp 之类）是身份信息，
// 服务端偶尔会下发空值，覆盖掉反而会把账号弄成未登录。
func (c *Client) mergeResponseCookies(res *http.Response) {
	if res == nil {
		return
	}
	updates := map[string]string{}
	for _, sc := range res.Cookies() {
		if (sc.Name == "__puus" || sc.Name == "__pus") && sc.Value != "" {
			updates[sc.Name] = sc.Value
		}
	}
	if len(updates) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	m := parseCookies(c.cookie)
	for k, v := range updates {
		m[k] = v
	}
	c.cookie = joinCookies(m)
}

// RefreshPuus 主动续一次期，返回更新后的完整 Cookie。
//
// 夸克只在「请求里没有 __puus」时才重新下发（见 AlistGo/alist#830），
// 所以这里故意删掉它去请求 /config —— 一个只读、无副作用的端点。
// 服务端下发的新值会被 mergeResponseCookies 合并回来。
// 失败时 c.cookie 保持原样（其它并发请求可能已经带来过新值），调用方
// 不要把旧 Cookie 写回去覆盖它。
func (c *Client) RefreshPuus(ctx context.Context) (string, error) {
	old := c.Cookie()
	if old == "" {
		return "", ErrCookie
	}
	m := parseCookies(old)
	delete(m, "__puus")
	stripped := joinCookies(m)
	if err := c.doWithCookie(ctx, http.MethodGet, "/config", nil, nil, nil, stripped); err != nil {
		return "", fmt.Errorf("夸克 Cookie 续期失败: %w", err)
	}
	now := c.Cookie()
	if v := parseCookies(now)["__puus"]; v == "" || v == parseCookies(old)["__puus"] {
		return "", ErrNoRefresh
	}
	return now, nil
}
