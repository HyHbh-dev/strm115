package mediaserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeEmby 模拟 Emby 4.10.2 在真机上观察到的行为，包括那几个反直觉的点：
//   - 向导未完成时 /Startup/Configuration 返回 200，完成后返回 401
//   - POST /Auth/Keys 返回 204 且**没有响应体**，key 只能从 GET 列表里找
//   - GET /Auth/Keys 的结果是 {"Items":[...]} 而不是裸数组
type fakeEmby struct {
	mu           sync.Mutex
	wizardDone   bool
	keys         []apiKey
	createKeyHit int // POST /Auth/Keys 被调用了几次
	authHit      int
	lastQuery    map[string]string
}

func (f *fakeEmby) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, 200, map[string]string{
			"ServerName": "fake-emby", "Version": "4.10.2.0", "Id": "abc",
		})
	})

	mux.HandleFunc("/Startup/Configuration", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			f.mu.Lock()
			done := f.wizardDone
			f.mu.Unlock()
			if done {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte("Access token is invalid or expired."))
				return
			}
			writeTestJSON(w, 200, map[string]string{"UICulture": "en-us"})
			return
		}
		f.recordQuery(r)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/Startup/User", func(w http.ResponseWriter, r *http.Request) {
		f.recordQuery(r)
		writeTestJSON(w, 200, map[string]any{})
	})

	mux.HandleFunc("/Startup/RemoteAccess", func(w http.ResponseWriter, r *http.Request) {
		f.recordQuery(r)
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/Startup/Complete", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.wizardDone = true
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	mux.HandleFunc("/Users/AuthenticateByName", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Username string `json:"Username"`
			Pw       string `json:"Pw"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.authHit++
		f.mu.Unlock()
		if in.Username != "admin" || in.Pw != "secret" {
			w.WriteHeader(http.StatusUnauthorized)
			writeTestJSON(w, 401, map[string]string{"error": "bad credentials"})
			return
		}
		// 登录不认 X-Emby-Token，只认 X-Emby-Authorization
		if r.Header.Get("X-Emby-Authorization") == "" {
			writeTestJSON(w, 400, map[string]string{"error": "missing client header"})
			return
		}
		writeTestJSON(w, 200, map[string]any{
			"AccessToken": "user-token-123",
			"User":        map[string]string{"Id": "u1"},
		})
	})

	mux.HandleFunc("/Auth/Keys", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("X-Emby-Token") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeTestJSON(w, 200, map[string]any{"Items": f.keys, "TotalRecordCount": len(f.keys)})
		case http.MethodPost:
			f.createKeyHit++
			f.keys = append(f.keys, apiKey{
				AppName:     r.URL.Query().Get("App"),
				AccessToken: "api-key-" + r.URL.Query().Get("App"),
			})
			w.WriteHeader(http.StatusNoContent) // 真机就是 204 无 body
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}

func (f *fakeEmby) recordQuery(r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lastQuery == nil {
		f.lastQuery = map[string]string{}
	}
	for k := range r.URL.Query() {
		f.lastQuery[k] = r.URL.Query().Get(k)
	}
}

func writeTestJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func TestNormalizeBase(t *testing.T) {
	cases := []struct{ in, want string }{
		{"http://192.168.1.10:8096", "http://192.168.1.10:8096"},
		{"http://192.168.1.10:8096/", "http://192.168.1.10:8096"},
		{"http://192.168.1.10:8096/emby", "http://192.168.1.10:8096"},
		{"192.168.1.10:8096", "http://192.168.1.10:8096"},
		{"  emby:8096  ", "http://emby:8096"},
		{"https://emby.example.com/emby/", "https://emby.example.com"},
	}
	for _, c := range cases {
		if got := NormalizeBase(c.in); got != c.want {
			t.Errorf("NormalizeBase(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestWizardAndAPIKeyFlow(t *testing.T) {
	f := &fakeEmby{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	a := NewAdmin(srv.URL)
	ctx := context.Background()

	done, err := a.WizardCompleted(ctx)
	if err != nil {
		t.Fatalf("检查向导状态失败: %v", err)
	}
	if done {
		t.Fatal("全新 Emby 应该报告向导未完成")
	}

	if err := a.CompleteWizard(ctx, "admin", "secret", "zh-CN", "CN"); err != nil {
		t.Fatalf("完成向导失败: %v", err)
	}
	if f.lastQuery["name"] != "admin" || f.lastQuery["UICulture"] != "zh-CN" {
		t.Errorf("向导参数没传对: %+v", f.lastQuery)
	}

	// 完成之后必须能识别出来，否则会重复走一遍向导
	done, err = a.WizardCompleted(ctx)
	if err != nil {
		t.Fatalf("复查向导状态失败: %v", err)
	}
	if !done {
		t.Fatal("向导完成后应报告已完成")
	}

	token, err := a.Authenticate(ctx, "admin", "secret")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if token != "user-token-123" {
		t.Fatalf("token = %q", token)
	}

	key, err := a.EnsureAPIKey(ctx, token, "strm115")
	if err != nil {
		t.Fatalf("创建 API Key 失败: %v", err)
	}
	if key != "api-key-strm115" {
		t.Fatalf("key = %q，期望从列表里回读到 api-key-strm115", key)
	}

	// 幂等：第二次调用不能再建一个
	key2, err := a.EnsureAPIKey(ctx, token, "strm115")
	if err != nil {
		t.Fatalf("第二次取 API Key 失败: %v", err)
	}
	if key2 != key {
		t.Fatalf("重复调用返回了不同的 key: %q vs %q", key, key2)
	}
	if f.createKeyHit != 1 {
		t.Fatalf("POST /Auth/Keys 被调用 %d 次，应该只建一次", f.createKeyHit)
	}
	if len(f.keys) != 1 {
		t.Fatalf("API Key 列表有 %d 项，应该只有 1 项", len(f.keys))
	}
}

func TestAuthenticateWrongPassword(t *testing.T) {
	f := &fakeEmby{wizardDone: true}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	a := NewAdmin(srv.URL)
	_, err := a.Authenticate(context.Background(), "admin", "wrong")
	if err == nil {
		t.Fatal("错误密码应该返回 error")
	}
	if !strings.Contains(err.Error(), "401") {
		t.Errorf("错误信息应包含状态码，实际: %v", err)
	}
}

func TestEnsureAPIKeyReportsMissing(t *testing.T) {
	// 服务端接受创建请求但列表里始终没有对应项 —— 说明接口行为和预期不符，
	// 必须显式报错，不能返回空 key 让上层拿空字符串去用。
	mux := http.NewServeMux()
	mux.HandleFunc("/Auth/Keys", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		writeTestJSON(w, 200, map[string]any{"Items": []apiKey{}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	a := NewAdmin(srv.URL)
	_, err := a.EnsureAPIKey(context.Background(), "token", "strm115")
	if err == nil {
		t.Fatal("列表里找不到 key 时必须报错")
	}
}

func TestWaitReadyTimeout(t *testing.T) {
	// 端口能连上但服务没起来（一直 502）时，WaitReady 应该超时而不是立刻成功
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	a := NewAdmin(srv.URL)
	_, err := a.WaitReady(context.Background(), 200*time.Millisecond)
	if err == nil {
		t.Fatal("服务不可用时应超时报错")
	}
}
