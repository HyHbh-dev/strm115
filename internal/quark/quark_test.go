package quark

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeQuark 是网页接口的最小假实现：token → detail → save → task 四步。
type fakeQuark struct {
	tokenCalls  atomic.Int32
	saveBody    atomic.Value
	taskPolls   atomic.Int32
	q           atomic.Value // 最近一次 query
	cookie      atomic.Value
	failCookie  atomic.Bool
	failPass    atomic.Bool
	shareTitle  string
	srv         *httptest.Server
	saveFailMsg string
}

func newFake(t *testing.T) *fakeQuark {
	t.Helper()
	f := &fakeQuark{shareTitle: "老友记 全十季"}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.q.Store(r.URL.Path + "?" + r.URL.RawQuery)
		f.cookie.Store(r.Header.Get("Cookie"))
		if f.failCookie.Load() {
			_, _ = w.Write([]byte(`{"status":200,"code":31001,"message":"请先登录"}`))
			return
		}
		if r.Header.Get("Referer") == "" {
			t.Errorf("%s 缺 Referer，夸克会挡", r.URL.Path)
		}
		switch r.URL.Path {
		case "/member":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"nickname":"老王"}}`))
		case "/share/sharepage/token":
			f.tokenCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["pwd_id"] == "" {
				t.Error("token 没带 pwd_id")
			}
			if f.failPass.Load() {
				_, _ = w.Write([]byte(`{"status":200,"code":41008,"message":"提取码错误"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"stoken":"ST1","title":"` + f.shareTitle + `"}}`))
		case "/share/sharepage/detail":
			if r.URL.Query().Get("stoken") != "ST1" {
				t.Error("detail 没带 stoken")
			}
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{
				"count":2,"share":{"title":"` + f.shareTitle + `"},
				"list":[
					{"fid":"s1","file_name":"S01","dir":true,"share_fid_token":"t1"},
					{"fid":"s2","file_name":"S02","dir":true,"share_fid_token":"t2"}]}}`))
		case "/share/sharepage/save":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			raw, _ := json.Marshal(body)
			f.saveBody.Store(string(raw))
			if f.saveFailMsg != "" {
				_, _ = w.Write([]byte(`{"status":200,"code":41010,"message":"` + f.saveFailMsg + `"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"task_id":"T1"}}`))
		case "/task":
			n := f.taskPolls.Add(1)
			if n < 2 {
				_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"status":1,"task_status":1}}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"status":2,"task_status":2}}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func TestParseShare(t *testing.T) {
	cases := []struct {
		in       string
		ok       bool
		id, pass string
	}{
		{"https://pan.quark.cn/s/abcdef123456", true, "abcdef123456", ""},
		{"https://pan.quark.cn/s/abcdef123456?pwd=1234", true, "abcdef123456", "1234"},
		{"复制这段内容后打开夸克网盘，链接：pan.quark.cn/s/abc123 提取码：8x9y", true, "abc123", "8x9y"},
		{"看看这个 https://pan.quark.cn/s/zzz，提取码:ab12", true, "zzz", "ab12"},
		{"https://pan.baidu.com/s/1abc", false, "", ""},
		{"夸克网盘 没有链接", false, "", ""},
	}
	for _, c := range cases {
		got, ok := ParseShare(c.in)
		if ok != c.ok {
			t.Errorf("%q 解析结果 ok=%v，想 %v", c.in, ok, c.ok)
			continue
		}
		if !ok {
			continue
		}
		if got.PwdID != c.id || got.Passcode != c.pass {
			t.Errorf("%q → id=%q pass=%q，想 id=%q pass=%q", c.in, got.PwdID, got.Passcode, c.id, c.pass)
		}
	}
}

func TestSaveShareFlow(t *testing.T) {
	f := newFake(t)
	c := NewWithBase("__puus=abc; __pus=def", f.srv.URL)
	res, err := c.SaveShare(context.Background(), "https://pan.quark.cn/s/abc123?pwd=1234", "", "dir9")
	if err != nil {
		t.Fatal(err)
	}
	if res.Title != "老友记 全十季" || res.Files != 2 {
		t.Errorf("结果不对: %+v", res)
	}
	if f.tokenCalls.Load() != 1 {
		t.Errorf("token 应该只换一次，实际 %d", f.tokenCalls.Load())
	}
	if f.taskPolls.Load() < 2 {
		t.Errorf("任务应该被轮询到完成，实际 %d 次", f.taskPolls.Load())
	}
	raw, _ := f.saveBody.Load().(string)
	for _, want := range []string{`"to_pdir_fid":"dir9"`, `"fid_list":["s1","s2"]`, `"stoken":"ST1"`, `"scene":"link"`} {
		if !strings.Contains(raw, want) {
			t.Errorf("save 请求少了 %s：%s", want, raw)
		}
	}
	ck, _ := f.cookie.Load().(string)
	if !strings.Contains(ck, "__puus=abc") {
		t.Errorf("Cookie 没带上: %q", ck)
	}
	q, _ := f.q.Load().(string)
	if !strings.Contains(q, "pr=ucpro") || !strings.Contains(q, "fr=pc") {
		t.Errorf("缺 pr/fr 参数: %s", q)
	}
}

