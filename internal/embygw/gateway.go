// Package embygw 是挡在 Emby 前面的 302 网关。
//
// 为什么需要它：Emby 播 .strm 时**不会**把客户端 302 到网盘 CDN —— 它自己把
// .strm 里的地址读出来、把字节拉下来、再转发给播放器。于是看一部 69GB 的
// remux，家宽上行和 Cloudflare 隧道全被这一段中转吃满。
//
// 这个网关做两件事：
//  1. 改写 PlaybackInfo 响应，把 .strm 媒体源的播放地址改成不经 Emby 中转的
//     入口（见 playback.go / mode.go，三档可切换）——客户端发什么路径从此
//     由我们决定，不必再逐个适配 SenPlayer / Infuse / Emby Web 各自的写法；
//  2. 认出「客户端要播放某个条目」的请求，问 Emby 这个条目的 .strm 内容，
//     把客户端 302 到网盘直链。视频字节从此不经过 Emby。
//
// 其余请求（登录、海报、转码）一律原样反向代理，行为不变。
//
// 三条安全边界：
//  1. 只拦「静态直出」的请求（static=true / original / Download）。转码与 HLS
//     必须继续走 Emby，否则客户端拿到一个 .mkv 直链播不了。
//  2. 解析不出来、或者条目不是 .strm（本地真实文件）时，静默退回代理 ——
//     网关出问题最多是「回到原来的慢」，不能变成播不了。
//  3. UA 命中例外名单的客户端直接不拦：个别播放器不跟 302。
package embygw

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"strm115/internal/playback"
)

// Resolver 把 .strm 里的 id 换成网盘直链。实现方是 play.Proxy。
type Resolver func(ctx context.Context, id, ua string) (string, error)

// PlayPaths 是「不该进 Emby 反代」的路径：播放跳转和健康检查都由播放代理自己处理。
//
// 网关和播放代理现在共用同一个端口（6086），所以这一步不能省：否则
// /d/{pickcode} 会被当成 Emby 路径转给 8096，播放立刻 404。
func isPlayPath(p string) bool {
	return p == "/healthz" || strings.HasPrefix(p, "/d/")
}

// Options 是网关的运行参数。
type Options struct {
	Upstream string   // 例如 http://127.0.0.1:8096
	APIKey   string   // 客户端没带 token 时用它查条目（兜底）
	Bypass   []string // UA 子串，命中则完全不拦（个别播放器不跟 302）
	Resolve  Resolver
	// Mode 决定 PlaybackInfo 改写后把 Path 指向哪（见 mode.go）。
	// 空值 = redirect，也就是最兼容的「统一 302 入口」。
	Mode PlayMode
	// Play 是同端口上的播放代理。命中 /d/ 或 /healthz 时原样交给它，
	// 其余的才当成 Emby 请求处理 —— 两个服务现在只开一个端口。
	Play http.Handler
	// MapLocal 把「媒体服务器看到的路径」换成本机路径。
	// 两边挂同一个卷时是恒等映射；服务器在别的机器上时由 server_path_from/to 描述。
	MapLocal func(string) string
	// Playback 记「此刻有几台设备在看」。不传就用进程级默认表，
	// 没配上限时等价于不限流，单测构造的网关因此零影响。
	Playback *playback.Registry
	Log      *slog.Logger
}

// Gateway 是 http.Handler：先看是不是「要播放」，是就 302，否则反向代理。
type Gateway struct {
	opt   Options
	proxy *httputil.ReverseProxy
	log   *slog.Logger

	// 同一条目的并发起播只预热一次：播放器点开一集常常连发两三个
	// PlaybackInfo，每个都去换一次直链纯属浪费网盘配额。
	warmMu sync.Mutex
	warm   map[string]bool

	// PlaybackInfo 阶段记下的 itemID → MediaSource Id。客户端随后按简化路径
	// 请求外挂字幕时，靠它把缺的那段补回去（见 subtitle.go）。
	srcMu      sync.RWMutex
	sourceIDs  map[string]string
}

