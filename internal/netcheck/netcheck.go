// Package netcheck 回答一个很具体的问题：这台机器现在能连上哪些外部服务。
//
// 自部署最容易踩的坑不是代码，而是「明明配好了却不通」：
// 115 的 webapi 被墙、Telegram 直连超时、种子站要代理、Emby 在另一个网段。
// 与其让用户去猜是哪个环节坏了，不如一次把这些端点都戳一遍，红了就是那一条的问题。
//
// 判定标准很简单：DNS 解析 + TCP + TLS + 拿到 HTTP 响应就算「可达」。
// 状态码只作为信息展示 —— api.telegram.org 根路径本来就回 404，
// 能回 404 恰恰证明网络是通的。
package netcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"strm115/internal/tmdb"
)

// Target 是一个要探测的端点。
type Target struct {
	Key   string // 稳定标识，前端按它排序/去重
	Name  string // 显示名：115 网盘
	Host  string // 显示用的主机名：webapi.115.com
	URL   string // 实际请求的地址
	Group string // 分组：基础网络 / 核心服务 / 元数据 / 外网 / 资源站
	// Proxy 表示这条要不要额外测一遍「经代理」。
	// 国内本来就通的（百度、115）走代理没意义，只会多两行噪音。
	Proxy bool
}

// Result 是一条探测结果。
type Result struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Host   string `json:"host"`
	Group  string `json:"group"`
	Via    string `json:"via"` // direct / proxy
	OK     bool   `json:"ok"`
	Status int    `json:"status,omitempty"`
	MS     int64  `json:"ms"`
	Err    string `json:"error,omitempty"`
}

// Report 是一整轮结果。
type Report struct {
	Proxy     string   `json:"proxy,omitempty"`
	Results   []Result `json:"results"`
	OK        int      `json:"ok"`
	Total     int      `json:"total"`
	CheckedAt string   `json:"checked_at"`
}

// Targets 返回内置的探测清单。
//
// 挑的都是「项目真的会去连」的地址：改这里之前先确认还有代码在用它，
// 否则网络检测会变成一份过时的清单。
// hostOf 只用来给界面显示主机名，解析不出来就原样返回，不影响探测。
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}

func Targets() []Target {
	// TMDB 的两条要跟着当前上游走：配了反代之后，页面还显示
	// api.themoviedb.org / image.tmdb.org 红了，会让人以为配置没生效。
	// 判定标准是「拿到 HTTP 响应就算通」，所以反代给的 401/404 也照样绿。
	apiBase, imgBase := tmdb.CurrentUpstream()
	apiURL := strings.TrimRight(apiBase, "/") + "/configuration"
	imgURL := strings.TrimRight(imgBase, "/") + "/w92/"
	return []Target{
		{Key: "baidu", Name: "百度", Host: "www.baidu.com", URL: "https://www.baidu.com/", Group: "基础网络"},
		{Key: "wecom", Name: "企业微信", Host: "api.weixin.qq.com", URL: "https://api.weixin.qq.com/", Group: "基础网络"},
		{Key: "115", Name: "115 网盘", Host: "webapi.115.com", URL: "https://webapi.115.com/", Group: "核心服务"},
		// 播放走的是 302 到 *.115cdn.net：这条断不了不代表能播，
		// 但这条要是经代理，原盘码率会被出口带宽卡死。所以单独列出来，
		// 并且同时测直连与代理。
		{Key: "115cdn", Name: "115 播放 CDN", Host: "cdnfhnfile.115cdn.net",
			URL: "https://cdnfhnfile.115cdn.net/", Group: "核心服务", Proxy: true},

		// 接口和图片分开测：图片 CDN 被墙而接口正常的组合很常见
		// （墙上一片空白但接口不报错）。Host 从生效地址里取，配了反代
		// 时界面上直接显示反代域名。
		{Key: "tmdb", Name: "TMDB 接口（电影墙）", Host: hostOf(apiURL), URL: apiURL, Group: "元数据", Proxy: true},
		{Key: "tmdbimg", Name: "TMDB 图片 CDN", Host: hostOf(imgURL), URL: imgURL, Group: "元数据", Proxy: true},
		// IMDb 不在这里：光戳端点分不清「断网 / Key 无效 / 配额用光」，
		// 所以挪到设置 → 电影墙数据源里的「检测 IMDb」，拿 Key 真查一次。

		{Key: "google", Name: "Google", Host: "www.google.com", URL: "https://www.google.com/generate_204", Group: "外网", Proxy: true},
		{Key: "telegram", Name: "Telegram", Host: "api.telegram.org", URL: "https://api.telegram.org/", Group: "外网", Proxy: true},
		{Key: "github", Name: "GitHub API", Host: "api.github.com", URL: "https://api.github.com/", Group: "外网", Proxy: true},
		{Key: "dockerhub", Name: "Docker Hub", Host: "hub.docker.com", URL: "https://hub.docker.com/", Group: "外网", Proxy: true},

		{Key: "tpb", Name: "海盗湾 TPB", Host: "apibay.org", URL: "https://apibay.org/q.php?q=test", Group: "资源站", Proxy: true},
		{Key: "nyaa", Name: "Nyaa", Host: "nyaa.si", URL: "https://nyaa.si/", Group: "资源站", Proxy: true},
		{Key: "dmhy", Name: "动漫花园", Host: "share.dmhy.org", URL: "https://share.dmhy.org/", Group: "资源站", Proxy: true},
	}
}

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36"

