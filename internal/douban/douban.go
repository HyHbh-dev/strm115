// Package douban 是豆瓣电影网页接口的最小客户端，只服务三件事：
// 「发现与订阅」的搜索候选、播出日历里的国产剧与国内档期、详情页的补充信息。
//
// 为什么需要它：国产剧和国内上映的片子在 TMDB 上常常缺集数、缺档期，
// 海报也是英文版的；豆瓣的「更新至第 N 集 / 上映日期」才是准的。所以
// 日历里港台与国产条目走豆瓣，欧美剧照旧走 TMDB，两边谁有就用谁。
//
// 三个端点（都是网页自己在用的 JSON 接口，没有官方 API）：
//
//	/j/subject_suggest                搜索建议，出片名 + 年份 + 海报 + id
//	/rexxar/api/v2/{movie|tv}/{id}    详情，出简介、评分、集数、上映日期
//	/rexxar/api/v2/subject_collection 榜单：正在上映 / 即将上映 / 国产热播
//
// 两条必须遵守的规矩，否则一律 418（豆瓣的风控页）：
//   - 带浏览器 UA；只带 Go 默认 UA 会被直接拒。
//   - 带对应站点自己的 Referer（rexxar 走 m.douban.com，搜索走 movie.douban.com）。
//
// 客户端自己做了三件降噪的事：串行 + 最小间隔（豆瓣对突发很敏感）、
// 按 URL 缓存、失败只返回错误由上层降级。豆瓣挂了不能让日历整页变空。
package douban

import (
	"context"
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
)

const (
	// DefaultBase 是 rexxar 接口根（手机版页面用的那套）。
	DefaultBase = "https://m.douban.com"
	// DefaultWebBase 是老的 /j/ 接口根（搜索建议、分类探索）。
	DefaultWebBase = "https://movie.douban.com"

	// minInterval 是两次请求之间的最小间隔。豆瓣对突发请求很敏感，
	// 一串并发过去会直接吃 418，之后几分钟连正常请求也进不来。
	minInterval = 200 * time.Millisecond
	// reqTimeout 单次请求上限：豆瓣偶尔会挂住，超过这个就别等了。
	reqTimeout = 12 * time.Second

	searchTTL     = 10 * time.Minute
	collectionTTL = 30 * time.Minute
	detailTTL     = 6 * time.Hour

	// Attribution 是界面上的数据来源署名。
	Attribution    = "国内档期与剧集进度来自豆瓣"
	AttributionURL = "https://movie.douban.com"

	// MaxPageSize 一页最多取多少条。豆瓣自己也是 20 一页。
	MaxPageSize = 20
)

// 榜单 key：这些是豆瓣网页内部用的集合名，不能用别的名字去猜。
const (
	CollMovieShowing = "movie_showing"    // 正在上映
	CollMovieSoon    = "movie_soon"       // 即将上映
	CollMovieTop250  = "movie_top250"     // Top 250
	CollMovieHot     = "movie_high_score" // 高分电影
	CollTVHot        = "tv_hot"           // 正在热播的剧
	CollTVDomestic   = "tv_domestic"      // 国产剧（含港台）
	CollTVVariety    = "tv_variety_show"  // 综艺
)

var userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

// flexInt 兼容豆瓣把同一个字段写成数字或字符串两种形态。
//
// 这不是洁癖：搜索接口的 id 是字符串（"35575567"），榜单和详情里也是字符串，
// 但年份有时是数字。写死 int 会让整条响应解析失败，而错误信息只会说
// 「cannot unmarshal string into Go struct field」，排查时完全看不出是哪个字段。
type flexInt int

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		// 只保留小数部分之前的数字（"8.0" 这种评分写法不会走到这里，兜个底）
		if i := strings.IndexByte(s, '.'); i > 0 {
			if m, e := strconv.Atoi(s[:i]); e == nil {
				*f = flexInt(m)
				return nil
			}
		}
		*f = 0
		return nil
	}
	*f = flexInt(n)
	return nil
}

