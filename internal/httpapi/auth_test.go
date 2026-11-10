package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strm115/internal/config"
	"strm115/internal/mediaserver"
	"strm115/internal/pan"
	"strm115/internal/panpool"
	"strm115/internal/store"
	syncsvc "strm115/internal/sync"
)

// ==================== 纯函数 ====================

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("s3cret-pass")
	if err != nil {
		t.Fatalf("哈希失败: %v", err)
	}
	if strings.Contains(h, "s3cret-pass") {
		t.Fatal("哈希串里不能出现明文密码")
	}
	if !verifyPassword(h, "s3cret-pass") {
		t.Fatal("正确密码应该通过校验")
	}
	if verifyPassword(h, "s3cret-pass ") {
		t.Fatal("错误密码不该通过")
	}
	// 同一个密码两次哈希必须不同（salt 随机），否则等于查表就能破
	h2, _ := hashPassword("s3cret-pass")
	if h == h2 {
		t.Fatal("两次哈希结果相同，salt 没生效")
	}
}

func TestVerifyPasswordRejectsGarbage(t *testing.T) {
	bad := []string{"", "plain", "pbkdf2-sha256$0$aa$bb", "pbkdf2-sha256$x$aa$bb", "md5$1$aa$bb"}
	for _, b := range bad {
		if verifyPassword(b, "whatever") {
			t.Errorf("%q 不该被当成合法哈希", b)
		}
	}
}

func TestValidateCredInput(t *testing.T) {
	if _, err := validateCredInput("admin", "123456"); err != nil {
		t.Fatalf("合法输入被拒: %v", err)
	}
	cases := []struct{ user, pass string }{
		{"", "123456"},
		{"a b", "123456"},
		{strings.Repeat("x", 33), "123456"},
		{"admin", "12345"},
	}
	for _, c := range cases {
		if _, err := validateCredInput(c.user, c.pass); err == nil {
			t.Errorf("user=%q pass=%q 应该被拒绝", c.user, c.pass)
		}
	}
}

// ==================== 接口流程 ====================

func newTestServer(t *testing.T) http.Handler {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	pool := panpool.New(st, pan.Options{MinGap: time.Millisecond, MaxAttempts: 1})
	t.Cleanup(pool.CloseAll)

	syncSvc := syncsvc.New(st, pool.Get,
		func(context.Context) (mediaserver.Server, error) { return nil, errors.New("无媒体服务器") },
		slog.Default())

	return New(Deps{
		Cfg:   config.Config{Addr: ":0", DataDir: dir, AdminUser: "admin"},
		Store: st,
		Pool:  pool,
		Sync:  syncSvc,
		Log:   slog.Default(),
	}).Handler()
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any, cookies []*http.Cookie) (*httptest.ResponseRecorder, []*http.Cookie) {
	t.Helper()
	var rdr *strings.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = strings.NewReader(string(b))
	} else {
		rdr = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, rec.Result().Cookies()
}