// 客户端取「原始文件」的几种路径形态：
//
//	/emby/videos/437/stream?static=true&MediaSourceId=mediasource_437
//	/emby/Videos/437/stream?static=true   ← SenPlayer / Infuse 这类第三方客户端
//	/Videos/437/stream.mkv?static=true    ← 不带 /emby 前缀
//	/emby/videos/437/original.mkv
//	/emby/Items/437/Download
//
// (?i) 不能省：Emby 自己的路由不区分大小写，官方 API 文档写的是 `/Videos/{Id}/stream`
// （大写 V），而 Emby Web 发的是小写 `videos`。以前正则只认小写，第三方客户端
// 的请求全部落到反代 → Emby 自己读 .strm 中转字节，公网下又慢又容易断。
var reStream = regexp.MustCompile(`(?i)^/(?:emby/)?(?:videos|audio)/([^/]+)/(?:stream|original)(?:\.[0-9a-zA-Z]+)?$`)
var reDownload = regexp.MustCompile(`(?i)^/(?:emby/)?Items/([^/]+)/Download$`)

// PlaybackInfo 是「客户端问怎么播」的那一步，永远早于真正的取流请求。
var rePlaybackInfo = regexp.MustCompile(`(?i)^/(?:emby/)?Items/([^/]+)/PlaybackInfo$`)

// 第三方客户端的鉴权头：`MediaBrowser Client="...", Device="...", Token="xxx"`。
// 它们往往不带 X-Emby-Token，只有这个复合头；抽不出 token 就查不到条目路径，
// 网关会静默退回中转，现象是「有的客户端能直连、有的不能」。
var reAuthToken = regexp.MustCompile(`(?i)Token\s*=\s*"?([^",\s]+)"?`)

