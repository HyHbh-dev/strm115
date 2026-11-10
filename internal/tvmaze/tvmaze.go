// Package tvmaze 是 TVMaze 公共 API 的最小客户端，只服务于播出日历。
//
// 为什么需要它：TheTVDB v4 的 API Key 已改成项目级授权（商业签约，或要求
// 每个终端用户持订阅 PIN），个人项目拿不到免费 Key。TVMaze 的公共 API
// 免费、无需鉴权，且 /schedule 直接给出「哪天、几点、在哪个电视网播哪一集」，
// 正好补 TMDB 只有 air_date、没有时刻的缺口。
//
// 数据许可 CC BY-SA：调用方必须在界面上署名并链接回 tvmaze.com
// （calendar.Sources.Attribution 就是为此存在的）。
//
// 两个端点的响应形状不一样，别写成一个 struct：
//   - /schedule      → 顶层 "show"
//   - /schedule/web  → HAL 的 "_embedded.show"（全球流媒体剧在这里）
//
// 限流是「每 10 秒至少 20 次」，整周 7 天 × 2 端点 = 14 次请求，
// 用低并发 + 429 退避 + 按天缓存压住，失败一律由上层静默降级。
package tvmaze

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
	// DefaultBase 是公共 API 根。自建反代时填「反代根」，后面直接拼 /schedule。
	DefaultBase = "https://api.tvmaze.com"
	// Attribution / AttributionURL 是 CC BY-SA 要求的署名。
	Attribution    = "播出时刻与电视网数据来自 TVMaze（CC BY-SA）"
	AttributionURL = "https://www.tvmaze.com"

	dayCacheTTL  = 30 * time.Minute
	requestLimit = 3 // 并发请求数，远低于官方限流
	maxAttempts  = 3 // 429 / 5xx 最多试这么多次
)

var userAgent = "strm115-calendar/1.0 (+https://www.tvmaze.com)"

// Episode 是归一化后的一集。Channel 已经把 network / webChannel 合并。
type Episode struct {
	Date     string // YYYY-MM-DD，播出地日期
	AirTime  string // HH:MM，播出地当地时刻；可能为空
	AirStamp string // RFC3339 UTC，可用于以后换算北京时间
	Title    string // 集标题
	Season   int
	Number   int
	Runtime  int
	ShowID   int
	ShowName string
	Channel  string // NBC / Netflix …
	Timezone string // America/New_York …
	TVDBID   int
	IMDbID   string
	Web      bool // 来自 /schedule/web
	ShowKey  string
}

// rawShow 是 /schedule 里顶层 show 与 _embedded.show 共用的结构。
type rawShow struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Network *struct {
		Name    string `json:"name"`
		Country *struct {
			Timezone string `json:"timezone"`
		} `json:"country"`
	} `json:"network"`
	WebChannel *struct {
		Name string `json:"name"`
	} `json:"webChannel"`
	Externals struct {
		TVRage  *int    `json:"tvrage"`
		TheTVDB *int    `json:"thetvdb"`
		IMDb    *string `json:"imdb"`
	} `json:"externals"`
}

// rawEpisode 同时兼容两种端点：show 与 _embedded.show 只会有其中一个。
type rawEpisode struct {
	ID       int      `json:"id"`
	Name     string   `json:"name"`
	AirDate  string   `json:"airdate"`
	AirTime  string   `json:"airtime"`
	AirStamp string   `json:"airstamp"`
	Season   int      `json:"season"`
	Number   int      `json:"number"`
	Runtime  int      `json:"runtime"`
	Show     *rawShow `json:"show"`
	Embedded struct {
		Show *rawShow `json:"show"`
	} `json:"_embedded"`
}

func (e rawEpisode) show() *rawShow {
	if e.Show != nil {
		return e.Show
	}
	return e.Embedded.Show
}

func (e rawEpisode) normalize(web bool) Episode {
	out := Episode{
		Date:     e.AirDate,
		AirTime:  strings.TrimSpace(e.AirTime),
		AirStamp: e.AirStamp,
		Title:    e.Name,
		Season:   e.Season,
		Number:   e.Number,
		Runtime:  e.Runtime,
		Web:      web,
	}
	if s := e.show(); s != nil {
		out.ShowID = s.ID
		out.ShowName = s.Name
		out.ShowKey = NormalizeShow(s.Name)
		if s.Network != nil {
			out.Channel = strings.TrimSpace(s.Network.Name)
			if s.Network.Country != nil {
				out.Timezone = s.Network.Country.Timezone
			}
		}
		if out.Channel == "" && s.WebChannel != nil {
			out.Channel = strings.TrimSpace(s.WebChannel.Name)
		}
		if s.Externals.TheTVDB != nil {
			out.TVDBID = *s.Externals.TheTVDB
		}
		if s.Externals.IMDb != nil {
			out.IMDbID = strings.TrimSpace(*s.Externals.IMDb)
		}
	}
	return out
}

// NormalizeShow 把剧名收敛成对齐用的键：小写、只留字母数字（含中文）。
// "Marvel's Daredevil" 和 "Marvels Daredevil" 应该算同一部。
// 导出给 calendar 用：TMDB 的剧名要和 TVMaze 的 ShowKey 走同一套归一化。
func NormalizeShow(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r >= 0x4e00 && r <= 0x9fff {
			b.WriteRune(r)
		}
	}
	return b.String()
}

