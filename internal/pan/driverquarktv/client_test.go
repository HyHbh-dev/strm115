package driverquarktv

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"strm115/internal/pan"
)

// fakeQuark 是最小可用的假 TV 网关。
type fakeQuark struct {
	listCalls   atomic.Int32
	downCalls   atomic.Int32
	tokenCalls  atomic.Int32
	userCalls   atomic.Int32
	lastQuery   atomic.Value // 最近一次 /file 的 query 串，验参数用
	lastHeaders atomic.Value // 最近一次请求的关键头
	// expireOnce 为 true 时，第一个业务请求回 errno=10001，用来验「过期自动刷新」
	expireOnce atomic.Bool
	srv        *httptest.Server
}

func newFakeQuark(t *testing.T) *fakeQuark {
	t.Helper()
	f := &fakeQuark{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		f.lastQuery.Store(r.URL.Path + "?" + q.Encode())
		f.lastHeaders.Store(map[string]string{
			"tm": r.Header.Get("x-pan-tm"),
			"tk": r.Header.Get("x-pan-token"),
			"id": r.Header.Get("x-pan-client-id"),
			"ua": r.Header.Get("User-Agent"),
		})
		switch r.URL.Path {
		case "/token":
			f.tokenCalls.Add(1)
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["code"] == "BAD" {
				_, _ = w.Write([]byte(`{"code":400,"message":"授权码已过期"}`))
				return
			}
			_, _ = w.Write([]byte(`{"code":200,"data":{"access_token":"AT2","refresh_token":"RT2","expires_in":7200}}`))
		case "/user":
			f.userCalls.Add(1)
			if q.Get("method") != "user_info" {
				t.Errorf("/user 只该带 method=user_info，实际 %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"nickname":"夸克老王"}}`))
		case "/file":
			switch q.Get("method") {
			case "list":
				f.listCalls.Add(1)
				if f.expireOnce.CompareAndSwap(true, false) {
					_, _ = w.Write([]byte(`{"status":-1,"errno":10001,"error_info":"token expired"}`))
					return
				}
				page := q.Get("page_index")
				if page == "0" {
					_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"total_count":3,"files":[
						{"fid":"f1","parent_fid":"0","filename":"电影A","size":100,"isdir":0,"updated_at":1700000000000},
						{"fid":"d1","parent_fid":"0","filename":"剧集B","size":0,"isdir":1,"updated_at":1700000000000}]}}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"total_count":3,"files":[
					{"fid":"f3","parent_fid":"0","filename":"电影C","size":300,"isdir":0,"updated_at":1700000000000}]}}`))
			case "download":
				f.downCalls.Add(1)
				if q.Get("fid") == "gone" {
					_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"fid":"gone"}}`))
					return
				}
				_, _ = w.Write([]byte(`{"status":0,"errno":0,"data":{"fid":"f1","file_name":"电影A.mkv","size":100,"download_url":"https://cdn.example/f1.mkv?sign=abc"}}`))
			default:
				t.Errorf("意外的 /file method=%s", q.Get("method"))
				w.WriteHeader(http.StatusNotFound)
			}
		case "/oauth/authorize":
			if q.Get("auth_type") != "code" || q.Get("client_id") != clientID {
				t.Errorf("authorize 参数不对: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{"status":0,"qr_data":"QUJD","query_token":"QT1"}`))
		case "/oauth/code":
			if q.Get("query_token") != "QT1" {
				t.Errorf("code 没带 query_token: %s", r.URL.RawQuery)
			}
			if f.tokenCalls.Load() == 0 {
				// 第一次：用户还没确认
				_, _ = w.Write([]byte(`{"status":0}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":0,"code":"C1"}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// pointDefaultsAt 把包级默认根指到假网关，Open 走的也是这条。
func pointDefaultsAt(t *testing.T, base string) {
	t.Helper()
	oldBase, oldCode := DefaultBase, DefaultCodeBase
	DefaultBase, DefaultCodeBase = base, base
	t.Cleanup(func() { DefaultBase, DefaultCodeBase = oldBase, oldCode })
}

func newTestClient(f *fakeQuark, rt string) *Client {
	c := New(Credential{RefreshToken: rt, DeviceID: "dev-1"})
	c.base = f.srv.URL
	c.codeBase = f.srv.URL
	return c
}

func TestOpenRefreshesTokenAndPing(t *testing.T) {
	f := newFakeQuark(t)
	pointDefaultsAt(t, f.srv.URL)
	drv, err := Open(context.Background(), pan.Account{Kind: Kind, Credential: `{"refresh_token":"RT","device_id":"dev-1"}`})
	if err != nil {
		t.Fatal(err)
	}
	c := drv.(*Client)
	if f.tokenCalls.Load() != 1 {
		t.Errorf("Open 应该换一次 token，实际 %d 次", f.tokenCalls.Load())
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping 失败: %v", err)
	}
	if got := c.Nickname(context.Background()); got != "夸克老王" {
		t.Errorf("昵称 = %q", got)
	}
	if c.Credential().RefreshToken != "RT2" {
		t.Errorf("刷新后的 refresh_token 没留住: %q", c.Credential().RefreshToken)
	}
}

func TestOpenRejectsBadCredential(t *testing.T) {
	if _, err := Open(context.Background(), pan.Account{Credential: `{"device_id":"d"}`}); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("缺 refresh_token 应该是 ErrAuth，实际 %v", err)
	}
	if _, err := Open(context.Background(), pan.Account{Credential: `not json`}); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("凭证格式错误应该是 ErrAuth，实际 %v", err)
	}
}

func TestListPageMapsEntriesAndPaging(t *testing.T) {
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	_ = c.ensureToken(context.Background())

	entries, total, err := c.ListPage(context.Background(), "0", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if total != 3 {
		t.Errorf("total = %d，想 3", total)
	}
	if len(entries) != 2 {
		t.Fatalf("第一页应有 2 条，实际 %d", len(entries))
	}
	if entries[0].ID != "f1" || entries[0].PickCode != "f1" || entries[0].Name != "电影A" {
		t.Errorf("文件字段没映射对: %+v", entries[0])
	}
	if entries[0].ModTime.IsZero() {
		t.Error("updated_at 没转成 ModTime")
	}
	if !entries[1].IsDir {
		t.Errorf("isdir=1 应该映射成目录: %+v", entries[1])
	}

	// offset 2 / limit 2 → 第 1 页（page_index=1）
	q, _ := f.lastQuery.Load().(string)
	if !strings.Contains(q, "page_index=0") {
		t.Errorf("第一页应该 page_index=0，实际 %s", q)
	}
	page2, _, err := c.ListPage(context.Background(), "0", 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 || page2[0].ID != "f3" {
		t.Errorf("第二页不对: %+v", page2)
	}
	q, _ = f.lastQuery.Load().(string)
	if !strings.Contains(q, "page_index=1") {
		t.Errorf("offset=2/limit=2 应该 page_index=1，实际 %s", q)
	}
	// 请求必须带签名三件套与 TV 客户端标识
	h, _ := f.lastHeaders.Load().(map[string]string)
	if len(h["tk"]) != 64 || len(h["tm"]) == 0 || h["id"] != clientID {
		t.Errorf("签名头不完整: %+v", h)
	}
	// 期望的 token 是 sha256("GET&/file&ts&signKey")
	want := sha256.Sum256([]byte("GET&/file&" + h["tm"] + "&" + signKey))
	if h["tk"] != hex.EncodeToString(want[:]) {
		t.Error("x-pan-token 签名不对")
	}
	if !strings.Contains(h["ua"], "Mobile Safari") {
		t.Errorf("UA 必须是 TV 客户端那一个，实际 %s", h["ua"])
	}
	if strings.Contains(q, "parent_fid=0") == false {
		t.Errorf("根目录要传 parent_fid=0，实际 %s", q)
	}
}

func TestListPageSkipsPartialOffset(t *testing.T) {
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	_ = c.ensureToken(context.Background())

	// offset=1, limit=2 → 仍是 page_index=0，但要跳过第 1 条
	entries, _, err := c.ListPage(context.Background(), "0", 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].ID != "d1" {
		t.Errorf("非整除 offset 应该跳过前 skip 条: %+v", entries)
	}
}

func TestListPageClampsPageSizeToQuarkLimit(t *testing.T) {
	// 上层统一用 200 列目录；夸克 TV 只收 1–100，否则直接回
	// "/file: page_size 范围: 1-100"，界面上会变成一句莫名奇妙的凭据报错。
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	_ = c.ensureToken(context.Background())
	if _, _, err := c.ListPage(context.Background(), "0", 0, 200); err != nil {
		t.Fatal(err)
	}
	q, _ := f.lastQuery.Load().(string)
	if !strings.Contains(q, "page_size=100") {
		t.Errorf("page_size 应收敛到 100，实际请求 %s", q)
	}
	if c.MaxPageSize() != quarkMaxPageSize || quarkMaxPageSize > 100 {
		t.Errorf("MaxPageSize=%d，夸克硬上限是 100", c.MaxPageSize())
	}
}

func TestDownloadURL(t *testing.T) {
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	_ = c.ensureToken(context.Background())

	u, err := c.DownloadURL(context.Background(), "f1", "Emby/1.0")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(u, "cdn.example") {
		t.Errorf("直链不对: %q", u)
	}
	q, _ := f.lastQuery.Load().(string)
	for _, want := range []string{"method=download", "fid=f1", "group_by=source"} {
		if !strings.Contains(q, want) {
			t.Errorf("取直链少了参数 %s：%s", want, q)
		}
	}
	if _, err := c.DownloadURL(context.Background(), "gone", ""); !errors.Is(err, pan.ErrNotFound) {
		t.Errorf("没给直链应该是 ErrNotFound，实际 %v", err)
	}
	if _, err := c.DownloadURL(context.Background(), "", ""); !errors.Is(err, pan.ErrNotFound) {
		t.Errorf("空 fid 应该是 ErrNotFound，实际 %v", err)
	}
}

// TV 接口只剩「按 id 查条目」是不支持的；移动/改名/删除/建目录已经改走
// 网页接口（见 web_test.go）。没有网页 Cookie 的账号调用它们时，要给一个
// 说得清的错误，而不是 ErrUnsupported。
func TestUnsupportedOps(t *testing.T) {
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	ctx := context.Background()

	if _, err := c.Stat(ctx, "f1"); !errors.Is(err, pan.ErrUnsupported) {
		t.Errorf("按 id 查条目应该 ErrUnsupported，实际 %v", err)
	}
	if _, err := c.Mkdir(ctx, "0", "x"); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("没有网页 Cookie 时建目录应该 ErrAuth，实际 %v", err)
	}
	if err := c.Move(ctx, "0", "f1"); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("没有网页 Cookie 时移动应该 ErrAuth，实际 %v", err)
	}
	if err := c.Rename(ctx, "f1", "x"); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("没有网页 Cookie 时改名应该 ErrAuth，实际 %v", err)
	}
	if err := c.Remove(ctx, "f1"); !errors.Is(err, pan.ErrAuth) {
		t.Errorf("没有网页 Cookie 时删除应该 ErrAuth，实际 %v", err)
	}
}

func TestTokenExpiredRefreshAndRetry(t *testing.T) {
	f := newFakeQuark(t)
	c := newTestClient(f, "RT")
	_ = c.ensureToken(context.Background()) // 1 次
	f.expireOnce.Store(true)

	entries, _, err := c.ListPage(context.Background(), "0", 0, 10)
	if err != nil {
		t.Fatalf("过期后应该自动刷新重放，实际 %v", err)
	}
	if len(entries) != 2 {
		t.Errorf("重放没拿到数据: %+v", entries)
	}
	if n := f.listCalls.Load(); n != 2 {
		t.Errorf("/file 应该被打两次（过期 + 重放），实际 %d", n)
	}
	if n := f.tokenCalls.Load(); n != 2 {
		t.Errorf("应该刷新一次 token（共 2 次），实际 %d", n)
	}
}

func TestQRLoginFlow(t *testing.T) {
	f := newFakeQuark(t)
	q := NewQRLogin(f.srv.URL, f.srv.URL)

	qr, err := q.Authorize(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if qr != "QUJD" || q.QueryToken != "QT1" {
		t.Errorf("二维码/query_token 没拿到: qr=%q token=%q", qr, q.QueryToken)
	}

	// 还没确认：不报错、也不算完成
	cr, done, err := q.Poll(context.Background())
	if err != nil || done {
		t.Fatalf("未确认时应该 (false,nil)，实际 done=%v err=%v", done, err)
	}
	_ = cr

	// 先在假网关里"记一笔已换过 token"，让它下一次返回 code
	f.tokenCalls.Add(1)
	cr, done, err = q.Poll(context.Background())
	if err != nil || !done {
		t.Fatalf("确认后应该返回凭证，实际 done=%v err=%v", done, err)
	}
	if cr.RefreshToken != "RT2" || cr.DeviceID == "" || cr.QueryToken != "QT1" {
		t.Errorf("凭证不完整: %+v", cr)
	}
	// 再轮询一次不该重复换 token
	f.tokenCalls.Store(0)
	if _, done, _ := q.Poll(context.Background()); done {
		t.Error("同一个会话不该二次建账号")
	}
	if f.tokenCalls.Load() != 0 {
		t.Error("已完成的会话不该再打 token 网关")
	}
}

func TestQRLoginBadCode(t *testing.T) {
	f := newFakeQuark(t)
	q := NewQRLogin(f.srv.URL, f.srv.URL)
	if _, err := q.Authorize(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.tokenCalls.Add(1)
	// 假网关返回 code=C1，但把 token 网关改成失败：用另一个 code 触发
	q.QueryToken = "QT1"
	// 直接验 exchange 的错误分类：过期/失效要归到 ErrAuth
	_, err := q.c.exchange(context.Background(), map[string]string{"code": "BAD"})
	if !errors.Is(err, pan.ErrAuth) {
		t.Errorf("「授权码已过期」应该归到 ErrAuth，实际 %v", err)
	}
}

func TestClassifyAPI(t *testing.T) {
	cases := []struct {
		status, errno int
		info          string
		want          error
	}{
		{401, 0, "", pan.ErrAuth},
		{0, 0, "请先登录", pan.ErrAuth},
		{0, 0, "操作太频繁", pan.ErrRisk},
		{404, 0, "", pan.ErrNotFound},
		{0, 0, "文件不存在", pan.ErrNotFound},
		{0, 0, "空间不足", pan.ErrRejected},
	}
	for _, c := range cases {
		err := classifyAPI("测", c.status, c.errno, c.info)
		if !errors.Is(err, c.want) {
			t.Errorf("status=%d info=%q → %v，想 %v", c.status, c.info, err, c.want)
		}
	}
}

func TestUnixMilli(t *testing.T) {
	if !unixMilli(0).IsZero() {
		t.Error("0 应该是零值时间")
	}
	if got := unixMilli(1700000000000); got.Year() != 2023 {
		t.Errorf("毫秒时间戳解析错: %v", got)
	}
	if got := unixMilli(1700000000); got.Year() != 2023 {
		t.Errorf("秒级时间戳兜底错: %v", got)
	}
}