// Item 是归一化后的一条豆瓣条目。剧集和电影共用一个结构，用 Kind 区分。
type Item struct {
	ID       int     `json:"id"`
	Kind     string  `json:"kind"` // movie / tv
	Title    string  `json:"title"`
	Original string  `json:"original,omitempty"`
	Year     int     `json:"year,omitempty"`
	Poster   string  `json:"poster,omitempty"`
	Rating   float64 `json:"rating,omitempty"`
	// Episodes 是剧集总集数；EpisodesInfo 是豆瓣原话「更新至28集」，
	// 两者都有才说明在追更（总集数已知、还没更完）。
	Episodes     int    `json:"episodes,omitempty"`
	EpisodesInfo string `json:"episodes_info,omitempty"`
	// ReleaseDate 统一成 YYYY-MM-DD（只知道月日时按今年/明年补全）；
	// ReleaseText 是豆瓣原话，日历上照原样显示更自然。
	ReleaseDate  string `json:"release_date,omitempty"`
	ReleaseText  string `json:"release_text,omitempty"`
	CardSubtitle string `json:"card_subtitle,omitempty"`
	Intro        string `json:"intro,omitempty"`
	URL          string `json:"url,omitempty"`
}

// Collection 是一个榜单的一页。
type Collection struct {
	Key   string `json:"key"`
	Total int    `json:"total"`
	Items []Item `json:"items"`
}

// Client 是豆瓣客户端。零值不可用，用 New。
type Client struct {
	base   string // rexxar 根
	webAny string // 老接口根
	hc     *http.Client

	mu      sync.Mutex
	lastReq time.Time
	cache   map[string]cacheEntry
}

type cacheEntry struct {
	body []byte
	at   time.Time
	ttl  time.Duration
}

// New 建一个客户端。proxy 为空表示直连。
func New(proxy string) *Client {
	return NewWithBases(proxy, "", "")
}

