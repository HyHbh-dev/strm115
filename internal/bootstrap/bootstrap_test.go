package bootstrap

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

// fakeStrm 模拟管理后台。只实现引导真正会调的那几个接口，
// 用来验证「步骤顺序、幂等判断、警告文案」这些编排逻辑。
type fakeStrm struct {
	mu         sync.Mutex
	settings   map[string]string
	accounts   []account
	libraries  []library
	attached   []int64
	syncStarts []int64
	libSeq     int64
}

func newFakeStrm() *fakeStrm {
	return &fakeStrm{settings: map[string]string{}}
}

func (f *fakeStrm) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/session", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"authed": true})
	})
	mux.HandleFunc("POST /api/login", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		cp := map[string]string{}
		for k, v := range f.settings {
			cp[k] = v
		}
		ok(w, cp)
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		for k, v := range in {
			f.settings[k] = v
		}
		f.mu.Unlock()
		ok(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/accounts", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok(w, f.accounts)
	})
	mux.HandleFunc("POST /api/115/cookie", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		_ = json.NewDecoder(r.Body).Decode(&in)
		if in["cookie"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"请粘贴 Cookie"}`))
			return
		}
		f.mu.Lock()
		a := account{ID: int64(len(f.accounts) + 1), Kind: "115", Name: in["name"]}
		f.accounts = append(f.accounts, a)
		f.mu.Unlock()
		ok(w, a)
	})
	mux.HandleFunc("GET /api/accounts/{id}/browse", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"entries": []browseEntry{
			{ID: "cid-movies", Name: "电影", IsDir: true},
			{ID: "cid-tv", Name: "剧集", IsDir: true},
			{ID: "file-1", Name: "readme.txt", IsDir: false},
		}})
	})
	mux.HandleFunc("GET /api/libraries", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		ok(w, f.libraries)
	})
	mux.HandleFunc("POST /api/libraries", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		f.mu.Lock()
		f.libSeq++
		l := library{
			ID:        f.libSeq,
			AccountID: int64(in["account_id"].(float64)),
			Name:      in["name"].(string),
			RemoteCID: in["remote_cid"].(string),
			LocalPath: "/media/" + in["name"].(string),
		}
		f.libraries = append(f.libraries, l)
		f.mu.Unlock()
		ok(w, l)
	})
	mux.HandleFunc("GET /api/libraries/{id}/job", func(w http.ResponseWriter, r *http.Request) {
		ok(w, progress{Running: false, Phase: "done", Files: 3, Written: 3})
	})
	mux.HandleFunc("POST /api/libraries/{id}/sync", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.syncStarts = append(f.syncStarts, 1)
		f.mu.Unlock()
		ok(w, progress{Running: true, Phase: "scan"})
	})
	mux.HandleFunc("POST /api/libraries/{id}/server", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.attached = append(f.attached, 1)
		f.mu.Unlock()
		ok(w, map[string]any{"server": "emby", "server_path": "/media/115", "server_lib_id": "42"})
	})
	mux.HandleFunc("POST /api/settings/test-server", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"ok": true})
	})
	return mux
}

func ok(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// fakeEmbyReady 只做两件事：报告就绪、报告向导已完成。
// 有了现成的 API Key 时引导不需要登录 Emby，这两个接口就够。
func fakeEmbyReady() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/System/Info/Public", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]string{"ServerName": "fake", "Version": "4.10.2.0"})
	})
	mux.HandleFunc("/Startup/Configuration", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	return mux
}

func TestRunFullChain(t *testing.T) {
	fs := newFakeStrm()
	strmSrv := httptest.NewServer(fs.handler())
	defer strmSrv.Close()
	embySrv := httptest.NewServer(fakeEmbyReady())
	defer embySrv.Close()

	cfg := Config{
		StrmURL:    strmSrv.URL,
		PlayDomain: "http://192.168.1.10:6086",
		EmbyURL:    embySrv.URL,
		EmbyKey:    "given-key",
		EmbyApp:    "strm115",
		WaitEmby:   3 * time.Second,
		WaitSync:   3 * time.Second,
		Timeout:    20 * time.Second,
		Cookie:     "UID=1;CID=2;SEID=3;KID=4",
		AccountNm:  "115",
		LibName:    "115",
		LibCID:     "3555355939475293750",
		LibType:    "movies",
		KeepExt:    true,
	}

	rep, err := Run(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if got := fs.settings["play_domain"]; got != "http://192.168.1.10:6086" {
		t.Errorf("play_domain = %q", got)
	}
	if got := fs.settings["server_key"]; got != "given-key" {
		t.Errorf("server_key = %q", got)
	}
	if got := fs.settings["server_url"]; got != embySrv.URL {
		t.Errorf("server_url = %q", got)
	}
	if len(fs.accounts) != 1 {
		t.Fatalf("应该用 Cookie 加了 1 个账号，实际 %d", len(fs.accounts))
	}
	if len(fs.libraries) != 1 || fs.libraries[0].RemoteCID != "3555355939475293750" {
		t.Fatalf("媒体库创建不正确: %+v", fs.libraries)
	}
	if len(fs.syncStarts) != 1 {
		t.Errorf("同步应该启动 1 次，实际 %d", len(fs.syncStarts))
	}
	if len(fs.attached) != 1 {
		t.Errorf("应该接入 Emby 1 次，实际 %d", len(fs.attached))
	}

	if !rep.EmbyReady || !rep.WizardDone || rep.EmbyKey != "given-key" ||
		rep.AccountID == 0 || rep.LibraryID == 0 || !rep.SyncStarted || !rep.Attached {
		t.Errorf("结果标记不完整: %+v", rep)
	}
}

func TestRunResolvesCIDByName(t *testing.T) {
	fs := newFakeStrm()
	fs.accounts = []account{{ID: 1, Kind: "115", Name: "115"}}
	strmSrv := httptest.NewServer(fs.handler())
	defer strmSrv.Close()

	cfg := Config{
		StrmURL:    strmSrv.URL,
		PlayDomain: "http://192.168.1.10:6086",
		SkipEmby:   true,
		WaitEmby:   100 * time.Millisecond,
		WaitSync:   2 * time.Second,
		Timeout:    10 * time.Second,
		LibName:    "115",
		LibCID:     "电影", // 填名字，引导自己去网上盘根目录查 cid
		LibType:    "movies",
	}
	if _, err := Run(context.Background(), cfg, discardLogger()); err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if len(fs.libraries) != 1 {
		t.Fatalf("应该建出 1 个库，实际 %d", len(fs.libraries))
	}
	if got := fs.libraries[0].RemoteCID; got != "cid-movies" {
		t.Errorf("按名称解析出的 cid = %q，期望 cid-movies", got)
	}
}

func TestRunIsIdempotent(t *testing.T) {
	fs := newFakeStrm()
	// 预置：账号和库都已经存在（模拟第二次执行 docker compose run）
	fs.accounts = []account{{ID: 1, Kind: "115", Name: "115"}}
	fs.libraries = []library{{ID: 1, Name: "115", RemoteCID: "cid-movies", LocalPath: "/media/115"}}

	strmSrv := httptest.NewServer(fs.handler())
	defer strmSrv.Close()

	cfg := Config{
		StrmURL:    strmSrv.URL,
		PlayDomain: "http://192.168.1.10:6086",
		EmbyURL:    "http://127.0.0.1:1", // 故意不可达
		EmbyKey:    "given-key",
		WaitEmby:   300 * time.Millisecond,
		WaitSync:   2 * time.Second,
		Timeout:    15 * time.Second,
		Cookie:     "UID=1;CID=2;SEID=3;KID=4",
		LibName:    "115",
		LibCID:     "cid-movies",
		LibType:    "movies",
	}

	rep, err := Run(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()

	if len(fs.accounts) != 1 {
		t.Errorf("已有账号时不应重复添加，实际 %d 个", len(fs.accounts))
	}
	if len(fs.libraries) != 1 {
		t.Errorf("已有同名库时不应重复创建，实际 %d 个", len(fs.libraries))
	}
	// Emby 不可达时必须降级为警告，而不是整条链失败
	if rep.EmbyReady {
		t.Error("Emby 不可达时 EmbyReady 应为 false")
	}
	if len(rep.Warnings) == 0 {
		t.Error("Emby 不可达应该产生警告")
	}
}

func TestRunWithoutCookieListsFolders(t *testing.T) {
	fs := newFakeStrm()
	fs.accounts = []account{{ID: 1, Kind: "115", Name: "115"}}

	strmSrv := httptest.NewServer(fs.handler())
	defer strmSrv.Close()

	cfg := Config{
		StrmURL:  strmSrv.URL,
		EmbyKey:  "",
		EmbyURL:  "http://127.0.0.1:1",
		SkipEmby: true,
		WaitEmby: 100 * time.Millisecond,
		WaitSync: time.Second,
		Timeout:  10 * time.Second,
		LibName:  "115",
	}

	rep, err := Run(context.Background(), cfg, discardLogger())
	if err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	if rep.LibraryID != 0 {
		t.Error("没有 cid 时不应建库")
	}
	// 至少要有「没选目录」这条警告
	found := false
	for _, w := range rep.Warnings {
		if strings.Contains(w, "PAN115_CID") {
			found = true
		}
	}
	if !found {
		t.Errorf("应提示需要填 PAN115_CID，实际警告: %v", rep.Warnings)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("BOOTSTRAP_STRM_URL", "")
	t.Setenv("EMBY_URL", "")
	t.Setenv("LIBRARY_TYPE", "nonsense")
	t.Setenv("BOOTSTRAP_WAIT_SYNC", "45")

	cfg := LoadConfig()
	if cfg.StrmURL != "http://strm115:6060" {
		t.Errorf("StrmURL 默认值 = %q", cfg.StrmURL)
	}
	if cfg.EmbyURL != "http://emby:8096" {
		t.Errorf("EmbyURL 默认值 = %q", cfg.EmbyURL)
	}
	if cfg.LibType != "movies" {
		t.Errorf("非法 LIBRARY_TYPE 应回落到 movies，实际 %q", cfg.LibType)
	}
	if cfg.WaitSync != 45*time.Second {
		t.Errorf("WaitSync = %v", cfg.WaitSync)
	}
	if !cfg.KeepExt {
		t.Error("KeepExt 默认应为 true")
	}
}

func TestPlayDomainNormalized(t *testing.T) {
	t.Setenv("PLAY_DOMAIN", "192.168.1.10:6086/")
	cfg := LoadConfig()
	if cfg.PlayDomain != "http://192.168.1.10:6086" {
		t.Errorf("PLAY_DOMAIN 应补全 scheme 并去掉尾斜杠，实际 %q", cfg.PlayDomain)
	}
}

func TestTruncate(t *testing.T) {
	if got := truncate("电影", 10); got != "电影" {
		t.Errorf("短名不该被截断: %q", got)
	}
	if got := truncate("一二三四五六", 4); got != "一二三…" {
		t.Errorf("按字符截断失败: %q", got)
	}
}
