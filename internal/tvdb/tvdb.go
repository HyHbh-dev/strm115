// Package tvdb 是 TheTVDB API v4 的最小客户端，只服务于播出日历。
//
// 分工：TMDB 出剧单、海报、分集播出日期和中文名；TVDB 补 TMDB 没有的
// 「每周几几点播」(airsTime / airsDays)、电视网 (originalNetwork)，以及
// 分集数据（episodes[].aired）作为 TMDB 分季接口的交叉补充。
//
// v4 没有 schedule 端点（OpenAPI 里没有 /schedule），所以「这周有哪些剧播」
// 不能靠它反查，剧单仍由 TMDB /tv/on_the_air 提供。
//
// 认证：POST /login {apikey, pin} → data.token，Bearer 有效 1 个月。
// 本包把 token 缓存在进程内，按 20 天主动换新（留 10 天余量），
// 遇到 401 再强制重登一次。没配 Key 时调用方拿到的 Client 直接是 nil，
// 日历会静默降级成「只有 TMDB」，不影响主链路。
package tvdb

import (
	"bytes"
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
	// DefaultBase 是 v4 接口根。测试里可以改成 httptest.Server 的地址。
	DefaultBase = "https://api4.thetvdb.com/v4"
	// tokenTTL 比官方的 1 个月短，避免卡在过期边界上。
	tokenTTL = 20 * 24 * time.Hour
)

// ErrNoKey 表示没配 TVDB API Key。
var ErrNoKey = errors.New("没有配置 TVDB API Key")

type Client struct {
	key  string
	pin  string
	Base string
	hc   *http.Client

	mu      sync.Mutex
	token   string
	tokenAt time.Time
	now     func() time.Time
}

// New 构造客户端。apiKey 为空时 Configured() 返回 false，调用方应跳过。
func New(apiKey, pin string) *Client {
	return &Client{
		key:  strings.TrimSpace(apiKey),
		pin:  strings.TrimSpace(pin),
		Base: DefaultBase,
		hc:   &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: netproxy.URL}},
		now:  time.Now,
	}
}

func (c *Client) Configured() bool { return c != nil && c.key != "" }

// Company 是 TVDB 里的电视网 / 制作公司记录，只取名字。
type Company struct {
	Name string `json:"name"`
}

// AirsDays 是每周固定播出的星期。字段按 OpenAPI 的 SeriesAirsDays。
type AirsDays struct {
	Monday    bool `json:"monday"`
	Tuesday   bool `json:"tuesday"`
	Wednesday bool `json:"wednesday"`
	Thursday  bool `json:"thursday"`
	Friday    bool `json:"friday"`
	Saturday  bool `json:"saturday"`
	Sunday    bool `json:"sunday"`
}

// Days 返回为 true 的星期（英文，和 TMDB 的播出地口径一致）。
func (a AirsDays) Days() []string {
	pairs := []struct {
		name string
		on   bool
	}{
		{"Monday", a.Monday}, {"Tuesday", a.Tuesday}, {"Wednesday", a.Wednesday},
		{"Thursday", a.Thursday}, {"Friday", a.Friday}, {"Saturday", a.Saturday},
		{"Sunday", a.Sunday},
	}
	out := []string{}
	for _, p := range pairs {
		if p.on {
			out = append(out, p.name)
		}
	}
	return out
}

// Episode 是 series extended 里的一集。aired 可能是 "2026-11-05" 或带时刻，
// 用 Date() 统一截成 YYYY-MM-DD。
type Episode struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Aired         string `json:"aired"`
	SeasonNumber  int    `json:"seasonNumber"`
	EpisodeNumber int    `json:"episodeNumber"`
	Runtime       int    `json:"runtime"`
	Image         string `json:"image"`
	Overview      string `json:"overview"`
}

// Date 返回播出日期；格式不对返回空串，调用方据此跳过这条。
func (e Episode) Date() string {
	s := strings.TrimSpace(e.Aired)
	if len(s) < 10 {
		return ""
	}
	d := s[:10]
	if len(d) != 10 || d[4] != '-' || d[7] != '-' {
		return ""
	}
	return d
}

// Series 是 /series/{id}/extended 的精简视图。
type Series struct {
	ID              int       `json:"id"`
	Name            string    `json:"name"`
	AirsTime        string    `json:"airsTime"`
	AirsDays        AirsDays  `json:"airsDays"`
	OriginalNetwork Company   `json:"originalNetwork"`
	LatestNetwork   Company   `json:"latestNetwork"`
	Image           string    `json:"image"`
	FirstAired      string    `json:"firstAired"`
	LastAired       string    `json:"lastAired"`
	NextAired       string    `json:"nextAired"`
	Episodes        []Episode `json:"episodes"`
}

