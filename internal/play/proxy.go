// Package play 实现 :6086 上的播放入口：/d/{pickcode} → 302 到 115 CDN。
//
// 服务端只经手一次重定向，媒体字节直接从 115 到播放器——这是整个方案能跑在
// 一台小 NAS 上的唯一原因。所以这里有一条铁律：
//
//	**302 失败时不许悄悄退化成服务端中转。**
//
// 退化了不会报错，只会让 NAS 的上行带宽在几分钟内被吃满，而用户完全不知道
// 发生了什么。要中转必须是用户显式打开的开关，不能是失败后的默认行为。
//
// 另一条铁律：**客户端的 UA 必须原样透传给网盘，空的也照样透传。**
// 115 的 CDN 直链是按申请时的 UA 签名的，凭空补一个 UA 等于签错票——
// 我们自己 302 得很开心，客户端拿到的却是 403 invalid signature。
package play

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"strm115/internal/pan"
	"strm115/internal/strm"
)

// ManagerProvider 按账号取一个受控的网盘客户端。
type ManagerProvider func(ctx context.Context, accountID int64) (*pan.Manager, error)

// AccountResolver 给出 pickcode 属于哪个账号。
// 查不到时返回 (0, false)，由 Proxy 用兜底账号。
type AccountResolver func(ctx context.Context, pickCode string) (int64, bool)

// KindResolver 按网盘类型给出一个可用账号（115 / quarktv）。
//
// 台账里查不到时用它：.strm 里的 q/ 前缀已经说明了这是夸克的文件，
// 那就该拿夸克账号去换直链，而不是把 fid 交给 115 去碰运气。
type KindResolver func(ctx context.Context, kind string) (int64, bool)

type Proxy struct {
	open     ManagerProvider
	resolve  AccountResolver
	byKind   KindResolver
	fallback int64
	log      *slog.Logger
	cache    *linkCache
	limiter  *ipLimiter
}

func New(open ManagerProvider, resolve AccountResolver, byKind KindResolver, fallbackAccount int64, log *slog.Logger) *Proxy {
	if log == nil {
		log = slog.Default()
	}
	return &Proxy{
		open:     open,
		resolve:  resolve,
		byKind:   byKind,
		fallback: fallbackAccount,
		log:      log,
		cache:    newLinkCache(20 * time.Minute),
		limiter:  newIPLimiter(30, time.Minute),
	}
}

// Handler 返回 :6086 的完整路由。
func (p *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/d/", p.handleDirect)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})
	return mux
}

func (p *Proxy) handleDirect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	id := strings.TrimPrefix(r.URL.Path, "/d/")
	id = strings.Trim(id, "/")
	if id == "" {
		http.Error(w, "missing pickcode", http.StatusBadRequest)
		return
	}
	pickCode := stripExt(id)

	// UA 原样透传，空就是空 —— 千万不能编一个。
	//
	// 115 的直链按「申请时的 UA」签名，而链接最终是被**客户端**拿去下载的。
	// 这里一旦兜底成 "Mozilla/5.0"，等于用 A 的 UA 签了张票给 B 用，
	// CDN 直接 403 invalid signature。Emby 的 HttpClient 抓 .strm 时不发 UA，
	// 就是这个坑：302 明明发对了，字节却永远下不来。
	ua := r.UserAgent()

	// 播放代理原先一行访问日志都没有：播不出来的时候只能去翻 Emby 的日志，
	// 而 Emby 只记「403」，看不到我们这边是谁、带了什么 UA、命没命中缓存。
	// UA 是这条链路上唯一会变的变量（115 的签名绑 UA），必须记下来。
	rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
	start := time.Now()
	hit := false
	defer func() {
		uaLog := ua
		if uaLog == "" {
			uaLog = "<无>" // Emby 这类客户端不发 UA，别让日志看起来像漏了字段
		}
		// 命中缓存的 302 记到 Debug：一次播放会连发几十个 Range 探测，
		// 全是 cache=true / 0ms 的行，会把真正的故障埋掉。
		logf := p.log.Info
		if hit {
			logf = p.log.Debug
		}
		logf("播放请求",
			"pickcode", pickCode,
			"ua", uaLog,
			"ip", clientIP(r),
			"cache", hit,
			"status", rec.code,
			"ms", time.Since(start).Milliseconds())
	}()
	w = rec

	// 命中缓存不扣限流额度：播放器起播会连发多次 Range 探测，
	// 每次都计数的话几秒钟就能把自己的额度用光。
	if u, ok := p.cache.get(pickCode, ua); ok {
		hit = true
		redirect(w, r, u)
		return
	}

	ip := clientIP(r)
	if !p.limiter.allow(ip) {
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	url, err := p.Resolve(ctx, pickCode, ua)
	if err != nil {
		p.log.Warn("取直链失败", "pickcode", pickCode, "err", err)
		http.Error(w, friendly(err), statusFor(err))
		return
	}
	redirect(w, r, url)
}

