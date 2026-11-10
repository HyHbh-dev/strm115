package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeEmby 只实现「连接媒体服务器」这条链路用到的接口。
// 状态码照抄真实 Emby 4.10 的实测结果（见 internal/mediaserver/emby_admin.go）。
type fakeEmby struct {
	mu       sync.Mutex
	user     string
	pass     string
	keys     []string
	wizardUp bool // true = 还没跑完向导
}

func newFakeEmby(t *testing.T) (*fakeEmby, *httptest.Server) {
	f := &fakeEmby{user: "testuser", pass: "emby-pass-1"}
	mux := http.NewServeMux()

	mux.HandleFunc("GET /System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"ServerName": "fake-emby", "Version": "4.10.2.0", "Id": "abc"})
	})
	mux.HandleFunc("GET /System/Info", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeTestJSON(w, map[string]any{"ServerName": "fake-emby", "Version": "4.10.2.0"})
	})
	mux.HandleFunc("GET /Startup/Configuration", func(w http.ResponseWriter, r *http.Request) {
		if f.wizardUp {
			writeTestJSON(w, map[string]any{"UICulture": "en-US"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized) // 已初始化
	})
	mux.HandleFunc("POST /Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var in struct{ Username, Pw string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in.Username != f.user || in.Pw != f.pass {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Header.Get("X-Emby-Authorization") == "" {
			w.WriteHeader(http.StatusBadRequest) // 真实 Emby 的行为：缺头是 400 不是 401
			return
		}
		writeTestJSON(w, map[string]any{"AccessToken": "admin-token", "User": map[string]string{"Id": "u1"}})
	})
	mux.HandleFunc("GET /Auth/Keys", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		items := make([]map[string]string, 0, len(f.keys))
		for _, k := range f.keys {
			items = append(items, map[string]string{"AppName": "strm115", "AccessToken": k})
		}
		f.mu.Unlock()
		writeTestJSON(w, map[string]any{"Items": items, "TotalRecordCount": len(items)})
	})
	mux.HandleFunc("POST /Auth/Keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "admin-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		f.mu.Lock()
		f.keys = append(f.keys, "key-"+strings.Repeat("x", 5))
		n := len(f.keys)
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent) // 实测：204 且无 body
		_ = n
	})
	mux.HandleFunc("GET /Library/VirtualFolders", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, []map[string]any{})
	})
	mux.HandleFunc("GET /Library/MediaFolders", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{}})
	})
	mux.HandleFunc("POST /Library/Refresh", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func writeTestJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// TestConnectServer 覆盖「网页里一键连接」：登录 Emby → 建 Key → 落库 → 状态可读。
func TestConnectServer(t *testing.T) {
	fake, embySrv := newFakeEmby(t)
	h := newTestServer(t)
	cookies := loginForTest(t, h)

	// 还没配时状态如实报告
	rec, _ := doJSON(t, h, http.MethodGet, "/api/settings/server-status", nil, cookies)
	var st map[string]any
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st["configured"] != false || st["connected"] != false {
		t.Fatalf("未配置状态不对: %v", st)
	}

	// 密码错了要明确失败，且不能把错的 key 存进去
	rec, _ = doJSON(t, h, http.MethodPost, "/api/settings/connect-server", map[string]string{
		"url": embySrv.URL, "username": "testuser", "password": "wrong",
	}, cookies)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("错密码应 401，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 正确密码
	rec, _ = doJSON(t, h, http.MethodPost, "/api/settings/connect-server", map[string]string{
		"url": embySrv.URL, "username": "testuser", "password": "emby-pass-1",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("连接失败 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["server_name"] != "fake-emby" {
		t.Fatalf("没拿到服务器信息: %v", out)
	}
	if out["key_preview"] == "" || strings.Contains(out["key_preview"].(string), "xxx") == false {
		t.Fatalf("key 预览不对: %v", out["key_preview"])
	}

	// 状态变成「已连接」
	rec, _ = doJSON(t, h, http.MethodGet, "/api/settings/server-status", nil, cookies)
	json.Unmarshal(rec.Body.Bytes(), &st)
	if st["configured"] != true || st["connected"] != true {
		t.Fatalf("连接后状态不对: %v", st)
	}

	// 设置接口不能把 API Key 明文回传给浏览器
	rec, _ = doJSON(t, h, http.MethodGet, "/api/settings", nil, cookies)
	var sets map[string]string
	json.Unmarshal(rec.Body.Bytes(), &sets)
	if sets["server_key"] != "" {
		t.Fatalf("不该回传明文 key: %q", sets["server_key"])
	}
	if sets["server_key_set"] != "1" {
		t.Fatalf("应该标记已配置: %v", sets)
	}

	// 再连一次要复用同一个 Key，不能每次都新建
	rec, _ = doJSON(t, h, http.MethodPost, "/api/settings/connect-server", map[string]string{
		"url": embySrv.URL, "username": "testuser", "password": "emby-pass-1",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("重复连接失败 %d: %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	n := len(fake.keys)
	fake.mu.Unlock()
	if n != 1 {
		t.Fatalf("API Key 应该只有 1 个（幂等），实际 %d", n)
	}
}

func loginForTest(t *testing.T, h http.Handler) []*http.Cookie {
	t.Helper()
	rec, cookies := doJSON(t, h, http.MethodPost, "/api/setup",
		map[string]string{"username": "admin", "password": "testpass1"}, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("初始化失败 %d: %s", rec.Code, rec.Body.String())
	}
	if len(cookies) == 0 {
		t.Fatal("没拿到会话 cookie")
	}
	return cookies
}