// New 构造网关。Upstream 解析不出来时返回错误（调用方据此不启用）。
func New(opt Options) (*Gateway, error) {
	up, err := url.Parse(strings.TrimRight(opt.Upstream, "/"))
	if err != nil || up.Scheme == "" || up.Host == "" {
		return nil, fmt.Errorf("Emby 地址不对：%q", opt.Upstream)
	}
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	rp := httputil.NewSingleHostReverseProxy(up)
	// 反向代理不能自己加超时：Emby 的 WebSocket / 长轮询会被切断。
	rp.Transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		MaxIdleConnsPerHost:   32,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	}
	origDirector := rp.Director
	rp.Director = func(r *http.Request) {
		origDirector(r)
	// PlaybackInfo 与条目详情的响应体要读，压缩过就解不开；只对这两个接口关压缩。
	// 详情页不改内容，只用来提前预热直链（STRMhub 的做法）。
	if rePlaybackInfo.MatchString(r.URL.Path) || reItemDetail.MatchString(r.URL.Path) {
		r.Header.Set("Accept-Encoding", "identity")
	}
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		opt.Log.Warn("Emby 反代失败", "path", r.URL.Path, "err", err)
		http.Error(w, "Emby 连不上："+err.Error(), http.StatusBadGateway)
	}
	g := &Gateway{opt: opt, proxy: rp, log: opt.Log, warm: map[string]bool{}}
	// 从源头把播放地址统一到不经 Emby 中转的入口；详情页只做预热。
	rp.ModifyResponse = func(resp *http.Response) error {
		if err := g.rewritePlaybackInfo(resp); err != nil {
			return err
		}
		return g.prefetchOnItemDetail(resp)
	}
	return g, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// 把客户端真正访问的协议 / 域名记进 context：ReverseProxy 会把出站请求的
	// Host 改成上游 Emby，ModifyResponse 里已经读不到原始值了。
	r = withClientBase(r)
	// 起播名额检查放在最前面：静态直出和 /d/ 直链都要算「一台设备在看」，
	// 而 PlaybackInfo、海报、转码、HEAD 探测都不算。
	if !g.allowPlayback(w, r) {
		return
	}
	// 播放代理优先：同一个端口既要能 302 直链，也要能当 Emby 入口用。
	if g.opt.Play != nil && isPlayPath(r.URL.Path) {
		g.opt.Play.ServeHTTP(w, r)
		return
	}
	// 第三方播放器按简化路径取外挂字幕时 Emby 会 404；补上 MediaSourceId 段
	// 再反代，本站生成的简体中文字幕才真的能被加载出来。字幕与「播放器跟不跟
	// 302」无关，所以放在 UA 例外名单之前。
	if g.rewriteSimpleSubtitlePath(r) {
		g.log.Debug("字幕路径已补全", "path", r.URL.Path)
		// 字幕请求本来就该交给 Emby，别再落到下面「播放请求未接管」的噪声日志里。
		g.proxy.ServeHTTP(w, r)
		return
	}
	if g.bypassed(r.UserAgent()) {
		g.proxy.ServeHTTP(w, r)
		return
	}
	// 起播前先换好直链：PlaybackInfo 与真正的取流请求之间至少隔一次往返，
	// 这段时间正好够把 302 要用的地址准备好（冷启动实测 2s → 0.2s）。
	if id, ok := playbackItemID(r); ok {
		g.prewarm(id, r)
		g.proxy.ServeHTTP(w, r)
		return
	}

	id, ok := streamItemID(r)
	if !ok {
		// 第三方客户端的路径写法、参数跟 Emby Web 常常不一样。留一行日志，
		// 用户说「这个播放器没走直连」时不用靠猜它到底请求了什么。
		// 只记主播放请求，HLS 分片（每秒一条）不在这里，免得刷屏。
		if looksLikePlayRequest(r.URL.Path) {
			g.log.Info("播放请求未接管，转回 Emby 中转",
				"method", r.Method, "path", r.URL.Path,
				"query", shortQuery(r.URL.RawQuery), "ua", uaLabel(r.UserAgent()))
		}
		g.proxy.ServeHTTP(w, r)
		return
	}
	// 分两步走，日志才能区分「这不是我们的片子」和「我们的片子但没换成直链」：
	// 前者静默（本地真实文件、别家直链都走这条路），后者要留痕。
	itemPath, err := g.itemPath(r.Context(), id, r)
	if err != nil {
		g.log.Debug("查不到条目路径，转回 Emby 代理", "item", id, "err", err)
		g.proxy.ServeHTTP(w, r)
		return
	}
	// Emby 条目里的 Path 通常是那个 .strm 文件本身，真正的播放地址写在文件里；
	// 也有版本直接把 .strm 内容（/d/ URL）当远程源返回。两种都要能抽出 pickcode，
	// 否则 302 会静默退回中转。本地真实文件（.mkv 等）两者都认不出，原样回代理。
	_, pick, _ := g.sourceDirectURL(itemPath)
	if pick == "" {
		var ok bool
		pick, ok = g.strmTargetOf(itemPath)
		if !ok {
			g.proxy.ServeHTTP(w, r)
			return
		}
	}
	resolveStart := time.Now()
	target, err := g.opt.Resolve(r.Context(), pick, r.UserAgent())
	if err != nil {
		// 解析失败一律回到代理：宁可慢，不能播不了。
		g.log.Warn("直链解析失败，转回 Emby 中转", "item", id, "pick", pick, "err", err)
		g.proxy.ServeHTTP(w, r)
		return
	}
	// ms 里带着「这次是命中预热好的缓存，还是现问了一遍网盘」：
	// 冷启动是几百到两千毫秒，预热命中是几毫秒。排查起播慢全看这个数。
	g.log.Info("播放改为直连 CDN", "item", id, "ua", uaLabel(r.UserAgent()),
		"ms", time.Since(resolveStart).Milliseconds(), "ip", r.RemoteAddr)
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, target, http.StatusFound)
}

// playbackItemID 从 PlaybackInfo 请求里取出条目 id。
func playbackItemID(r *http.Request) (string, bool) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		return "", false
	}
	m := rePlaybackInfo.FindStringSubmatch(r.URL.Path)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// prewarm 在后台把「条目 → 直链」这段先跑掉。