func TestSetupLoginChangePassword(t *testing.T) {
	h := newTestServer(t)

	// 1. 没初始化时：会话接口如实报告，业务接口一律拒绝
	rec, _ := doJSON(t, h, http.MethodGet, "/api/session", nil, nil)
	var sess map[string]any
	json.Unmarshal(rec.Body.Bytes(), &sess)
	if sess["setup_needed"] != true || sess["authed"] != false {
		t.Fatalf("未初始化状态不对: %v", sess)
	}
	if rec, _ := doJSON(t, h, http.MethodGet, "/api/libraries", nil, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("未初始化时业务接口应 401，实际 %d", rec.Code)
	}

	// 2. 弱密码被拒
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/setup", map[string]string{"username": "admin", "password": "123"}, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("短密码应 400，实际 %d", rec.Code)
	}

	// 3. 完成初始化并拿到会话
	rec, cookies := doJSON(t, h, http.MethodPost, "/api/setup",
		map[string]string{"username": "boss", "password": "goodpass1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("初始化失败 %d: %s", rec.Code, rec.Body.String())
	}
	if len(cookies) == 0 {
		t.Fatal("初始化后应该下发会话 cookie")
	}

	// 4. 带 cookie 能访问业务接口
	if rec, _ := doJSON(t, h, http.MethodGet, "/api/libraries", nil, cookies); rec.Code != http.StatusOK {
		t.Fatalf("登录后应 200，实际 %d", rec.Code)
	}

	// 5. 不能重复初始化（否则任何人都能改密码）
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/setup",
		map[string]string{"username": "evil", "password": "hacked123"}, nil); rec.Code != http.StatusConflict {
		t.Fatalf("重复初始化应 409，实际 %d", rec.Code)
	}

	// 6. 登录：错账号、错密码都不行
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/login",
		map[string]string{"username": "boss", "password": "wrongpass"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应 401，实际 %d", rec.Code)
	}
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/login",
		map[string]string{"username": "other", "password": "goodpass1"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("错账号应 401，实际 %d", rec.Code)
	}

	// 7. 改密码：当前密码错了要拒绝
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/credentials",
		map[string]string{"current_password": "nope", "username": "boss", "password": "newpass22"}, cookies); rec.Code != http.StatusForbidden {
		t.Fatalf("当前密码不对应 403，实际 %d", rec.Code)
	}

	// 8. 改成功后：新密码可登录，旧会话失效
	rec, newCookies := doJSON(t, h, http.MethodPost, "/api/credentials",
		map[string]string{"current_password": "goodpass1", "username": "boss", "password": "newpass22"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("改密码失败 %d: %s", rec.Code, rec.Body.String())
	}
	if rec, _ := doJSON(t, h, http.MethodGet, "/api/settings", nil, cookies); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧会话应该失效，实际 %d", rec.Code)
	}
	if rec, _ := doJSON(t, h, http.MethodGet, "/api/settings", nil, newCookies); rec.Code != http.StatusOK {
		t.Fatalf("新会话应可用，实际 %d", rec.Code)
	}
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/login",
		map[string]string{"username": "boss", "password": "goodpass1"}, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("旧密码应失效，实际 %d", rec.Code)
	}
	if rec, _ := doJSON(t, h, http.MethodPost, "/api/login",
		map[string]string{"username": "boss", "password": "newpass22"}, nil); rec.Code != http.StatusOK {
		t.Fatalf("新密码应能登录，实际 %d", rec.Code)
	}
}

func TestEnvSeedsAdminOnce(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "seed.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	defer st.Close()
	pool := panpool.New(st, pan.Options{MinGap: time.Millisecond, MaxAttempts: 1})
	defer pool.CloseAll()
	syncSvc := syncsvc.New(st, pool.Get,
		func(context.Context) (mediaserver.Server, error) { return nil, errors.New("无") }, slog.Default())

	mk := func(pass string) *Server {
		return New(Deps{
			Cfg:   config.Config{Addr: ":0", DataDir: dir, AdminUser: "auto", AdminPass: pass},
			Store: st, Pool: pool, Sync: syncSvc, Log: slog.Default(),
		})
	}

	s := mk("fromenv1")
	if !s.credsReady() || s.currentUser() != "auto" {
		t.Fatal("环境变量应在首次启动时初始化管理员账号")
	}
	// 第二个进程（模拟重启）用不同的环境变量：数据库里的账号不能被覆盖
	s2 := mk("another1")
	if !s2.credsReady() || s2.currentUser() != "auto" {
		t.Fatal("重启后应沿用数据库里的账号")
	}
	if verifyPassword(s2.credHash, "another1") {
		t.Fatal("环境变量不该覆盖数据库里已有的密码")
	}
	if !verifyPassword(s2.credHash, "fromenv1") {
		t.Fatal("数据库里的密码应该仍然有效")
	}
}
