package embygw

// 改写 PlaybackInfo：让所有客户端拿到「不经 Emby 中转」的播放地址。
//
// 为什么不能只靠「猜客户端会发什么 stream 路径」：Emby Web 发
// /emby/videos/{id}/stream?static=true，SenPlayer 发
// /emby/videos/{id}/stream.mkv?...&Static=true（参数名大写 S），Infuse 又可能
// 省掉 /emby 前缀、加上 MediaSourceId。每漏一种，那次播放就静默退回 Emby
// 自己读 .strm 中转字节。日志里「播放请求未接管」记下的正是这些变体。
//
// 两个参考项目（115-station / STRMhub，同源的 MediaWarp 思路）给的是同一条
// 根治办法：在 PlaybackInfo 响应里把地址改掉，客户端发什么由我们决定。差别
// 只在改向哪里：
//
//	115-station → 自己的 /Videos/{id}/stream 302 入口（相对路径，最兼容）
//	STRMhub     → .strm 内容里的 /d/ 直链（DirectPlay，少一跳）
//
// 这里用 emby_gateway_mode 把两家的优点合成三档（见 mode.go），默认仍是
// 115-station 那档：万一某个客户端不认绝对 Path，行为与已验证的 v0.5.4 一致。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 打开条目详情也视为「马上要播」：STRMhub 在这一步就后台取直链，用户看简介
// 的那几秒把冷解析跑完，PlaybackInfo 阶段只剩缓存命中。
var reItemDetail = regexp.MustCompile(`(?i)^/(?:emby/)?(?:Users/[^/]+/)?Items/[^/]+/?$`)

// 第三方客户端把 UserId 放在复合鉴权头里，取流时 Emby 偶尔会要它。
var reUserID = regexp.MustCompile(`(?i)UserId\s*=\s*"?([^",\s]+)"?`)

// playbackStreamURL 是写进 PlaybackInfo 的相对地址（115-station 那一路）。
//
// 用相对路径：客户端沿用自己正在访问的协议 / 域名 / 反代前缀（公网走
// cloudflared、局域网走 6086 都对），不去读不可信的 X-Forwarded-Host。
// Static 故意用官方那种首字母大写的写法 —— 反正网关现在大小写都不敏感。
func playbackStreamURL(itemID, sourceID, token, userID string) string {
	q := url.Values{"Static": {"true"}}
	if sourceID != "" {
		q.Set("MediaSourceId", sourceID)
	}
	// 客户端自己的凭据回给它自己；没有凭据就整个跳过改写（见下）。
	// 绝不能把服务端兜底的 API Key 塞进返回给客户端的 URL。
	if token != "" {
		q.Set("api_key", token)
		q.Set("ApiKey", token) // Jellyfin 10.11 起认这个写法
	}
	if userID != "" {
		q.Set("UserId", userID)
	}
	return "/Videos/" + url.PathEscape(itemID) + "/stream?" + q.Encode()
}

// isOurStrmSource 判断一个媒体源是不是本站生成的 .strm。
//
// 复用 strmTarget 的解析：它只认路径里带 /d/ 且后面有 id 的地址，
// 别家的 http 直链、本地真实视频文件都不会被误判成我们的。
func isOurStrmSource(p string) bool {
	p = strings.TrimSpace(p)
	if p == "" {
		return false
	}
	if strings.HasSuffix(strings.ToLower(p), ".strm") {
		return true
	}
	_, ok := strmTarget(p)
	return ok
}

// queryFold 取参数名忽略大小写的第一个值（参考 115-station 的 embyClientToken）。
func queryFold(r *http.Request, key string) string {
	if r == nil {
		return ""
	}
	for k, vals := range r.URL.Query() {
		if strings.EqualFold(k, key) && len(vals) > 0 {
			return strings.TrimSpace(vals[0])
		}
	}
	return ""
}