//
// 为什么要单独预热：客户端起播是两步 —— 先 POST PlaybackInfo 拿媒体源，
// 再 GET stream 取字节。第二步到达时如果还要现场问 Emby 条目路径、读 .strm、
// 再向网盘换直链，用户看到的就是一两秒的黑屏。这段在第一步之后就已经能算出，
// 提前算完，第二步命中缓存直接 302。
//
// 边界：
//   - 不阻塞当前请求（PlaybackInfo 该多快还多快）；
//   - 换一个独立 context，请求返回后原 context 会被取消；
//   - 同条目同 UA 并发时只跑一次；
//   - 任何一步失败都只写 debug 日志，不影响那次播放（真播时还会再算一遍）。
func (g *Gateway) prewarm(itemID string, r *http.Request) {
	if g.opt.Resolve == nil || g.opt.Play == nil {
		return
	}
	token, ua := g.embyToken(r), r.UserAgent()
	if token == "" {
		return
	}
	key := itemID + "|" + ua

	g.warmMu.Lock()
	if g.warm[key] {
		g.warmMu.Unlock()
		return
	}
	g.warm[key] = true
	g.warmMu.Unlock()

	go func() {
		defer func() {
			g.warmMu.Lock()
			delete(g.warm, key)
			g.warmMu.Unlock()
		}()

		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		itemPath, err := g.itemPathWith(ctx, itemID, token)
		if err != nil {
			g.log.Debug("预热跳过：查不到条目路径", "item", itemID, "err", err)
			return
		}
		// 与取流时同一套解析：Path 可能是 .strm 文件，也可能已经是 /d/ URL。
		_, pick, _ := g.sourceDirectURL(itemPath)
		if pick == "" {
			var ok bool
			pick, ok = g.strmTargetOf(itemPath)
			if !ok {
				return
			}
		}
		g.prewarmPick(pick, ua)
	}()
}

// prewarmPick 直接按 pickcode 预热，不必再问一遍 Emby。
//
// PlaybackInfo 改写时媒体源的 Path 里已经有 /d/{id}，详情页预取也一样。
// 过去的 prewarm 一定要先查条目、再读 .strm 文件，遇到「Path 本来就是
// /d/ URL」的 Emby 版本会整个跳过 —— 冷解析 1~6 秒全落在起播关键路径上。
// 这里与 /d/ 端点共用同一个 Resolve（同一份直链缓存），命中即返回。
func (g *Gateway) prewarmPick(pick, ua string) {
	if pick == "" || g.opt.Resolve == nil {
		return
	}
	key := "pick:" + pick + "|" + ua
	g.warmMu.Lock()
	if g.warm[key] {
		g.warmMu.Unlock()
		return
	}
	g.warm[key] = true
	g.warmMu.Unlock()

	go func() {
		defer func() {
			g.warmMu.Lock()
			delete(g.warm, key)
			g.warmMu.Unlock()
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		start := time.Now()
		if _, err := g.opt.Resolve(ctx, pick, ua); err != nil {
			g.log.Debug("预热失败", "pick", pick, "err", err)
			return
		}
		g.log.Debug("直链已预热", "pick", pick, "ms", time.Since(start).Milliseconds())
	}()
}

func (g *Gateway) bypassed(ua string) bool {
	if ua == "" {
		return false
	}
	for _, s := range g.opt.Bypass {
		s = strings.TrimSpace(s)
		if s != "" && strings.Contains(strings.ToLower(ua), strings.ToLower(s)) {
			return true
		}
	}
	return false
}

// streamItemID 判断这次请求是不是「要原始文件」，是就返回条目 id。
//
// 只有静态直出才算：转码请求（带 VideoCodec / 输出 m3u8）必须留在 Emby，
// 否则客户端会拿到一个它解不了的 .mkv 直链。
func streamItemID(r *http.Request) (string, bool) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return "", false
	}
	p := r.URL.Path
	if m := reDownload.FindStringSubmatch(p); m != nil {
		return m[1], true
	}
	m := reStream.FindStringSubmatch(p)
	if m == nil {
		return "", false
	}
	lower := strings.ToLower(p)
	if strings.HasSuffix(lower, "/original") || strings.Contains(lower, "/original.") {
		return m[1], true
	}
	// 参数名也要大小写不敏感：Emby 官方 API 写的是 Static=true（大写 S），
	// SenPlayer / Infuse 照着发；只认小写 static 时它们会静默退回中转。
	if queryTrueFold(r.URL.Query(), "static") {
		return m[1], true
	}
	return "", false
}

// queryTrueFold 找「参数名忽略大小写等于 key、值忽略大小写等于 true」的项。
// url.Values.Get 是大小写敏感的，客户端把 Static / STATIC 换种写法就会漏。
func queryTrueFold(q url.Values, key string) bool {
	for k, vals := range q {
		if !strings.EqualFold(k, key) {
			continue
		}
		for _, v := range vals {
			if strings.EqualFold(strings.TrimSpace(v), "true") {
				return true
			}
		}
	}
	return false
}