type cachedDay struct {
	at  time.Time
	eps []Episode
}

// Client 是带按天缓存的客户端。零值不可用，请用 New。
type Client struct {
	Base string
	hc   *http.Client

	mu    sync.Mutex
	cache map[string]cachedDay
	now   func() time.Time
}

// New 构造客户端。base 留空用官方地址；proxy 留空直连，格式不对就忽略。
func New(base, proxy string) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		base = DefaultBase
	}
	hc := &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: netproxy.URL}}
	if u, err := url.Parse(strings.TrimSpace(proxy)); err == nil && u.Host != "" &&
		(u.Scheme == "http" || u.Scheme == "https") {
		hc.Transport = &http.Transport{Proxy: http.ProxyURL(u)}
	}
	return &Client{Base: base, hc: hc, cache: map[string]cachedDay{}, now: time.Now}
}

// Endpoint 返回这一轮真正生效的接口根，供设置页回显。
func (c *Client) Endpoint() string {
	if c == nil || c.Base == "" {
		return DefaultBase
	}
	return c.Base
}

func (c *Client) cacheKey(date string, web bool) string {
	return date + "|" + strconv.FormatBool(web)
}

func (c *Client) getCached(key string) ([]Episode, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.cache[key]
	if !ok || c.now().Sub(e.at) >= dayCacheTTL {
		return nil, false
	}
	return e.eps, true
}

func (c *Client) putCached(key string, eps []Episode) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// 缓存只按天存，条目数有限；真被塞爆了就整体清掉，比逐条淘汰简单。
	if len(c.cache) > 120 {
		c.cache = map[string]cachedDay{}
	}
	c.cache[key] = cachedDay{at: c.now(), eps: eps}
}

// Schedule 取某一天、某一个端点的全部集。country 留空按官方默认 US。
func (c *Client) Schedule(ctx context.Context, date, country string, web bool) ([]Episode, error) {
	date = strings.TrimSpace(date)
	if date == "" {
		return nil, errors.New("日期不能为空")
	}
	key := c.cacheKey(date, web)
	if eps, ok := c.getCached(key); ok {
		return eps, nil
	}

	v := url.Values{}
	v.Set("date", date)
	if !web {
		if c := strings.TrimSpace(country); c != "" {
			v.Set("country", c)
		}
	}
	path := "/schedule"
	if web {
		path = "/schedule/web"
	}
	endpoint := c.Endpoint() + path + "?" + v.Encode()

	var body []byte
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", userAgent)
		resp, err := c.hc.Do(req)
		if err != nil {
			return nil, fmt.Errorf("请求 TVMaze 失败：%w", err)
		}
		body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("读取 TVMaze 响应失败：%w", err)
		}
		if resp.StatusCode == http.StatusOK {
			break
		}
		retryable := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
		if !retryable || attempt == maxAttempts {
			return nil, fmt.Errorf("TVMaze 返回 HTTP %d", resp.StatusCode)
		}
		// 官方建议 429 后短暂停一下再试；尊重 ctx，别把日历请求卡死。
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 1200 * time.Millisecond):
		}
	}

	var raw []rawEpisode
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 TVMaze 响应失败：%w", err)
	}
	eps := make([]Episode, 0, len(raw))
	for _, e := range raw {
		if strings.TrimSpace(e.AirDate) == "" {
			continue
		}
		eps = append(eps, e.normalize(web))
	}
	c.putCached(key, eps)
	return eps, nil
}

// Week 取整周（两个端点）。单日失败不影响其它天，只要有任意一天成功就返回结果。
func (c *Client) Week(ctx context.Context, dates []string, country string) ([]Episode, error) {
	type job struct {
		date string
		web  bool
	}
	jobs := make([]job, 0, len(dates)*2)
	for _, d := range dates {
		jobs = append(jobs, job{d, false}, job{d, true})
	}
	out := make([][]Episode, len(jobs))
	errs := make([]error, len(jobs))
	sem := make(chan struct{}, requestLimit)
	var wg sync.WaitGroup
	for i := range jobs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				errs[i] = ctx.Err()
				return
			}
			defer func() { <-sem }()
			out[i], errs[i] = c.Schedule(ctx, jobs[i].date, country, jobs[i].web)
		}(i)
	}
	wg.Wait()

	var all []Episode
	var firstErr error
	okDays := 0
	for i := range jobs {
		if errs[i] != nil {
			if firstErr == nil {
				firstErr = errs[i]
			}
			continue
		}
		okDays++
		all = append(all, out[i]...)
	}
	if okDays == 0 {
		return nil, firstErr
	}
	return all, firstErr
}

// Ping 真打一次当日排期，给设置页的「测试连接」用。
func (c *Client) Ping(ctx context.Context) (int, error) {
	date := c.now().Format("2006-01-02")
	// 绕过缓存：测试按钮的意义就是「此刻能不能连上」。
	c.mu.Lock()
	delete(c.cache, c.cacheKey(date, false))
	c.mu.Unlock()
	eps, err := c.Schedule(ctx, date, "US", false)
	if err != nil {
		return 0, err
	}
	return len(eps), nil
}