// NewWithBases 同上，但可以换两个接口根 —— 给自建反代和测试用。
//
// 豆瓣在境外访问不稳，将来把它也套到反代后面时，两个根都指到反代上：
// 反代按路径分流（/rexxar/... 与 /j/...），所以只配一个地址就够。
// 传空串 = 用官方地址。
func NewWithBases(proxy, rexxar, web string) *Client {
	c := &Client{
		base: DefaultBase, webAny: DefaultWebBase,
		cache: map[string]cacheEntry{},
	}
	tr := &http.Transport{
		Proxy:               netproxy.URL,
		MaxIdleConns:        8,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	if p := strings.TrimSpace(proxy); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	c.hc = &http.Client{Transport: tr, Timeout: reqTimeout}
	c.setBases(rexxar, web)
	return c
}

// 测试用：把两个根都指到 httptest 服务器上。
func (c *Client) setBases(rexxar, web string) {
	if rexxar != "" {
		c.base = strings.TrimRight(rexxar, "/")
	}
	if web != "" {
		c.webAny = strings.TrimRight(web, "/")
	}
}

// request 发一个 GET，带节流、缓存和「被风控」的明确报错。
//
// 缓存键就是完整 URL：豆瓣的参数顺序是固定的（我们自己拼），不用归一化。
func (c *Client) request(ctx context.Context, rawURL, referer string, ttl time.Duration) ([]byte, error) {
	c.mu.Lock()
	if e, ok := c.cache[rawURL]; ok && time.Since(e.at) < e.ttl {
		c.mu.Unlock()
		return e.body, nil
	}
	// 串行化 + 最小间隔：并发打过去就是 418。
	if wait := minInterval - time.Since(c.lastReq); wait > 0 {
		c.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		c.mu.Lock()
	}
	c.lastReq = time.Now()
	c.mu.Unlock()

	rctx, cancel := context.WithTimeout(ctx, reqTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", referer)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9")

	res, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	switch res.StatusCode {
	case http.StatusOK:
		// 继续
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusTooManyRequests, http.StatusForbidden, 418:
		return nil, fmt.Errorf("豆瓣限流了（HTTP %d），过一会儿再试", res.StatusCode)
	default:
		return nil, fmt.Errorf("豆瓣返回 HTTP %d", res.StatusCode)
	}

	c.mu.Lock()
	// 顺手清一层过期项，长跑进程里这张表不至于只涨不落。
	if len(c.cache) > 256 {
		for k, e := range c.cache {
			if time.Since(e.at) > e.ttl {
				delete(c.cache, k)
			}
		}
	}
	c.cache[rawURL] = cacheEntry{body: body, at: time.Now(), ttl: ttl}
	c.mu.Unlock()
	return body, nil
}

// ErrNotFound 表示豆瓣明确说这条不存在（详情被删、id 写错）。
var ErrNotFound = errors.New("豆瓣没有这个条目")

// Search 按片名搜豆瓣，返回候选清单。
//
// 走 /j/subject_suggest：它只回片名、年份、海报、id，不抓详情，快。
// 类型判断靠 episode 字段：有集数的是剧，否则按 type 走 —— 豆瓣的
// type 字段对剧集经常也写 movie，不能全信。
func (c *Client) Search(ctx context.Context, q string, limit int) ([]Item, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	if limit <= 0 || limit > MaxPageSize {
		limit = MaxPageSize
	}
	u := c.webAny + "/j/subject_suggest?" + url.Values{"q": {q}}.Encode()
	body, err := c.request(ctx, u, c.webAny+"/", searchTTL)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		ID       flexInt `json:"id"`
		Title    string  `json:"title"`
		SubTitle string  `json:"sub_title"`
		Year     flexInt `json:"year"`
		Type     string  `json:"type"`
		Episode  flexInt `json:"episode"`
		Img      string  `json:"img"`
		URL      string  `json:"url"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析豆瓣搜索失败: %w", err)
	}
	out := make([]Item, 0, len(raw))
	for _, r := range raw {
		id := int(r.ID)
		if id <= 0 {
			continue
		}
		kind := "movie"
		if r.Episode > 0 || r.Type == "tv" {
			kind = "tv"
		}
		out = append(out, Item{
			ID: id, Kind: kind,
			Title: strings.TrimSpace(r.Title), Original: strings.TrimSpace(r.SubTitle),
			Year: int(r.Year), Poster: httpsUp(r.Img),
			URL: subjectURL(kind, id),
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Detail 取一部作品的全量信息。
//
// kind 猜错是常态（搜索接口的类型对剧集不可靠），所以 404 时自动换另一个
// 端点再试一次，返回的 Kind 一定是真实类型。
func (c *Client) Detail(ctx context.Context, kind string, id int) (*Item, error) {
	if id <= 0 {
		return nil, fmt.Errorf("豆瓣 id 不合法: %d", id)
	}
	try := []string{"movie", "tv"}
	if kind == "tv" {
		try = []string{"tv", "movie"}
	}
	var lastErr error
	for _, k := range try {
		u := fmt.Sprintf("%s/rexxar/api/v2/%s/%d", c.base, k, id)
		body, err := c.request(ctx, u, c.base+"/"+k+"/", detailTTL)
		if errors.Is(err, ErrNotFound) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, err
		}
		it, err := parseDetail(body, k, id)
		if err != nil {
			return nil, err
		}
		return it, nil
	}
	if lastErr == nil {
		lastErr = ErrNotFound
	}
	return nil, lastErr
}

// Collection 取一个榜单的一页。start 从 0 起，count 上限 MaxPageSize。
func (c *Client) Collection(ctx context.Context, key string, start, count int) (*Collection, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, errors.New("缺少榜单名")
	}
	if count <= 0 || count > MaxPageSize {
		count = MaxPageSize
	}
	if start < 0 {
		start = 0
	}
	u := fmt.Sprintf("%s/rexxar/api/v2/subject_collection/%s/items?%s",
		c.base, url.PathEscape(key),
		url.Values{"start": {strconv.Itoa(start)}, "count": {strconv.Itoa(count)}}.Encode())
	body, err := c.request(ctx, u, c.base+"/", collectionTTL)
	if err != nil {
		return nil, err
	}
	return parseCollection(body, key)
}

// ==================== 解析 ====================

func parseCollection(body []byte, key string) (*Collection, error) {
	var raw struct {
		Total int `json:"total"`
		Items []struct {
			ID           flexInt `json:"id"`
			Title        string  `json:"title"`
			Type         string  `json:"type"`
			Year         flexInt `json:"year"`
			EpisodesInfo string  `json:"episodes_info"`
			CardSubtitle string  `json:"card_subtitle"`
			Rating       *struct {
				Value float64 `json:"value"`
			} `json:"rating"`
			Cover *struct {
				URL string `json:"url"`
			} `json:"cover"`
			Pic *struct {
				Large string `json:"large"`
			} `json:"pic"`
			// 即将上映用这两个：11.10 / 11月10日
			ReleaseDate string `json:"release_date"`
			RecentDate  string `json:"most_recent_release_date"`
		} `json:"subject_collection_items"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析豆瓣榜单失败: %w", err)
	}
	out := &Collection{Key: key, Total: raw.Total, Items: make([]Item, 0, len(raw.Items))}
	for _, r := range raw.Items {
		if r.ID <= 0 {
			continue
		}
		kind := "movie"
		if r.Type == "tv" {
			kind = "tv"
		}
		poster := ""
		if r.Cover != nil {
			poster = httpsUp(r.Cover.URL)
		}
		if poster == "" && r.Pic != nil {
			poster = httpsUp(r.Pic.Large)
		}
		it := Item{
			ID: int(r.ID), Kind: kind,
			Title: strings.TrimSpace(r.Title), Year: int(r.Year),
			Poster: poster, EpisodesInfo: strings.TrimSpace(r.EpisodesInfo),
			CardSubtitle: strings.TrimSpace(r.CardSubtitle),
			ReleaseText:  firstNonEmpty(strings.TrimSpace(r.RecentDate), strings.TrimSpace(r.ReleaseDate)),
			URL:          subjectURL(kind, int(r.ID)),
		}
		if r.Rating != nil {
			it.Rating = r.Rating.Value
		}
		it.ReleaseDate = parseRelease(it.ReleaseText, it.Year, time.Now())
		out.Items = append(out.Items, it)
	}
	return out, nil
}