// embyToken 从请求里取客户端的 Emby token，取不到则用设置里的兜底 Key。
// 解析规则与 requestToken 共用一份：Bearer / ApiKey / 复合头都要认。
func (g *Gateway) embyToken(r *http.Request) string {
	if t := requestToken(r); t != "" {
		return t
	}
	return g.opt.APIKey
}

// itemPath 借客户端自己的 token 查条目路径，服务端不用再存一份用户凭据。
func (g *Gateway) itemPath(ctx context.Context, itemID string, r *http.Request) (string, error) {
	return g.itemPathWith(ctx, itemID, g.embyToken(r))
}

func (g *Gateway) itemPathWith(ctx context.Context, itemID, token string) (string, error) {
	if token == "" {
		return "", fmt.Errorf("请求里没有 Emby token")
	}

	// Emby 的接口在 /emby 下才是完整的那一套（根路径只有一部分）；
	// 上游本身已经带 /emby 时不要再叠一层。
	prefix := "/emby"
	if strings.HasSuffix(strings.TrimRight(g.opt.Upstream, "/"), "/emby") {
		prefix = ""
	}
	u := strings.TrimRight(g.opt.Upstream, "/") + prefix + "/Items?Ids=" + url.QueryEscape(itemID) + "&Fields=Path&Limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-Emby-Token", token)
	req.Header.Set("Accept", "application/json")

	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("Emby 返回 %d", resp.StatusCode)
	}
	var out struct {
		Items []struct {
			Path string `json:"Path"`
		} `json:"Items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Items) == 0 || out.Items[0].Path == "" {
		return "", fmt.Errorf("条目 %s 没有 Path", itemID)
	}
	return out.Items[0].Path, nil
}

// strmTargetOf 读 .strm 文件并抽出播放代理的 id。
//
// 只认 .strm 后缀：库里同时有真实视频文件时，那些条目的 Path 是 .mkv，
// 读它既没意义也不安全。
func (g *Gateway) strmTargetOf(serverPath string) (string, bool) {
	p := serverPath
	if g.opt.MapLocal != nil {
		p = g.opt.MapLocal(p)
	}
	if !strings.HasSuffix(strings.ToLower(p), ".strm") {
		return "", false
	}
	// .strm 里就一行地址，4KB 足够；多读的可能是被改坏的库，不碰。
	b, err := os.ReadFile(p)
	if err != nil {
		g.log.Debug(".strm 读不到，转回 Emby 代理", "path", p, "err", err)
		return "", false
	}
	if len(b) > 4096 {
		b = b[:4096]
	}
	return strmTarget(string(b))
}

// strmTarget 从 .strm 内容里抽出播放代理的 id（/d/{pickcode} 或 /d/q/{fid}）。
//
// 只认自己写的地址：路径里必须出现 /d/，抽出来的是它后面那一段（带不带扩展名
// 都行，play.Proxy 会剥）。本地真实文件、别的网盘直链一律不接管。
func strmTarget(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", false
	}
	i := strings.Index(u.Path, "/d/")
	if i < 0 {
		return "", false
	}
	pick := strings.Trim(u.Path[i+len("/d/"):], "/")
	if pick == "" {
		return "", false
	}
	return pick, true
}

// looksLikePlayRequest 只认「一次播放一个」的主请求，不含 HLS 分片。
func looksLikePlayRequest(p string) bool {
	l := strings.ToLower(p)
	// /Subtitles/36/Stream.srt 里也有 "stream"，但那是字幕，不是播放请求。
	// 把它记成「未接管、转回中转」会让人以为播放链路坏了。
	if strings.Contains(l, "/subtitles/") {
		return false
	}
	return strings.Contains(l, "/stream") || strings.Contains(l, "/original") ||
		strings.Contains(l, "/download") || strings.Contains(l, "master.m3u8")
}

func shortQuery(q string) string {
	if len(q) > 160 {
		return q[:160]
	}
	return q
}

// reAuthDeviceID / reAuthDeviceName 从 MediaBrowser 复合头里取设备标识。
// 第三方客户端（SenPlayer / Infuse / Emby iOS）有时不带 X-Emby-Token，
// 只把一串 MediaBrowser Client="…", Device="…", DeviceId="…", Token="…"
// 塞在 X-Emby-Authorization 里。
var reAuthDeviceID = regexp.MustCompile(`(?i)DeviceId\s*=\s*"([^"]+)"`)
var reAuthDeviceName = regexp.MustCompile(`(?i)Device\s*=\s*"([^"]+)"`)
var reAuthClient = regexp.MustCompile(`(?i)Client\s*=\s*"([^"]+)"`)