// Resolve 把 .strm 里的 id 换成网盘直链（已经按 ua 签过名）。
//
// 除了 :6086 的 /d/ 入口，Emby 302 网关也走这里：网关从媒体服务器查出条目的
// .strm 内容后，拿同一个 id 来换直链，然后把客户端 302 到 CDN —— 视频字节
// 完全不经过 Emby 所在的那台机器。缓存与限流状态都由这个 Proxy 统一持有，
// 所以两条入口拿到的是同一批直链，不会各签各的。
func (p *Proxy) Resolve(ctx context.Context, id, ua string) (string, error) {
	pickCode := stripExt(id)
	kind, fileID := strm.SplitPlayKind(pickCode)

	if u, ok := p.cache.get(pickCode, ua); ok {
		return u, nil
	}

	accountID := int64(0)
	if p.resolve != nil {
		if a, ok := p.resolve(ctx, fileID); ok {
			accountID = a
		}
	}
	if accountID == 0 && kind != "" && p.byKind != nil {
		if a, ok := p.byKind(ctx, kind); ok {
			accountID = a
		}
	}
	if accountID == 0 && kind == "" {
		accountID = p.fallback
	}
	if accountID == 0 {
		if kind == "quarktv" {
			return "", pan.NewErr(pan.ErrAuth, "取直链", "还没有添加夸克账号，请先在管理后台登录夸克", nil)
		}
		return "", pan.NewErr(pan.ErrAuth, "取直链", "还没有添加网盘账号，请先在管理后台扫码登录 115", nil)
	}

	mgr, err := p.open(ctx, accountID)
	if err != nil {
		p.log.Error("取网盘客户端失败", "pickcode", pickCode, "account", accountID, "err", err)
		return "", err
	}
	// Manager 是池化对象，不在这里 Close（节流状态要复用）

	url, err := mgr.DownloadURL(ctx, fileID, ua)
	if err != nil {
		return "", err
	}
	p.cache.put(pickCode, ua, url)
	return url, nil
}

func redirect(w http.ResponseWriter, r *http.Request, url string) {
	// Referer / Origin 一律清掉：CDN 对跨站来源可能拒绝
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, url, http.StatusFound)
}

// stripExt 去掉直链路径里的扩展名：/d/abc123.mkv → abc123
// 扩展名只是给播放器认类型的装饰，115 要的是纯 pickcode。
func stripExt(id string) string {
	ext := strm.Ext(id)
	if ext != "" && strm.IsVideo("x"+ext) {
		return strings.TrimSuffix(id, ext)
	}
	return id
}

// statusRecorder 只记状态码。播放代理不需要写响应体的统计（它不写响应体，
// 只发 302），所以不用包 Write。
type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (w *statusRecorder) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

func clientIP(r *http.Request) string {
	// 不信任 X-Forwarded-For：那是客户端可以随便写的。
	// 真的部署在反代后面时，再显式配置可信代理。
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func statusFor(err error) int {
	switch pan.KindOf(err) {
	case pan.ErrCooling:
		return http.StatusServiceUnavailable
	case pan.ErrRisk:
		return http.StatusTooManyRequests
	case pan.ErrAuth:
		return http.StatusForbidden
	case pan.ErrNotFound:
		return http.StatusNotFound
	default:
		return http.StatusBadGateway
	}
}

func friendly(err error) string {
	switch pan.KindOf(err) {
	case pan.ErrCooling:
		return "网盘接口冷却中，请稍后再试"
	case pan.ErrRisk:
		return "被网盘限流，请稍后再试"
	case pan.ErrAuth:
		return "网盘登录已失效，请重新登录"
	case pan.ErrNotFound:
		return "文件不存在或已失效"
	default:
		return "取直链失败：" + err.Error()
	}
}

// ==================== 直链缓存 ====================
//
// 缓存键必须含 UA：115 的直链是按请求时的 UA 签发的，
// 用 A 的 UA 取到的链接在 B 上会 403。这个坑踩过一次就会记住。

type cacheEntry struct {
	url     string
	expires time.Time
}

type linkCache struct {
	mu  sync.Mutex
	m   map[string]cacheEntry
	ttl time.Duration
	n   int
}

func newLinkCache(ttl time.Duration) *linkCache {
	return &linkCache{m: make(map[string]cacheEntry, 256), ttl: ttl}
}

func cacheKey(pickCode, ua string) string { return pickCode + "\x00" + ua }

func (c *linkCache) get(pickCode, ua string) (string, bool) {
	k := cacheKey(pickCode, ua)
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[k]
	if !ok {
		return "", false
	}
	if time.Now().After(e.expires) {
		delete(c.m, k)
		return "", false
	}
	return e.url, true
}

func (c *linkCache) put(pickCode, ua, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	// 简单的容量保护：超过 5000 条时清掉所有过期的
	if c.n > 5000 {
		now := time.Now()
		for k, v := range c.m {
			if now.After(v.expires) {
				delete(c.m, k)
			}
		}
		c.n = len(c.m)
	}
	c.m[cacheKey(pickCode, ua)] = cacheEntry{url: url, expires: time.Now().Add(c.ttl)}
}

// ==================== 每 IP 限流 ====================

type ipLimiter struct {
	mu     sync.Mutex
	bucket map[string]*ipBucket
	limit  int
	window time.Duration
}

type ipBucket struct {
	n     int
	reset time.Time
}

func newIPLimiter(limit int, window time.Duration) *ipLimiter {
	return &ipLimiter{bucket: make(map[string]*ipBucket, 64), limit: limit, window: window}
}

func (l *ipLimiter) allow(ip string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.bucket[ip]
	if !ok || now.After(b.reset) {
		l.bucket[ip] = &ipBucket{n: 1, reset: now.Add(l.window)}
		if len(l.bucket) > 4096 {
			for k, v := range l.bucket {
				if now.After(v.reset) {
					delete(l.bucket, k)
				}
			}
		}
		return true
	}
	if b.n >= l.limit {
		return false
	}
	b.n++
	return true
}
