package httpapi

// 登录限速与跨站写请求校验。
//
// 这两件事放在 middleware / handler 层而不是反代层，是因为这个项目既能
// 直接裸跑（局域网 http://IP:6060），也会挂 Caddy；只靠网关保护的话，
// 裸跑形态就完全没有防爆破和防跨站能力。

import (
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	loginMaxFails   = 5
	loginWindow     = 5 * time.Minute
	loginBlockFor   = 5 * time.Minute
	loginGuardSweep = 10 * time.Minute
)

type loginAttempt struct {
	fails        int
	first        time.Time
	blockedUntil time.Time
}

type loginGuard struct {
	mu   sync.Mutex
	byIP map[string]*loginAttempt
}

func newLoginGuard() *loginGuard {
	return &loginGuard{byIP: map[string]*loginAttempt{}}
}

// blocked 返回还要等多久；0 表示可以继续尝试。
func (g *loginGuard) blocked(key string, now time.Time) time.Duration {
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.byIP[key]
	if a == nil {
		return 0
	}
	if now.Before(a.blockedUntil) {
		return a.blockedUntil.Sub(now)
	}
	return 0
}

// fail 记一次失败：同一窗口内累计到阈值就封锁一段时间。
func (g *loginGuard) fail(key string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sweep(now)
	a := g.byIP[key]
	if a == nil {
		a = &loginAttempt{first: now}
		g.byIP[key] = a
	}
	if now.Sub(a.first) > loginWindow {
		a.fails, a.first = 0, now
	}
	a.fails++
	if a.fails >= loginMaxFails {
		a.blockedUntil = now.Add(loginBlockFor)
	}
}

func (g *loginGuard) clear(key string) {
	g.mu.Lock()
	delete(g.byIP, key)
	g.mu.Unlock()
}

// sweep 顺手清掉老条目，避免长跑进程里这张表无限增长（调用方持锁）。
func (g *loginGuard) sweep(now time.Time) {
	for k, a := range g.byIP {
		if now.Sub(a.first) > loginGuardSweep && now.After(a.blockedUntil) {
			delete(g.byIP, k)
		}
	}
}

// clientKey 只用 TCP 对端地址，不信任 X-Forwarded-For（可伪造）。
// 挂了反代时所有请求共享同一个 IP 的配额：单管理员后台够用，
// 真要按真实客户端限速应该在前置网关做，而不是让应用猜代理头。
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}

// crossSiteWrite 只拦「浏览器发出的、带 Origin 且来源不匹配」的写请求。
//
// 为什么不带 Origin 就放行：curl / 脚本 / 服务端回调本来就不发 Origin，
// 拦了会误伤所有非浏览器客户端；而浏览器跨站 POST 一定会带 Origin，
// 配合 SameSite=Lax 已经能挡掉绝大多数 CSRF，这里是补上同站子域那一类。
func crossSiteWrite(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || strings.EqualFold(origin, "null") {
		return true
	}
	host := r.Host
	if fh := r.Header.Get("X-Forwarded-Host"); fh != "" {
		host = strings.TrimSpace(strings.Split(fh, ",")[0])
	}
	return !strings.EqualFold(u.Host, host)
}