// Run 并发把所有端点戳一遍。
//
// proxy 为空就只测直连；给了代理地址就再补一轮「经代理」，
// 这样用户能直接看出「直连不通但代理通」——这正是配代理的依据。
func Run(ctx context.Context, proxy string) Report {
	proxy = strings.TrimSpace(proxy)
	rep := Report{Proxy: proxy, CheckedAt: time.Now().Format(time.RFC3339)}

	direct := client("")
	var viaProxy *http.Client
	if proxy != "" {
		viaProxy = client(proxy)
	}

	type job struct {
		t   Target
		via string
		hc  *http.Client
	}
	var jobs []job
	for _, t := range Targets() {
		jobs = append(jobs, job{t: t, via: "direct", hc: direct})
		if t.Proxy && viaProxy != nil {
			jobs = append(jobs, job{t: t, via: "proxy", hc: viaProxy})
		}
	}

	// 6 个并发足够：再快也没用，瓶颈在那些超时的端点。
	sem := make(chan struct{}, 6)
	out := make([]Result, len(jobs))
	var wg sync.WaitGroup
	for i, j := range jobs {
		wg.Add(1)
		go func(i int, j job) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i] = probe(ctx, j.hc, j.t, j.via)
		}(i, j)
	}
	wg.Wait()

	for _, r := range out {
		rep.Total++
		if r.OK {
			rep.OK++
		}
	}
	rep.Results = out
	return rep
}

func client(proxy string) *http.Client {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 6 * time.Second}).DialContext,
		TLSHandshakeTimeout:   6 * time.Second,
		ResponseHeaderTimeout: 8 * time.Second,
		MaxIdleConns:          16,
		IdleConnTimeout:       30 * time.Second,
	}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport: tr,
		Timeout:   9 * time.Second,
		// 有些站会 301 到别的主机，跟过去才算真的通
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
}

func probe(ctx context.Context, hc *http.Client, t Target, via string) Result {
	r := Result{Key: t.Key, Name: t.Name, Host: t.Host, Group: t.Group, Via: via}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.URL, nil)
	if err != nil {
		r.Err = err.Error()
		return r
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "*/*")

	start := time.Now()
	resp, err := hc.Do(req)
	r.MS = time.Since(start).Milliseconds()
	if err != nil {
		r.Err = explain(err, via)
		return r
	}
	// 只读一点就关掉：我们要的是「能不能握手」，不是内容。
	io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	resp.Body.Close()

	// 407 = 代理要认证，等于代理没配好，算不通。
	if resp.StatusCode == http.StatusProxyAuthRequired {
		r.Status = resp.StatusCode
		r.Err = "代理要求认证（检查用户名密码）"
		return r
	}
	// 5xx 是代理/网关自己吐的：说明它连不上目标，而不是目标回了错误。
	// 以前把这类也算「可达」，用户看到一片绿却什么都打不开。
	if via == "proxy" && resp.StatusCode >= 500 {
		r.Status = resp.StatusCode
		r.Err = fmt.Sprintf("代理出口失败（HTTP %d）", resp.StatusCode)
		return r
	}
	r.OK = true
	r.Status = resp.StatusCode
	return r
}

// explain 把 net/http 那串又长又重复的错误压成一句人话。
// 界面上这一栏就那么宽，原始错误会撑爆布局，也没人看得懂。
func explain(err error, via string) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded) || strings.Contains(msg, "timeout"):
		if via == "proxy" {
			return "超时（代理没转发出去）"
		}
		return "超时（连不上，可能需要代理）"
	// 代理自己连不上时，错误里也带 "refused"，所以这一条要排在前面
	case strings.Contains(msg, "proxyconnect"):
		return "连不上代理"
	case strings.Contains(msg, "refused") || strings.Contains(msg, "actively refused"):
		return "拒绝连接"
	case strings.Contains(msg, "no such host"):
		return "DNS 解析失败"
	case strings.Contains(msg, "certificate"):
		return "证书错误（可能被劫持）"
	case strings.Contains(msg, "EOF"):
		return "连接被重置"
	}
	if len(msg) > 120 {
		msg = msg[:120] + "…"
	}
	return fmt.Sprintf("失败：%s", msg)
}
