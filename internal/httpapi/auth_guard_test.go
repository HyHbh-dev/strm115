package httpapi

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"strm115/internal/config"
)

func TestLoginGuardBlocksAfterFiveFails(t *testing.T) {
	g := newLoginGuard()
	now := time.Now()
	const key = "10.0.0.9"

	for i := 0; i < loginMaxFails-1; i++ {
		g.fail(key, now)
	}
	if d := g.blocked(key, now); d != 0 {
		t.Fatalf("第 %d 次失败不该被锁，实际还要等 %v", loginMaxFails-1, d)
	}
	g.fail(key, now)
	if d := g.blocked(key, now); d <= 0 {
		t.Fatal("累计到阈值后应被锁")
	}
	if d := g.blocked(key, now.Add(loginBlockFor+time.Second)); d != 0 {
		t.Fatalf("封锁到期后应放行，实际 %v", d)
	}
	g.clear(key)
	if d := g.blocked(key, now); d != 0 {
		t.Fatal("登录成功后应清空该来源的失败记录")
	}

	// 窗口之外的失败重新计数，不会把很久以前的一次失败一直记着。
	g.fail(key, now)
	g.fail(key, now.Add(loginWindow+time.Second))
	if d := g.blocked(key, now.Add(loginWindow+time.Second)); d != 0 {
		t.Fatalf("窗口外的旧失败不该累计，实际锁 %v", d)
	}
}

func TestCrossSiteWrite(t *testing.T) {
	cases := []struct {
		name, method, origin, host, xfh string
		want                            bool
	}{
		{"同源 POST", http.MethodPost, "http://127.0.0.1:6060", "127.0.0.1:6060", "", false},
		{"跨站 POST", http.MethodPost, "https://evil.example", "127.0.0.1:6060", "", true},
		{"curl 不带 Origin", http.MethodPut, "", "127.0.0.1:6060", "", false},
		{"GET 不拦", http.MethodGet, "https://evil.example", "127.0.0.1:6060", "", false},
		{"HEAD 不拦", http.MethodHead, "https://evil.example", "127.0.0.1:6060", "", false},
		{"反代借 X-Forwarded-Host", http.MethodPost, "https://admin.example.com", "strm115:6060", "admin.example.com", false},
		{"反代但来源不符", http.MethodPost, "https://evil.example", "strm115:6060", "admin.example.com", true},
		{"Origin null", http.MethodPost, "null", "127.0.0.1:6060", "", true},
		{"大小写不敏感", http.MethodDelete, "http://LOCALHOST:6060", "localhost:6060", "", false},
	}
	for _, c := range cases {
		r := httptest.NewRequest(c.method, "http://"+c.host+"/api/subscriptions", nil)
		r.Host = c.host
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.xfh != "" {
			r.Header.Set("X-Forwarded-Host", c.xfh)
		}
		if got := crossSiteWrite(r); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestSessionCookieSecureFlag(t *testing.T) {
	for _, secure := range []bool{false, true} {
		s := &Server{cfg: config.Config{CookieSecure: secure}, sess: newSessionStore(sessionTTL)}
		w := httptest.NewRecorder()
		s.setSessionCookie(w, "tok")
		cookies := w.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("secure=%v: 期望 1 个 cookie，实际 %d", secure, len(cookies))
		}
		c := cookies[0]
		if c.Secure != secure || !c.HttpOnly || c.Value != "tok" || c.MaxAge <= 0 {
			t.Errorf("secure=%v: cookie 异常 %+v", secure, c)
		}
		w2 := httptest.NewRecorder()
		s.clearSessionCookie(w2)
		if cc := w2.Result().Cookies()[0]; cc.MaxAge != -1 || cc.Secure != secure {
			t.Errorf("secure=%v: 清除 cookie 异常 %+v", secure, cc)
		}
	}
}

func TestMiddlewareRejectsCrossSiteWrite(t *testing.T) {
	s := &Server{log: slog.Default(), sess: newSessionStore(sessionTTL), login: newLoginGuard()}
	h := s.middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	bad := httptest.NewRequest(http.MethodPost, "http://admin.example.com/api/subscriptions", nil)
	bad.Host = "admin.example.com"
	bad.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != http.StatusForbidden {
		t.Fatalf("跨站写请求应 403，实际 %d", w.Code)
	}
	if got := w.Header().Get("X-Frame-Options"); got != "SAMEORIGIN" {
		t.Errorf("缺少 X-Frame-Options，实际 %q", got)
	}

	// 不带 Origin 的同源/脚本请求不该被 CSRF 闸误伤（这里会因未登录拿到 401，
	// 只要不是 403 就说明没被跨站逻辑拦下）。
	ok := httptest.NewRequest(http.MethodPost, "http://admin.example.com/api/subscriptions", nil)
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, ok)
	if w2.Code == http.StatusForbidden {
		t.Fatal("不带 Origin 的请求被误判为跨站")
	}
}
