package embygw

// 播放地址策略：把 115-station 与 STRMhub 两套做法合成可切换的三档。
//
//	redirect  115-station：PlaybackInfo 统一改写成相对 /Videos/{id}/stream，
//	          由网关 302 到网盘 CDN。相对地址天然跟随客户端正在访问的
//	          协议 / 域名 / 反代前缀，兼容性最好，也是默认值。
//	hybrid    两家合并：Path 给「按客户端 host 归一化后的 /d/ 直链」，
//	          DirectStreamUrl 仍留统一 302 入口。支持 DirectPlay 的客户端
//	          直接找播放代理（少一跳、完全不碰 Emby），其余客户端走备用入口。
//	direct    STRMhub：只给 /d/ 直链并关掉 DirectStream，逼播放器自己直连。
//
// 两个参考项目的差别只在「改写 PlaybackInfo 之后把 Path 指向哪里」：
// 115-station 指自己的 stream handler，STRMhub 指 /d/ 端点。它们共同的
// 前提都是「别让 Emby 去读 .strm」，所以三档都不会让视频字节经过 Emby。

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
)

// PlayMode 是设置项 emby_gateway_mode 的取值。
type PlayMode string

const (
	ModeRedirect PlayMode = "redirect"
	ModeHybrid   PlayMode = "hybrid"
	ModeDirect   PlayMode = "direct"
)

// ParseMode 把设置里的字符串归一成模式，空 / 拼错一律回默认（最稳的那档）。
func ParseMode(s string) PlayMode {
	switch PlayMode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeHybrid:
		return ModeHybrid
	case ModeDirect:
		return ModeDirect
	default:
		return ModeRedirect
	}
}

// clientBase 是「客户端此刻正在访问的协议 + host」。
//
// STRMhub 的 normalizeDirectURL 用的是配置里的域名；但 .strm 里写的是
// http://192.168.1.10:6086 这种**内网地址**，公网客户端拿到根本连不上。
// 所以这里按请求推导：客户端访问 https://emby.example.com 就拿到 https 的
// /d/ 地址，局域网访问 192.168.x.x:6086 就拿到同端口的地址。
type clientBase struct {
	Scheme string
	Host   string
}

type clientBaseKey struct{}

// withClientBase 把原始入站请求的访问地址记进 context。
//
// 必须在 ReverseProxy 之前做：ModifyResponse 拿到的 resp.Request 是**发往
// 上游**的请求，Director 已经把 Host 改成了 Emby 的地址，从那里读不到
// 客户端真正访问的域名。ReverseProxy 的 req.Clone 会保留 context。
func withClientBase(r *http.Request) *http.Request {
	b, ok := clientBaseOf(r)
	if !ok {
		return r
	}
	return r.WithContext(context.WithValue(r.Context(), clientBaseKey{}, b))
}

func baseFromContext(ctx context.Context) (clientBase, bool) {
	b, ok := ctx.Value(clientBaseKey{}).(clientBase)
	return b, ok && b.Scheme != "" && b.Host != ""
}

// clientBaseOf 推导客户端的访问地址。
//
// 反代场景下 r.Host 常常被改成后端地址，这时才用 X-Forwarded-Host；
// 优先用 r.Host 是因为它不依赖客户端可伪造的头。两者都做字符白名单校验，
// 避免把带路径 / 用户信息的怪值拼进返回给播放器的绝对 URL。
func clientBaseOf(r *http.Request) (clientBase, bool) {
	if r == nil {
		return clientBase{}, false
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	} else if p := strings.TrimSpace(firstCSV(r.Header.Get("X-Forwarded-Proto"))); strings.EqualFold(p, "https") {
		scheme = "https"
	}

	host := strings.TrimSpace(r.Host)
	fwd := strings.TrimSpace(firstCSV(r.Header.Get("X-Forwarded-Host")))
	// Host 形如 127.0.0.1:6086 / localhost / 内网 IP，而转发头给的是公网域名：
	// 说明请求经过了隧道或反代，播放地址得按公网那套算。
	if isLocalHost(host) && validHost(fwd) && !isLocalHost(fwd) {
		host = fwd
	}
	if !validHost(host) {
		return clientBase{}, false
	}
	return clientBase{Scheme: scheme, Host: host}, true
}

func firstCSV(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i]
	}
	return s
}