func TestSaveShareCookieInvalid(t *testing.T) {
	f := newFake(t)
	f.failCookie.Store(true)
	c := NewWithBase("bad", f.srv.URL)
	_, err := c.SaveShare(context.Background(), "https://pan.quark.cn/s/abc", "", "")
	if !errors.Is(err, ErrCookie) {
		t.Errorf("Cookie 失效要归到 ErrCookie，实际 %v", err)
	}
}

func TestSaveSharePasscodeWrong(t *testing.T) {
	f := newFake(t)
	f.failPass.Store(true)
	c := NewWithBase("ok", f.srv.URL)
	_, err := c.SaveShare(context.Background(), "https://pan.quark.cn/s/abc", "9999", "")
	if !errors.Is(err, ErrShareInvalid) {
		t.Errorf("提取码错误要归到 ErrShareInvalid，实际 %v", err)
	}
}

func TestSaveShareRejectsNonQuarkLink(t *testing.T) {
	f := newFake(t)
	c := NewWithBase("ok", f.srv.URL)
	_, err := c.SaveShare(context.Background(), "https://pan.baidu.com/s/1x", "", "")
	if !errors.Is(err, ErrShareInvalid) {
		t.Errorf("非夸克链接要归到 ErrShareInvalid，实际 %v", err)
	}
	if f.tokenCalls.Load() != 0 {
		t.Error("认不出链接就不该发请求")
	}
}

func TestSaveShareWithoutCookie(t *testing.T) {
	c := New("")
	if _, err := c.SaveShare(context.Background(), "https://pan.quark.cn/s/abc", "", ""); !errors.Is(err, ErrCookie) {
		t.Errorf("没配 Cookie 要归到 ErrCookie，实际 %v", err)
	}
	if err := c.Ping(context.Background()); !errors.Is(err, ErrCookie) {
		t.Errorf("Ping 也该报 ErrCookie，实际 %v", err)
	}
}

func TestSaveShareSaveRejected(t *testing.T) {
	f := newFake(t)
	f.saveFailMsg = "空间不足"
	c := NewWithBase("ok", f.srv.URL)
	_, err := c.SaveShare(context.Background(), "https://pan.quark.cn/s/abc", "", "")
	if err == nil || !strings.Contains(err.Error(), "空间不足") {
		t.Errorf("转存失败要原样透出夸克原话，实际 %v", err)
	}
}

func TestPing(t *testing.T) {
	f := newFake(t)
	c := NewWithBase("ok", f.srv.URL)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !c.Configured() {
		t.Error("有 Cookie 时 Configured 应该是 true")
	}
}

// ==================== Cookie 保活 ====================

// newKeepServer 扮夸克网页网关：/config 与 /member 都会下发新的 __puus，
// reissue=false 时故意不下发，用来验「没轮换」不算故障。
func newKeepServer(t *testing.T, reissue bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if reissue {
			http.SetCookie(w, &http.Cookie{Name: "__puus", Value: "NEW_PUUS", Path: "/"})
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{}}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func TestRefreshPuusStripsOldAndPersistsNew(t *testing.T) {
	srv, calls := newKeepServer(t, true)
	c := NewWithBase("__pus=P; __puus=OLD; __uid=U; __kp=K", srv.URL)
	fresh, err := c.RefreshPuus(t.Context())
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("该只打一次 /config，实际 %d", calls.Load())
	}
	m := parseCookies(fresh)
	if m["__puus"] != "NEW_PUUS" {
		t.Errorf("新的 __puus 没写进 Cookie: %q", fresh)
	}
	// 身份字段必须原样保留，只换会话字段。
	if m["__pus"] != "P" || m["__uid"] != "U" || m["__kp"] != "K" {
		t.Errorf("身份字段被弄丢了: %q", fresh)
	}
	if c.Cookie() != fresh {
		t.Errorf("客户端自己的 Cookie 没同步: %q vs %q", c.Cookie(), fresh)
	}
}

func TestRefreshPuusNoReissueIsNotAnError(t *testing.T) {
	srv, _ := newKeepServer(t, false)
	c := NewWithBase("__pus=P; __puus=OLD", srv.URL)
	_, err := c.RefreshPuus(t.Context())
	if !errors.Is(err, ErrNoRefresh) {
		t.Fatalf("没下发新值时该返回 ErrNoRefresh，实际 %v", err)
	}
	// 失败不能把原 Cookie 弄坏。
	if got := parseCookies(c.Cookie())["__puus"]; got != "OLD" {
		t.Errorf("原 Cookie 被覆盖了: %q", got)
	}
}

func TestMergeResponseCookiesOnNormalRequest(t *testing.T) {
	srv, _ := newKeepServer(t, true)
	c := NewWithBase("__pus=P; __puus=OLD; __uid=U", srv.URL)
	if err := c.Ping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := parseCookies(c.Cookie())["__puus"]; got != "NEW_PUUS" {
		t.Errorf("普通请求的 Set-Cookie 没被收下: %q", got)
	}
	if got := parseCookies(c.Cookie())["__uid"]; got != "U" {
		t.Errorf("无关字段不该被改: %q", got)
	}
}

func TestParseJoinCookies(t *testing.T) {
	m := parseCookies(" __pus = A ; ; __puus=B; __uid=C ")
	if m["__pus"] != "A" || m["__puus"] != "B" || m["__uid"] != "C" {
		t.Fatalf("解析不对: %+v", m)
	}
	if got := joinCookies(m); got != "__pus=A; __puus=B; __uid=C" {
		t.Errorf("拼回去不对（应排序且不带多余空格）: %q", got)
	}
}
