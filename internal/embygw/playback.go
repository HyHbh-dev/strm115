package embygw

// 改写 PlaybackInfo：让所有客户端统一走本站的 302 入口。
//
// 为什么不能只靠「猜客户端会发什么 stream 路径」：Emby Web 发
// /emby/videos/{id}/stream?static=true，SenPlayer 发
// /emby/videos/{id}/stream.mkv?...&Static=true（参数名大写 S），Infuse 又可能
// 省掉 /emby 前缀、加上 MediaSourceId。每漏一种，那次播放就静默退回 Emby
// 自己读 .strm 中转字节。日志里「播放请求未接管」记下的正是这些变体。
//
// 参考 115-station / STRMhub（同源的 MediaWarp 思路）：不追着客户端跑，而是在
// PlaybackInfo 的响应里把媒体源的 DirectStreamUrl 改成本站入口，并关掉转码。
// 播放器拿到什么地址是**我们说了算**，之后不管它是 Emby Web、SenPlayer、
// Infuse 还是 iOS，都会来请求同一个 /Videos/{id}/stream?Static=true。
//
// 与 115-station 的差别：它们把地址指到自己专门注册的 302 handler；我们直接
// 复用网关已有的 streamItemID 分支 —— 同一个 URL 被网关接管时 302 到 CDN，
// 万一没接管（解析失败 / 条目不是我们的 .strm）就原样反代给 Emby，Emby 认得
// 这个官方路径，最差只是回到中转，不会播不了。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// playbackStreamURL 是写进 PlaybackInfo 的相对地址。
//
// 用相对路径：客户端沿用自己正在访问的协议 / 域名 / 反代前缀（公网走
// cloudflared、局域网走 6086 都对），不去读不可信的 X-Forwarded-Host。
// Static 故意用官方那种首字母大写的写法 —— 反正网关现在大小写都不敏感。
func playbackStreamURL(itemID, sourceID, token string) string {
	q := url.Values{"Static": {"true"}}
	if sourceID != "" {
		q.Set("MediaSourceId", sourceID)
	}
	// 客户端自己的凭据回给它自己；没有凭据就整个跳过改写（见下）。
	// 绝不能把服务端兜底的 API Key 塞进返回给客户端的 URL。
	if token != "" {
		q.Set("api_key", token)
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

// requestToken 只取「客户端自己带过来」的凭据，不含服务端兜底 Key。
func requestToken(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, v := range []string{
		r.Header.Get("X-Emby-Token"),
		r.URL.Query().Get("api_key"),
		r.Header.Get("X-MediaBrowser-Token"),
	} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	for _, v := range []string{
		r.Header.Get("X-Emby-Authorization"),
		r.Header.Get("Authorization"),
	} {
		if m := reAuthToken.FindStringSubmatch(v); len(m) == 2 {
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

	changed := 0
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
		streamURL := playbackStreamURL(m[1], sourceID, token)
		ms["Path"] = streamURL
		ms["DirectStreamUrl"] = streamURL
		ms["Protocol"] = "Http"
		ms["IsRemote"] = true
		ms["SupportsDirectPlay"] = false
		ms["SupportsDirectStream"] = true
		ms["SupportsTranscoding"] = false
		ms["AddApiKeyToDirectStreamUrl"] = false
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
	g.log.Info("PlaybackInfo 已改写为 302 入口", "item", m[1], "sources", changed)
	return nil
}