// validHost 只放字母数字、点、连字符、冒号和 IPv6 方括号。
func validHost(h string) bool {
	if h == "" || len(h) > 253 || strings.ContainsAny(h, "/\\@?# \t\r\n") {
		return false
	}
	for _, c := range h {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			continue
		}
		switch c {
		case '.', '-', ':', '[', ']':
		default:
			return false
		}
	}
	return true
}

// isLocalHost 认本机 / 内网地址，用来判断 Host 是不是被反代改写过。
func isLocalHost(h string) bool {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return true
	}
	if i := strings.LastIndexByte(h, ':'); i >= 0 && !strings.HasSuffix(h, "]") {
		h = h[:i]
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" || strings.HasSuffix(h, ".localhost") ||
		h == "host.docker.internal" || h == "127.0.0.1" || h == "::1" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast()
}

// normalizeDirectURL 把 .strm 里写死的 scheme://host 换成客户端当前这一套，
// 保留 /d/{id}[.ext] 路径与 ?/片名 这类查询串。
//
// 只认本站播放入口：别家的 http 直链、本地文件路径都不改。
func normalizeDirectURL(raw string, b clientBase) (string, bool) {
	if b.Scheme == "" || b.Host == "" {
		return "", false
	}
	raw = strings.TrimSpace(raw)
	if _, ok := strmTarget(raw); !ok {
		return "", false
	}
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return "", false
	}
	u.Scheme = b.Scheme
	u.Host = b.Host
	u.User = nil
	return u.String(), true
}

// knownContainers 用来防「片名.2025」被当成容器。
var knownContainers = map[string]bool{
	"mkv": true, "mp4": true, "m4v": true, "avi": true, "mov": true,
	"ts": true, "m2ts": true, "mts": true, "wmv": true, "flv": true,
	"rmvb": true, "rm": true, "iso": true, "mpg": true, "mpeg": true,
	"webm": true, "vob": true, "3gp": true, "strm": true,
}

// directContainer 从 .strm 内容或文件名里推出真实容器。
//
// 参考项目都会在改写后把 Container 从 "strm" 改成原始容器：URL 已经变成
// /Videos/.../stream 或 /d/...，客户端再按 "strm" 去猜格式就可能拒绝播放。
func directContainer(raw string) string {
	if i := strings.Index(raw, "?/"); i >= 0 {
		if c := extContainer(raw[i+2:]); c != "" {
			return c
		}
	}
	if u, err := url.Parse(strings.TrimSpace(raw)); err == nil {
		if c := extContainer(u.Path); c != "" {
			return c
		}
	}
	return extContainer(raw)
}

func extContainer(p string) string {
	// 本地 .strm 文件：/media/F1.mkv.strm → 先剥掉 .strm 再看里面那层。
	if strings.HasSuffix(strings.ToLower(p), ".strm") {
		p = p[:len(p)-len(".strm")]
	}
	c := strings.ToLower(strings.TrimPrefix(path.Ext(p), "."))
	if knownContainers[c] {
		return c
	}
	return ""
}

// sourceDirectURL 从 PlaybackInfo 的媒体源 Path 里取出 .strm 指向的播放地址。
//
// Path 有两种写法（实测本机返回第二种）：
//   /media/电影/F1.mkv.strm                  本地文件，要读内容
//   http://192.168.1.10:6086/d/xxx.mkv?/F1  Emby 直接把 .strm 内容当远程源
//
// 返回 (原始 URL, 播放代理的 pickcode, 容器)。取不到就都为空，由调用方
// 降级回统一 302 入口 —— 功能不受影响。
func (g *Gateway) sourceDirectURL(serverPath string) (string, string, string) {
	p := strings.TrimSpace(serverPath)
	if p == "" {
		return "", "", ""
	}
	if _, err := url.Parse(p); err == nil && strings.Contains(p, "://") {
		if pick, ok := strmTarget(p); ok {
			return p, pick, directContainer(p)
		}
		return "", "", ""
	}
	local := p
	if g.opt.MapLocal != nil {
		local = g.opt.MapLocal(p)
	}
	if !strings.HasSuffix(strings.ToLower(local), ".strm") {
		return "", "", ""
	}
	b, err := os.ReadFile(local)
	if err != nil {
		return "", "", ""
	}
	if len(b) > 4096 {
		b = b[:4096]
	}
	raw := strings.TrimSpace(string(b))
	if pick, ok := strmTarget(raw); ok {
		c := directContainer(raw)
		if c == "" {
			c = directContainer(local)
		}
		return raw, pick, c
	}
	return "", "", ""
}