// Network 优先用原始电视网，没有则退到最近一次的电视网。
func (s *Series) Network() string {
	if s == nil {
		return ""
	}
	if n := strings.TrimSpace(s.OriginalNetwork.Name); n != "" {
		return n
	}
	return strings.TrimSpace(s.LatestNetwork.Name)
}

// AirTime 规整成 HH:MM。TVDB 偶尔给 "9:00 PM" / "20:00:00"，只取前 5 个字符。
func (s *Series) AirTime() string {
	if s == nil {
		return ""
	}
	v := strings.TrimSpace(s.AirsTime)
	if len(v) >= 5 && v[2] == ':' {
		return v[:5]
	}
	return v
}

// Token 返回可用的 Bearer Token，必要时先登录。
func (c *Client) Token(ctx context.Context) (string, error) {
	if !c.Configured() {
		return "", ErrNoKey
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.now().Sub(c.tokenAt) < tokenTTL {
		return c.token, nil
	}
	tok, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.token, c.tokenAt = tok, c.now()
	return tok, nil
}

func (c *Client) invalidate() {
	c.mu.Lock()
	c.token, c.tokenAt = "", time.Time{}
	c.mu.Unlock()
}

func (c *Client) login(ctx context.Context) (string, error) {
	body := map[string]string{"apikey": c.key}
	if c.pin != "" {
		body["pin"] = c.pin
	}
	buf, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Base+"/login", bytes.NewReader(buf))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("请求 TVDB 登录失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("TVDB 登录失败（HTTP %d）：%s", resp.StatusCode, apiMessage(raw))
	}
	var out struct {
		Status string `json:"status"`
		Data   struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("解析 TVDB 登录响应失败: %w", err)
	}
	if tok := strings.TrimSpace(out.Data.Token); tok != "" {
		return tok, nil
	}
	if out.Status != "success" {
		return "", errors.New("TVDB 登录没返回 Token：" + apiMessage(raw))
	}
	return "", errors.New("TVDB 登录没返回 Token")
}

// SeriesExtended 查一部剧的播出时刻、电视网和分集。
func (c *Client) SeriesExtended(ctx context.Context, id int) (*Series, error) {
	if id <= 0 {
		return nil, fmt.Errorf("TVDB 剧集 id 不合法: %d", id)
	}
	v := url.Values{}
	v.Set("meta", "episodes")
	v.Set("short", "true")
	var out struct {
		Status string  `json:"status"`
		Data   *Series `json:"data"`
	}
	if err := c.request(ctx, "/series/"+strconv.Itoa(id)+"/extended", v, &out); err != nil {
		return nil, err
	}
	if out.Data == nil {
		return nil, errors.New("TVDB 没返回剧集数据")
	}
	return out.Data, nil
}

// Ping 验证 Key / PIN 是否有效：先换 Token，再打一个轻量端点确认 Token 能用。
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.Token(ctx); err != nil {
		return err
	}
	// /languages 返回的是 {"data":[...]}，只要能解析成 JSON 且 HTTP 200
	// 就说明 Token 真的能用；具体结构不关心。
	var probe any
	return c.request(ctx, "/languages", nil, &probe)
}

// request 带 Bearer Token 发 GET；401 时清 Token 重登再试一次。
func (c *Client) request(ctx context.Context, path string, v url.Values, out any) error {
	for attempt := 0; attempt < 2; attempt++ {
		tok, err := c.Token(ctx)
		if err != nil {
			return err
		}
		u := c.Base + path
		if len(v) > 0 {
			u += "?" + v.Encode()
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Accept", "application/json")
		resp, err := c.hc.Do(req)
		if err != nil {
			return fmt.Errorf("请求 TVDB 失败: %w", err)
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			c.invalidate()
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("TVDB %s 返回 %d：%s", path, resp.StatusCode, apiMessage(raw))
		}
		if out != nil {
			if err := json.Unmarshal(raw, out); err != nil {
				return fmt.Errorf("解析 TVDB 响应失败: %w", err)
			}
		}
		return nil
	}
	return errors.New("TVDB 认证反复失败")
}

// apiMessage 把 TVDB 的错误体翻成人话，别让界面只显示一个 401。
func apiMessage(raw []byte) string {
	var e struct {
		Message string `json:"message"`
		Status  string `json:"status"`
	}
	if json.Unmarshal(raw, &e) == nil {
		if e.Message != "" {
			return e.Message
		}
		if e.Status != "" {
			return e.Status
		}
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 160 {
		s = s[:160]
	}
	if s == "" {
		return "无响应体"
	}
	return s
}
