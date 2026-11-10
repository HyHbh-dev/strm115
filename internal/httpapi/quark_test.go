package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"strm115/internal/pan/driverquarktv"
)

// withFakeQuarkWeb 把 quark_web_base 指到一个假网页接口，并把 Cookie 换成 known cookie。
//
// 用设置项而不是替身：这条链路真正会出问题的地方就是「读哪份 Cookie、走哪个反代」，
// 绕过设置就等于没测。
func withFakeQuarkWeb(t *testing.T, s *Server) *atomic.Int32 {
	t.Helper()
	var signCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Cookie"), "__puus=good") {
			_, _ = w.Write([]byte(`{"status":200,"code":31001,"message":"请先登录"}`))
			return
		}
		switch r.URL.Path {
		case "/member":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"nickname":"老王"}}`))
		case "/share/sharepage/token":
			signCalls.Add(1)
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"stoken":"ST1","title":"沙丘2"}}`))
		case "/share/sharepage/detail":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{
				"count":1,"share":{"title":"沙丘2"},
				"list":[{"fid":"f1","file_name":"沙丘2.mkv","share_fid_token":"t1"}]}}`))
		case "/share/sharepage/save":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"task_id":"T1"}}`))
		case "/task":
			_, _ = w.Write([]byte(`{"status":200,"code":0,"data":{"status":2,"task_status":2}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if err := s.st.SetSetting(t.Context(), "quark_web_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	return &signCalls
}

// addQuarkAccount 直接往库里塞一个夸克账号（credential 是 TV 侧凭证 + 可选网页 Cookie）。
func addQuarkAccount(t *testing.T, s *Server, webCookie string) int64 {
	t.Helper()
	cred, _ := json.Marshal(driverquarktv.Credential{
		RefreshToken: "RT", DeviceID: "dev-1", WebCookie: webCookie,
	})
	id, err := s.st.CreateAccount(t.Context(), quarkKind, "夸克老王", string(cred))
	if err != nil {
		t.Fatal(err)
	}
	return id
}

// 没配夸克账号时，转存要给出「去配一个」的明确指引，而不是 500。
func TestQuarkShareWithoutAccount(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/quark/share",
		map[string]any{"url": "https://pan.quark.cn/s/abc123"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("该是 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "夸克账号") {
		t.Errorf("提示里要说清缺什么: %s", rec.Body.String())
	}
}

// 扫码登录只管播放，转存没 Cookie 时必须明确说「缺 Cookie」而不是报「分享无效」。
func TestQuarkShareWithoutCookie(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeQuarkWeb(t, s)
	addQuarkAccount(t, s, "")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/quark/share",
		map[string]any{"share_code": "abc123"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("该是 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Cookie") {
		t.Errorf("要提示缺网页 Cookie: %s", rec.Body.String())
	}
}

// 正常一轮：认链接 → 换 stoken → 列表 → 转存 → 等任务，最后回标题与条目数。
func TestQuarkShareSaves(t *testing.T) {
	s, _ := newServerWithStore(t)
	calls := withFakeQuarkWeb(t, s)
	acctID := addQuarkAccount(t, s, "__puus=good; __pus=ok")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/quark/share", map[string]any{
		"url":      "https://pan.quark.cn/s/abc123?pwd=1234",
		"passcode": "1234",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("该是 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK     bool   `json:"ok"`
		Title  string `json:"title"`
		Files  int    `json:"files"`
		Dir    string `json:"dir"`
		Synced bool   `json:"synced"`
		Acct   int64  `json:"account_id"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.Title != "沙丘2" || out.Files != 1 {
		t.Errorf("转存结果不对: %+v", out)
	}
	if out.Acct != acctID {
		t.Errorf("账号 id 不对: %d", out.Acct)
	}
	if out.Dir != "夸克网盘根目录" {
		t.Errorf("没配库时该说根目录，实际 %q", out.Dir)
	}
	if out.Synced {
		t.Error("没有夸克库时不该说同步过")
	}
	if calls.Load() != 1 {
		t.Errorf("token 应该只换一次，实际 %d", calls.Load())
	}
}

// Cookie 失效：401 + 让人能照着做的一句话。
func TestQuarkCookieInvalid(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeQuarkWeb(t, s)
	addQuarkAccount(t, s, "__puus=expired")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/quark/share",
		map[string]any{"share_code": "abc123"}, cookies)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("Cookie 失效该是 401，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "重新粘贴") {
		t.Errorf("要告诉用户怎么修: %s", rec.Body.String())
	}
}