// allowPlayback 占一个「同时播放设备」名额；满了返回 false 并已写好响应。
//
// 只拦 GET 的主播放请求：HEAD 是探测/预热，POST PlaybackInfo 只是问怎么播，
// HLS 分片与转码没有静态直出参数，都不会走到这里来占坑。
func (g *Gateway) allowPlayback(w http.ResponseWriter, r *http.Request) bool {
	reg := g.opt.Playback
	if reg == nil {
		reg = playback.Default
	}
	if reg == nil || r.Method != http.MethodGet {
		return true
	}
	ua := r.UserAgent()
	dev := deviceIDOf(r)
	// Emby 服务端会自己来拉流做 ffprobe（UA 是 ffmpeg / libav，且不带
	// DeviceId），那不是「一台设备在看」，占掉名额会把真正要播的人拦在门外。
	if isServerUA(ua) {
		return true
	}
	var itemID string
	if strings.HasPrefix(r.URL.Path, "/d/") {
		// 直连模式：只认有 UA 的播放器，空 UA 的探测一律不算。
		if ua == "" {
			return true
		}
	} else {
		id, ok := streamItemID(r)
		if !ok {
			return true
		}
		// 统一 302 入口：Emby / SenPlayer / Infuse 起播都会带 DeviceId。
		// 没带的宁可不统计，也不能把 ffprobe 当成一个人。
		if dev == "" {
			return true
		}
		itemID = id
	}
	ip := clientIP(r)
	err := reg.Acquire(r.Context(), playback.Session{
		Key:    playback.KeyFor(dev, ip, ua),
		Label:  deviceLabel(r, ua),
		IP:     ip,
		UA:     ua,
		ItemID: itemID,
	})
	if errors.Is(err, playback.ErrFull) {
		limit := reg.Limit()
		w.Header().Set("Retry-After", "5")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprintf(w,
			"同时播放设备已达上限（%d 台），请稍后再试；也可到「系统配置 → 播放直连」调高上限或释放旧设备。", limit)
		g.log.Warn("同时播放设备已达上限", "limit", limit,
			"path", r.URL.Path, "ip", ip, "ua", uaLabel(ua))
		return false
	}
	// ctx 取消之类的异常不该让播放卡死，最差就是这一台没被统计。
	return true
}

func deviceIDOf(r *http.Request) string {
	for _, v := range []string{
		queryFold(r, "DeviceId"),
		r.Header.Get("X-Emby-Device-Id"),
	} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	for _, v := range []string{
		r.Header.Get("X-Emby-Authorization"),
		r.Header.Get("Authorization"),
	} {
		if m := reAuthDeviceID.FindStringSubmatch(v); len(m) == 2 && m[1] != "" {
			return m[1]
		}
	}
	return ""
}

func deviceLabel(r *http.Request, ua string) string {
	for _, v := range []string{
		r.Header.Get("X-Emby-Authorization"),
		r.Header.Get("Authorization"),
	} {
		if m := reAuthDeviceName.FindStringSubmatch(v); len(m) == 2 && m[1] != "" {
			return m[1]
		}
		if m := reAuthClient.FindStringSubmatch(v); len(m) == 2 && m[1] != "" {
			return m[1]
		}
	}
	if d := queryFold(r, "DeviceId"); d != "" {
		return d
	}
	return uaLabel(ua)
}

func clientIP(r *http.Request) string {
	if v := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); v != "" {
		return v
	}
	if v := r.Header.Get("X-Forwarded-For"); v != "" {
		if i := strings.IndexByte(v, ','); i >= 0 {
			v = v[:i]
		}
		return strings.TrimSpace(v)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// isServerUA 过滤服务端自己的探测（ffprobe / 预热 / 脚本），它们不该占设备名额。
func isServerUA(ua string) bool {
	l := strings.ToLower(ua)
	for _, k := range []string{
		"emby server", "ffmpeg", "libav", "lavf", "go-http-client",
		"python-urllib", "python-requests", "curl/",
	} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

func uaLabel(ua string) string {
	if ua == "" {
		return "<无>"
	}
	if len(ua) > 60 {
		return ua[:60]
	}
	return ua
}
