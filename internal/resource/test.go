package resource

// 「测一下这个源还活着吗」。
//
// 搜索页原来的问题是：配完只能等到真搜片名时才知道某个源挂了，
// 而那时看到的只是一句「失败」，分不清是地址写错、要登录、还是被墙。
// 这里给每个源一条独立的探测路径，把「能不能连上 / 返回了什么」直接摊开。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TestOptions 是一次源探测的参数。
type TestOptions struct {
	Kind   string // tg / site / custom
	Target string // 频道名、站点 key，或自定义源那一行原文
	Query  string // 探测用的关键词，留空则按 kind 取默认值
	Proxy  string
	// Timeout 只兜住一个源，测试是有人在界面上等着的，不该给太久。
	Timeout time.Duration
}

// TestResult 是探测结果。
//
// OK 表示「这个源能正常返回」，不代表「一定搜到东西」：私有站因为没登录
// 返回 0 条时，OK 仍是 true，但 Notice 会说明原因 —— 这两件事得分开，
// 否则用户看到 0 条会把一个正常的源删掉。
type TestResult struct {
	Kind    string   `json:"kind"`
	Name    string   `json:"name"`
	OK      bool     `json:"ok"`
	Count   int      `json:"count"`
	MS      int64    `json:"ms"`
	Query   string   `json:"query"`
	URL     string   `json:"url,omitempty"` // 自定义源实际请求的地址（打码后的）
	Error   string   `json:"error,omitempty"`
	Notice  string   `json:"notice,omitempty"`
	Samples []string `json:"samples,omitempty"`
	// Cached 表示这条是 60 秒内的旧结果，没有重新抓。
	// 只有 TG 频道有缓存：t.me 会把同一频道的连续请求挂住，连点两次测试
	// 第二次必然超时，那个红叉是假的。
	Cached bool `json:"cached,omitempty"`
}

// defaultTestQuery 是「探活」用的关键词。
//
// 挑 1080p 而不是某个片名：资源站上它几乎必然有结果，能真正验证到
// 「解析出来的条目里有没有磁力」这一步，而不只是「HTTP 200」。
const defaultTestQuery = "1080p"

// Test 探一个源。
//
// 返回值 error 只表示「参数不对」（比如 kind 写错、目标为空），
// 源本身连不上属于正常结果，放在 TestResult.Error 里。
func (s *Service) Test(ctx context.Context, opt TestOptions) (*TestResult, error) {
	kind := strings.ToLower(strings.TrimSpace(opt.Kind))
	target := strings.TrimSpace(opt.Target)
	if target == "" {
		return nil, errors.New("要测哪个源？先填一个频道或地址")
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 15 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, opt.Timeout)
	defer cancel()

	hc := httpClient(opt.Proxy, opt.Timeout)
	q := strings.TrimSpace(opt.Query)

	switch kind {
	case "tg", "telegram", "channel":
		ch := ""
		if list := NormalizeChannels(target); len(list) > 0 {
			ch = list[0]
		}
		if ch == "" {
			return nil, errors.New("频道名不对，应该像 QukanMovie 或 t.me/QukanMovie")
		}
		// 不带关键词也能探：能打开预览页就说明频道存在且公开。
		t0 := time.Now()
		items, cached, err := s.telegram(ctx, hc, ch, q, 5, false)
		r := &TestResult{Kind: "tg", Name: "@" + ch, MS: time.Since(t0).Milliseconds(), Query: q, Cached: cached}
		r.URL = "https://t.me/s/" + ch
		fillResult(r, items, err)
		if r.OK && r.Count == 0 {
			switch {
			case q == "":
				r.Notice = "频道能打开，但最近几条没有正文内容 —— 可能刚建、或设了仅订阅者可见"
			default:
				r.Notice = "频道能打开，但这个关键词没搜到；换「测试词」再试一次"
			}
		}
		return r, nil

	case "site", "torrent", "builtin":
		site, ok := findSite(target)
		if !ok {
			return nil, fmt.Errorf("不认识的站点 %q（内置的只有：%s）",
				target, strings.Join(DefaultSites, " / "))
		}
		if q == "" {
			q = defaultTestQuery
		}
		t0 := time.Now()
		items, err := site.Search(ctx, hc, q, 5)
		r := &TestResult{Kind: "site", Name: site.Name, MS: time.Since(t0).Milliseconds(), Query: q}
		fillResult(r, items, err)
		if r.OK && r.Count == 0 {
			r.Notice = "站点能连上，但没返回结果 —— 换「测试词」再试一次"
		}
		return r, nil

	case "custom", "rss", "torznab", "":
		sites := ParseCustomSites(target)
		if len(sites) == 0 {
			return nil, errors.New("这条地址读不出来，应该像：我的站 | https://example.com/api?apikey=xxx&q={q}")
		}
		cs := sites[0]
		if q == "" {
			q = defaultTestQuery
		}
		t0 := time.Now()
		items, err := searchCustom(ctx, hc, cs, q, 5)
		r := &TestResult{Kind: "custom", Name: cs.Name, MS: time.Since(t0).Milliseconds(), Query: q}
		r.URL = maskURL(cs.searchURL(q))
		fillResult(r, items, err)
		if r.OK && r.Count == 0 {
			r.Notice = "地址能访问，但没解析出条目 —— 确认它返回的是 RSS/Torznab，" +
				"且关键词占位符写的是 {q}"
		}
		return r, nil
	}
	return nil, fmt.Errorf("kind 只能是 tg / site / custom，收到的是 %q", opt.Kind)
}

// fillResult 把「跑出来的东西」和「跑出来的错」翻译成给用户看的结果。
func fillResult(r *TestResult, items []Item, err error) {
	if err != nil {
		r.Error = err.Error()
		return
	}
	r.OK = true
	r.Count = len(items)
	for _, it := range items {
		if len(r.Samples) >= 3 {
			break
		}
		t := it.Title
		if n := []rune(t); len(n) > 60 {
			t = string(n[:60]) + "…"
		}
		// 有磁力的打个标：探活的关键是「能不能直接入库」，只看标题不够
		if it.Magnet != "" {
			t += "  [磁力]"
		}
		r.Samples = append(r.Samples, t)
	}
}

// maskURL 打码地址里的密钥。
//
// apikey / passkey 一类的参数在界面上直接显示没有好处，而自定义源的地址
// 恰恰都带密钥 —— 测试结果里露一次，截图发出去就泄露了。
func maskURL(raw string) string {
	i := strings.Index(raw, "?")
	if i < 0 {
		return raw
	}
	base, qs := raw[:i], raw[i+1:]
	parts := strings.Split(qs, "&")
	for j, p := range parts {
		k := p
		if e := strings.Index(p, "="); e >= 0 {
			k = p[:e]
		}
		switch strings.ToLower(k) {
		case "apikey", "api_key", "passkey", "key", "token", "password", "pwd", "r":
			parts[j] = k + "=***"
		}
	}
	return base + "?" + strings.Join(parts, "&")
}