// parseDetail 把 rexxar 的详情响应归一化。
func parseDetail(body []byte, kind string, id int) (*Item, error) {
	var raw struct {
		ID           flexInt `json:"id"`
		Title        string  `json:"title"`
		Original     string  `json:"original_title"`
		Year         flexInt `json:"year"`
		Intro        string  `json:"intro"`
		Episodes     flexInt `json:"episodes_count"`
		EpisodesInfo string  `json:"episodes_info"`
		CardSubtitle string  `json:"card_subtitle"`
		URL          string  `json:"url"`
		CoverURL     string  `json:"cover_url"`
		Cover        *struct {
			URL string `json:"url"`
		} `json:"cover"`
		Pic *struct {
			Large string `json:"large"`
		} `json:"pic"`
		Rating *struct {
			Value float64 `json:"value"`
		} `json:"rating"`
		Pubdate    []string `json:"pubdate"`
		ReleaseDay string   `json:"release_date"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析豆瓣详情失败: %w", err)
	}
	poster := httpsUp(raw.CoverURL)
	if poster == "" && raw.Cover != nil {
		poster = httpsUp(raw.Cover.URL)
	}
	if poster == "" && raw.Pic != nil {
		poster = httpsUp(raw.Pic.Large)
	}
	it := &Item{
		ID: id, Kind: kind,
		Title: strings.TrimSpace(raw.Title), Original: strings.TrimSpace(raw.Original),
		Year: int(raw.Year), Poster: poster,
		Episodes: int(raw.Episodes), EpisodesInfo: strings.TrimSpace(raw.EpisodesInfo),
		CardSubtitle: strings.TrimSpace(raw.CardSubtitle),
		Intro:        strings.TrimSpace(raw.Intro),
		URL:          firstNonEmpty(strings.TrimSpace(raw.URL), subjectURL(kind, id)),
	}
	if raw.ID > 0 {
		it.ID = int(raw.ID)
	}
	if raw.Rating != nil {
		it.Rating = raw.Rating.Value
	}
	// pubdate 是「2023-04-22(中国大陆)」这种，第一条就是主要上映地。
	for _, p := range raw.Pubdate {
		if d := parseRelease(p, it.Year, time.Now()); d != "" {
			it.ReleaseDate, it.ReleaseText = d, strings.TrimSpace(p)
			break
		}
	}
	if it.ReleaseDate == "" {
		if d := parseRelease(raw.ReleaseDay, it.Year, time.Now()); d != "" {
			it.ReleaseDate, it.ReleaseText = d, strings.TrimSpace(raw.ReleaseDay)
		}
	}
	return it, nil
}

// releaseLayouts 是豆瓣出现过的几种日期写法。
var releaseLayouts = []string{"2006-01-02", "2006-1-2", "2006/01/02"}

// parseRelease 把豆瓣的日期写法统一成 YYYY-MM-DD，认不出来返回空串。
//
//	"2023-04-22(中国大陆)" → 2023-04-22
//	"11.10" / "11月10日"   → 今年的 11-10；这个日子已经过去两个多月就当下一年
//	                          （「即将上映」列表里跨年很常见）
//
// 猜错年份比留空更糟：日历会把一部片子放到错误的周里，用户找不到就以为
// 功能坏了。所以只在「月.日」这种明确是档期预告的写法上猜，别的不猜。
func parseRelease(raw string, year int, now time.Time) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// 带年份的写法：截前 10 个字符交给 time.Parse 验一遍。
	for _, l := range releaseLayouts {
		if len(s) >= 8 {
			head := s
			if len(head) > 10 {
				head = head[:10]
			}
			if t, err := time.Parse(l, head); err == nil {
				return t.Format("2006-01-02")
			}
		}
	}
	// 「11.10」「11月10日」：只有月和日。
	mon, day := monthDay(s)
	if mon <= 0 || day <= 0 {
		return ""
	}
	y := now.Year()
	if year > 0 && year >= y-1 && year <= y+2 {
		// 榜单里同时给了作品年份时优先它：跨年档期（12 月上映的次年片子）
		// 用作品年份比用「今年」准。
		y = year
	}
	d := time.Date(y, time.Month(mon), day, 0, 0, 0, 0, time.UTC)
	// 只给月日、又已经过去很久 —— 说明这是明年的档期。
	if d.Before(now.AddDate(0, -2, 0)) {
		d = d.AddDate(1, 0, 0)
	}
	return d.Format("2006-01-02")
}

// monthDay 从「11.10」「11月10日」「11-10」里取月和日，取不到返回 0,0。
func monthDay(s string) (int, int) {
	s = strings.TrimSuffix(strings.TrimSpace(s), "日")
	var a, b string
	switch {
	case strings.Contains(s, "月"):
		parts := strings.SplitN(s, "月", 2)
		if len(parts) != 2 {
			return 0, 0
		}
		a, b = parts[0], parts[1]
	case strings.Contains(s, "."):
		parts := strings.SplitN(s, ".", 2)
		a, b = parts[0], parts[1]
	case strings.Contains(s, "-") && len(s) <= 5:
		parts := strings.SplitN(s, "-", 2)
		a, b = parts[0], parts[1]
	default:
		return 0, 0
	}
	mon, day := atoi(a), atoi(b)
	if mon < 1 || mon > 12 || day < 1 || day > 31 {
		return 0, 0
	}
	return mon, day
}

// subjectURL 是给人点开的网页地址。电影和剧集用的是同一套 /subject/{id}/，
// 只有 rexxar 接口才分 /movie 和 /tv。
func subjectURL(_ string, id int) string {
	return fmt.Sprintf("https://movie.douban.com/subject/%d/", id)
}

// httpsUp 把豆瓣的老 http 图床换成 https：网页是 https 的，混着 http
// 图片会被浏览器当混合内容拦掉。
func httpsUp(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "http://") {
		return "https://" + strings.TrimPrefix(s, "http://")
	}
	return s
}

func atoi(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0
		}
		n = n*10 + int(r-'0')
		if n > 1<<30 {
			return 0
		}
	}
	return n
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