// requestToken 只取「客户端自己带过来」的凭据，不含服务端兜底 Key。
// 参考 115-station：第三方客户端可能用 Bearer、Jellyfin 的 ApiKey 查询参数，
// 或者把 token 塞在 MediaBrowser 复合头里，只认 X-Emby-Token 会漏掉一大半。
func requestToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, v := range []string{
		r.Header.Get("X-Emby-Token"),
		r.Header.Get("X-MediaBrowser-Token"),
		queryFold(r, "api_key"),
		queryFold(r, "apikey"), // Jellyfin 10.11 起不带下划线
	} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	for _, v := range []string{
		r.Header.Get("Authorization"),
		r.Header.Get("X-Emby-Authorization"),
	} {
		if v = strings.TrimSpace(v); v == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(v), "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		if m := reAuthToken.FindStringSubmatch(v); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

func requestUserID(r *http.Request) string {
	if r == nil {
		return ""
	}
	if id := queryFold(r, "userid"); id != "" {
		return id
	}
	for _, v := range []string{
		r.Header.Get("X-Emby-Authorization"),
		r.Header.Get("Authorization"),
	} {
		if m := reUserID.FindStringSubmatch(v); len(m) == 2 {
			return m[1]
		}
	}
	return ""
}

// rewritePlaybackInfo 是 ReverseProxy.ModifyResponse：只处理 PlaybackInfo 的
// 200 JSON 响应，其余一律原样放回。任何异常都恢复原始 body 后返回，
// 绝不因为改写失败让播放器连 Emby 都连不上。
func (g *Gateway) rewritePlaybackInfo(resp *http.Response) error {
	if resp == nil || resp.Request == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	m := rePlaybackInfo.FindStringSubmatch(resp.Request.URL.Path)
	if m == nil {
		return nil
	}
	start := time.Now()
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return nil
	}
	// New() 里已经对 PlaybackInfo 关掉压缩；真遇到压缩响应就别硬解。
	if resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	token := requestToken(resp.Request)
	if token == "" {
		// 没带凭据的请求（Emby 内部探测、匿名会话）改了反而会让后续取流 401。
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if err != nil {
		return err
	}
	restore := func() { resp.Body = io.NopCloser(bytes.NewReader(body)) }
	var root map[string]any
	if json.Unmarshal(body, &root) != nil {
		restore()
		return nil
	}
	if code, ok := root["ErrorCode"]; ok && code != nil && code != "" {
		restore()
		return nil
	}
	sources, _ := root["MediaSources"].([]any)
	if len(sources) == 0 {
		restore()
		return nil
	}

	base, hasBase := baseFromContext(resp.Request.Context())
	mode := ParseMode(string(g.opt.Mode))
	userID := requestUserID(resp.Request)
	ua := resp.Request.UserAgent()

	changed, withDirect := 0, 0
	for _, s := range sources {
		ms, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if infinite, _ := ms["IsInfiniteStream"].(bool); infinite {
			continue
		}
		p, _ := ms["Path"].(string)
		// PlaybackInfo 里的 Path 有两种写法，都得认：
		//   /media/电影/F1.mkv.strm                    （Emby 直接给 .strm 文件）
		//   http://192.168.x.x:6086/d/xxx.mkv?/片名   （Emby 把 .strm 内容当远程源，
		//                                             实测本机就是这种，且没有 DirectStreamUrl）
		// 只认后缀的话第二种会整个漏掉，客户端照旧去请求 /Videos/.../stream 走中转。
		if !isOurStrmSource(p) {
			continue
		}
		sourceID, _ := ms["Id"].(string)
		streamURL := playbackStreamURL(m[1], sourceID, token, userID)

		// 两家共有的优点：此刻就知道 pickcode，先把直链换好（取流时命中缓存），
		// 并从 .strm 内容里读出真实容器，别让客户端按 "strm" 猜格式。
		raw, pick, container := g.sourceDirectURL(p)
		if pick != "" {
			g.prewarmPick(pick, ua)
		}

		// STRMhub 那一路：把 .strm 里写死的内网地址换成「客户端此刻访问的」地址。
		// 算不出来（Path 是本地文件但读不到、Host 不可信）就自动退回 115-station
		// 的相对入口 —— 播放不会因为开了新模式而挂掉。
		direct := ""
		if mode != ModeRedirect && hasBase {
			direct, _ = normalizeDirectURL(raw, base)
		}
		if direct != "" {
			withDirect++
		}

		ms["DirectStreamUrl"] = streamURL
		switch {
		case direct != "" && mode == ModeDirect:
			ms["Path"] = direct
			delete(ms, "DirectStreamUrl")
			ms["SupportsDirectPlay"] = true
			ms["SupportsDirectStream"] = false
		case direct != "" && mode == ModeHybrid:
			ms["Path"] = direct
			ms["SupportsDirectPlay"] = true
			ms["SupportsDirectStream"] = true
		default:
			ms["Path"] = streamURL
			ms["SupportsDirectPlay"] = false
			ms["SupportsDirectStream"] = true
		}
		ms["Protocol"] = "Http"
		ms["IsRemote"] = true
		ms["SupportsTranscoding"] = false
		ms["AddApiKeyToDirectStreamUrl"] = false
		if c := container; c != "" && c != "strm" {
			ms["Container"] = c
		}
		// 留着 TranscodingUrl 会让 SenPlayer 这类客户端挑 HLS：那时它拿到的
		// 是 Emby 转出来的 m3u8，302 网关按设计不接管（分片不是原始文件）。
		for _, k := range []string{
			"TranscodingUrl", "TranscodingContainer", "TranscodingSubProtocol",
			"RequiredHttpHeaders",
		} {
			delete(ms, k)
		}
		changed++
	}
	// 无论有没有改写都留一行摘要：用户说「还是走了中转」时，一眼能看出
	// 是媒体源没认出来，还是改写没生效（参考项目都这么做）。
	g.log.Info("PlaybackInfo 改写摘要", "item", m[1], "mode", mode,
		"sources", len(sources), "rewritten", changed, "direct", withDirect,
		"ms", time.Since(start).Milliseconds())
	if changed == 0 {
		restore()
		return nil
	}

	out, err := json.Marshal(root)
	if err != nil {
		restore()
		return nil
	}
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	resp.Header.Set("Content-Length", strconv.Itoa(len(out)))
	resp.Header.Del("Content-Encoding")
	return nil
}

// prefetchOnItemDetail 是 STRMhub 的「详情页预取」：用户打开详情到按下播放
// 之间通常有好几秒，这段时间足够把网盘直链换好；PlaybackInfo 阶段再预热
// 就只剩缓存命中。只触发后台取链，不改响应体一个字节。
func (g *Gateway) prefetchOnItemDetail(resp *http.Response) error {
	if resp == nil || resp.Request == nil || resp.StatusCode != http.StatusOK {
		return nil
	}
	if resp.Request.Method != http.MethodGet ||
		!reItemDetail.MatchString(resp.Request.URL.Path) ||
		rePlaybackInfo.MatchString(resp.Request.URL.Path) {
		return nil
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") ||
		resp.Header.Get("Content-Encoding") != "" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	if err != nil {
		return err
	}
	var d struct {
		Path string `json:"Path"`
	}
	if json.Unmarshal(body, &d) != nil || !isOurStrmSource(d.Path) {
		return nil
	}
	if _, pick, _ := g.sourceDirectURL(d.Path); pick != "" {
		g.prewarmPick(pick, resp.Request.UserAgent())
	}
	return nil
}