// 贴 Cookie 的接口：先真打一次探测，错的直接拒。
func TestQuarkCookieEndpoint(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeQuarkWeb(t, s)
	acctID := addQuarkAccount(t, s, "")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/accounts/"+strconv.FormatInt(acctID, 10)+"/quark-cookie",
		map[string]string{"cookie": "__puus=bad"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("坏 Cookie 该是 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, h, http.MethodPost, "/api/accounts/"+strconv.FormatInt(acctID, 10)+"/quark-cookie",
		map[string]string{"cookie": "__puus=good"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("好 Cookie 该是 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	// 落库后账号列表要能显示出「已配置」
	rec, _ = doJSON(t, h, http.MethodGet, "/api/accounts", nil, cookies)
	if !strings.Contains(rec.Body.String(), `"quark_cookie_set":true`) {
		t.Errorf("账号列表没标出 Cookie 已配: %s", rec.Body.String())
	}
}

// 扫码登录：authorize → 未确认 → 确认 → 建账号（kind=quarktv）。
func TestQuarkQRLoginCreatesAccount(t *testing.T) {
	s, _ := newServerWithStore(t)
	var confirmed atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oauth/authorize":
			_, _ = w.Write([]byte(`{"status":0,"qr_data":"QUJD","query_token":"QT1"}`))
		case "/oauth/code":
			if !confirmed.Load() {
				_, _ = w.Write([]byte(`{"status":0}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":0,"code":"C1"}`))
		case "/token":
			_, _ = w.Write([]byte(`{"code":200,"data":{"access_token":"AT","refresh_token":"RT1"}}`))
		case "/user":
			_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"nickname":"夸克老王"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	oldBase, oldCode := driverquarktv.DefaultBase, driverquarktv.DefaultCodeBase
	driverquarktv.SetUpstream(srv.URL, srv.URL)
	t.Cleanup(func() { driverquarktv.SetUpstream(oldBase, oldCode) })

	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/accounts/quarktv/qr", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("生成二维码该 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var qr struct {
		ID  string `json:"id"`
		PNG string `json:"png"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &qr)
	if qr.ID == "" || !strings.HasPrefix(qr.PNG, "data:image/jpeg;base64,") {
		t.Fatalf("二维码返回不对: %+v", qr)
	}

	// 第一次：还没确认
	rec, _ = doJSON(t, h, http.MethodGet, "/api/accounts/quarktv/qr/"+qr.ID, nil, cookies)
	if !strings.Contains(rec.Body.String(), `"status":"waiting"`) {
		t.Errorf("未确认该是 waiting: %s", rec.Body.String())
	}

	confirmed.Store(true)
	rec, _ = doJSON(t, h, http.MethodGet, "/api/accounts/quarktv/qr/"+qr.ID, nil, cookies)
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Fatalf("确认后该是 ok: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "夸克老王") {
		t.Errorf("账号名该用夸克昵称: %s", rec.Body.String())
	}

	accts, err := s.st.ListAccounts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(accts) != 1 || accts[0].Kind != quarkKind {
		t.Fatalf("账号没落库: %+v", accts)
	}
	full, err := s.st.GetAccount(t.Context(), accts[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(full.Credential, "RT1") {
		t.Errorf("refresh_token 没存进去: %s", full.Credential)
	}
}
