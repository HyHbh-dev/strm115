package driver115

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	driver "github.com/SheltonZhu/115driver/pkg/driver"

	"strm115/internal/pan"
)

// fake115 扮演离线提交要打的两个端点：取签名 + 提交单条链接。
type fake115 struct {
	mu       sync.Mutex
	forms    []url.Values
	signHits int
	// reply 决定第 n 次提交的回包（n 从 0 开始）
	reply func(n int, form url.Values) (int, string)
}

func (f *fake115) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/space", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.signHits++
		f.mu.Unlock()
		io.WriteString(w, `{"state":true,"sign":"sign-1","time":1794047939,"size":"21.83TB"}`)
	})
	mux.HandleFunc("/add", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("表单解析失败: %v", err)
			return
		}
		f.mu.Lock()
		n := len(f.forms)
		f.forms = append(f.forms, r.PostForm)
		f.mu.Unlock()
		code, body := f.reply(n, r.PostForm)
		w.WriteHeader(code)
		io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldSpace, oldAdd := offlineSpaceURL, offlineAddURL
	offlineSpaceURL = srv.URL + "/space"
	offlineAddURL = srv.URL + "/add"
	t.Cleanup(func() { offlineSpaceURL, offlineAddURL = oldSpace, oldAdd })
	return srv
}

// newTestClient 造一个不联网的客户端：uid 预设好，省掉 GetUser 那次真实请求。
func newTestClient() *Client {
	return &Client{c: driver.New(driver.UA("strm115-test")), uid: 424242}
}

// TestAddOfflineURIsSuccess 覆盖正常提交：签名只取一次、链接去重、参数齐全。
func TestAddOfflineURIsSuccess(t *testing.T) {
	f := &fake115{reply: func(n int, form url.Values) (int, string) {
		return 200, `{"state":true,"errno":0,"info_hash":"hash-` + form.Get("url")[len(form.Get("url"))-1:] + `"}`
	}}
	f.server(t)

	c := newTestClient()
	hashes, err := c.AddOfflineURIs(context.Background(),
		[]string{"  magnet:?xt=urn:btih:aa ", "", "magnet:?xt=urn:btih:aa", "magnet:?xt=urn:btih:bb"},
		"")
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	// 去重后两条，各回一个 hash
	if len(hashes) != 2 {
		t.Fatalf("应返回 2 个 hash，实际 %v", hashes)
	}
	if len(f.forms) != 2 {
		t.Fatalf("去重后应只发 2 次提交，实际 %d 次", len(f.forms))
	}
	if f.signHits != 1 {
		t.Fatalf("sign 应被缓存、只取一次，实际取了 %d 次", f.signHits)
	}

	form := f.forms[0]
	for k, want := range map[string]string{
		"url":        "magnet:?xt=urn:btih:aa",
		"uid":        "424242",
		"sign":       "sign-1",
		"time":       "1794047939",
		"wp_path_id": "0", // 空目录要落成根目录
	} {
		if got := form.Get(k); got != want {
			t.Fatalf("参数 %s 应为 %q，实际 %q", k, want, got)
		}
	}
}

// TestAddOfflineURIsKeepsDirID 指定目录时要原样带过去。
func TestAddOfflineURIsKeepsDirID(t *testing.T) {
	f := &fake115{reply: func(int, url.Values) (int, string) {
		return 200, `{"state":true,"info_hash":"h1"}`
	}}
	f.server(t)

	if _, err := newTestClient().AddOfflineURIs(context.Background(), []string{"ed2k://|file|x|1|h|/"}, "9527"); err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if got := f.forms[0].Get("wp_path_id"); got != "9527" {
		t.Fatalf("保存目录应为 9527，实际 %q", got)
	}
}

// TestAddOfflineURIsNotLogin 掉登录要归成 ErrAuth，并且把缓存的签名作废。
func TestAddOfflineURIsNotLogin(t *testing.T) {
	f := &fake115{reply: func(int, url.Values) (int, string) {
		return 200, `{"state":false,"errno":99,"error_msg":"请重新登录"}`
	}}
	f.server(t)

	c := newTestClient()
	_, err := c.AddOfflineURIs(context.Background(), []string{"magnet:?xt=urn:btih:cc"}, "0")
	if !errors.Is(err, pan.ErrAuth) {
		t.Fatalf("errno 99 应归为 ErrAuth，实际 %v", pan.KindOf(err))
	}
	c.mu.Lock()
	left := c.auth
	c.mu.Unlock()
	if left.sign != "" {
		t.Fatal("掉登录后必须清掉签名缓存，否则下一条还会拿废 sign 去撞")
	}
}

// TestAddOfflineURIsPartial 第一条成功、第二条被拒：已成功的 hash 要交出来。
func TestAddOfflineURIsPartial(t *testing.T) {
	f := &fake115{reply: func(n int, _ url.Values) (int, string) {
		if n == 0 {
			return 200, `{"state":true,"info_hash":" ok-hash "}`
		}
		return 200, `{"state":false,"errno":10008,"error_msg":"链接无效"}`
	}}
	f.server(t)

	hashes, err := newTestClient().AddOfflineURIs(context.Background(),
		[]string{"magnet:?xt=urn:btih:dd", "magnet:?xt=urn:bad"}, "0")
	if err == nil {
		t.Fatal("第二条被拒时必须报错")
	}
	if !errors.Is(err, pan.ErrRejected) {
		t.Fatalf("应归为 ErrRejected，实际 %v", pan.KindOf(err))
	}
	if len(hashes) != 1 {
		t.Fatalf("应返回已提交成功的 1 个 hash，实际 %v", hashes)
	}
}

// TestAddOfflineURIsPlainTextReply 115 历史上会回纯文本（那句 decode fail!），
// 这种响应要变成可读错误，而不是让 JSON 解析的原始报错漏到用户面前。
func TestAddOfflineURIsPlainTextReply(t *testing.T) {
	f := &fake115{reply: func(int, url.Values) (int, string) {
		return 200, "decode fail!"
	}}
	f.server(t)

	_, err := newTestClient().AddOfflineURIs(context.Background(), []string{"magnet:?xt=urn:btih:ee"}, "0")
	if err == nil {
		t.Fatal("纯文本回包应该报错")
	}
	if !strings.Contains(err.Error(), "非 JSON") || !strings.Contains(err.Error(), "decode fail!") {
		t.Fatalf("错误信息应带上原文便于排查，实际 %q", err.Error())
	}
}

// TestAddOfflineURIsNoSign 取不到签名（state=false）说明登录态没了。
func TestAddOfflineURIsNoSign(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"state":false,"error_msg":"请重新登录"}`)
	}))
	defer srv.Close()
	oldSpace := offlineSpaceURL
	offlineSpaceURL = srv.URL
	defer func() { offlineSpaceURL = oldSpace }()

	_, err := newTestClient().AddOfflineURIs(context.Background(), []string{"magnet:?xt=urn:btih:ff"}, "0")
	if !errors.Is(err, pan.ErrAuth) {
		t.Fatalf("取不到签名应归为 ErrAuth，实际 %v", pan.KindOf(err))
	}
}
