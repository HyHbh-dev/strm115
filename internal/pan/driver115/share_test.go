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

	"strm115/internal/pan"
)

// fakeShare 扮演 share/snap 与 share/receive 两个端点。
type fakeShare struct {
	mu       sync.Mutex
	snaps    []url.Values
	receives []url.Values

	snapReply    func(n int, q url.Values) (int, string)
	receiveReply func(n int, form url.Values) (int, string)
}

func (f *fakeShare) server(t *testing.T) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/snap", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		n := len(f.snaps)
		f.snaps = append(f.snaps, r.URL.Query())
		f.mu.Unlock()
		code, body := f.snapReply(n, r.URL.Query())
		w.WriteHeader(code)
		io.WriteString(w, body)
	})
	mux.HandleFunc("/receive", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("表单解析失败: %v", err)
			return
		}
		f.mu.Lock()
		n := len(f.receives)
		f.receives = append(f.receives, r.PostForm)
		f.mu.Unlock()
		code, body := f.receiveReply(n, r.PostForm)
		w.WriteHeader(code)
		io.WriteString(w, body)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	oldSnap, oldRecv := shareSnapURL, shareReceiveURL
	shareSnapURL = srv.URL + "/snap"
	shareReceiveURL = srv.URL + "/receive"
	t.Cleanup(func() { shareSnapURL, shareReceiveURL = oldSnap, oldRecv })
}

const snapOK = `{"state":true,"data":{"shareinfo":{"snap_id":"snap-1","share_title":"沙丘2 2024 2160p","share_state":1,"expire_time":0},
"count":2,"list":[{"fid":"111","n":"沙丘2.2160p.mkv","fc":0},{"fid":"222","n":"字幕","fc":1}]}}`

func TestSaveShareSuccess(t *testing.T) {
	f := &fakeShare{
		snapReply:    func(int, url.Values) (int, string) { return 200, snapOK },
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)
	c := newTestClient()

	res, err := c.SaveShare(context.Background(), "swz8k9x3", "ab12", "123456")
	if err != nil {
		t.Fatalf("转存应当成功: %v", err)
	}
	if res.Files != 2 || res.Title != "沙丘2 2024 2160p" || res.Already {
		t.Errorf("结果不对: %+v", res)
	}

	snap := f.snaps[0]
	if snap.Get("share_code") != "swz8k9x3" || snap.Get("receive_code") != "ab12" {
		t.Errorf("snap 参数不对: %v", snap)
	}
	form := f.receives[0]
	if form.Get("share_code") != "swz8k9x3" || form.Get("receive_code") != "ab12" {
		t.Errorf("receive 参数不对: %v", form)
	}
	if form.Get("cid") != "123456" {
		t.Errorf("目标目录没带上: %v", form)
	}
	if form.Get("file_id") != "111,222" {
		t.Errorf("file_id 应是逗号分隔的全部顶层条目: %v", form)
	}
	if form.Get("is_check") != "0" {
		t.Errorf("is_check 应固定 0: %v", form)
	}
}

func TestSaveShareAlreadySaved(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) { return 200, snapOK },
		receiveReply: func(int, url.Values) (int, string) {
			return 200, `{"state":false,"errno":4100024,"error":"已经转存过"}`
		},
	}
	f.server(t)

	res, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "")
	if err != nil {
		t.Fatalf("已转存过不该算失败: %v", err)
	}
	if !res.Already {
		t.Error("应标成 Already")
	}
	if f.receives[0].Get("cid") != "0" {
		t.Errorf("没指定目录时应落到根目录 0: %v", f.receives[0])
	}
}

func TestSaveShareWrongPassword(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) {
			return 200, `{"state":false,"errno":4100009,"error":"提取码错误"}`
		},
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "0000", "0")
	if !errors.Is(err, pan.ErrRejected) {
		t.Fatalf("提取码错应归为 ErrRejected，实际 %v", pan.KindOf(err))
	}
	if !strings.Contains(err.Error(), "提取码") {
		t.Errorf("错误信息应提到提取码: %v", err)
	}
	if len(f.receives) != 0 {
		t.Error("snap 阶段就该拦下，不该再打 receive")
	}
}

func TestSaveShareForbidden(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) {
			return 200, `{"state":true,"data":{"shareinfo":{"share_title":"某某","forbid_reason":"涉嫌违规"}}}`
		},
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if !errors.Is(err, pan.ErrRejected) || !strings.Contains(err.Error(), "封禁") {
		t.Fatalf("被封的分享应报 ErrRejected + 封禁原因，实际 %v", err)
	}
}

func TestSaveShareExpired(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) {
			return 200, `{"state":true,"data":{"shareinfo":{"share_title":"x","expire_time":1000000000}}}`
		},
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if !errors.Is(err, pan.ErrRejected) || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期分享应报 ErrRejected + 过期，实际 %v", err)
	}
}

func TestSaveShareEmptyShare(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) {
			return 200, `{"state":true,"data":{"shareinfo":{"share_title":"空"},"count":0,"list":[]}}`
		},
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if !errors.Is(err, pan.ErrNotFound) {
		t.Fatalf("空分享应归为 ErrNotFound，实际 %v", pan.KindOf(err))
	}
}

func TestSaveShareLoginExpired(t *testing.T) {
	f := &fakeShare{
		snapReply:    func(int, url.Values) (int, string) { return 200, snapOK },
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":false,"errno":99,"error":"未登录"}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if !errors.Is(err, pan.ErrAuth) {
		t.Fatalf("未登录应归为 ErrAuth，实际 %v", pan.KindOf(err))
	}
}

func TestSaveShareNonJSON(t *testing.T) {
	f := &fakeShare{
		snapReply:    func(int, url.Values) (int, string) { return 200, "decode fail!" },
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if !errors.Is(err, pan.ErrTransient) {
		t.Fatalf("非 JSON 响应应归为 ErrTransient，实际 %v", pan.KindOf(err))
	}
	if !strings.Contains(err.Error(), "decode fail") {
		t.Errorf("错误信息应带上响应原文: %v", err)
	}
}

func TestSaveShareEmptyCode(t *testing.T) {
	f := &fakeShare{
		snapReply:    func(int, url.Values) (int, string) { return 200, snapOK },
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	_, err := newTestClient().SaveShare(context.Background(), "  ", "", "0")
	if !errors.Is(err, pan.ErrRejected) {
		t.Fatalf("空分享码应直接拒绝，实际 %v", pan.KindOf(err))
	}
	if len(f.snaps) != 0 {
		t.Error("空分享码不该发请求")
	}
}

// 新版快照的 list 只有 cid，没有 fid —— 实测就是这样，别按 fid 判空。
const snapCIDOnly = `{"state":true,"data":{"shareinfo":{"share_title":"只给 cid"},"count":1,
"list":[{"cid":"987","n":"片子.mkv","fc":0}]}}`

func TestSaveShareUsesCIDWhenFIDMissing(t *testing.T) {
	f := &fakeShare{
		snapReply:    func(int, url.Values) (int, string) { return 200, snapCIDOnly },
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	res, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if err != nil {
		t.Fatalf("转存应当成功: %v", err)
	}
	if res.Files != 1 {
		t.Errorf("文件数应为 1，实际 %d", res.Files)
	}
	if got := f.receives[0].Get("file_id"); got != "987" {
		t.Errorf("应拿 cid 当 file_id，实际 %q", got)
	}
}

// 一个 id 都拿不到时（快照没返回明细）不能硬失败：不带 file_id 就是「全部转存」。
func TestSaveShareWithoutEntryIDs(t *testing.T) {
	f := &fakeShare{
		snapReply: func(int, url.Values) (int, string) {
			return 200, `{"state":true,"data":{"shareinfo":{"share_title":"没明细"},"count":3,"list":[]}}`
		},
		receiveReply: func(int, url.Values) (int, string) { return 200, `{"state":true}` },
	}
	f.server(t)

	res, err := newTestClient().SaveShare(context.Background(), "swz8k9x3", "", "0")
	if err != nil {
		t.Fatalf("不该失败: %v", err)
	}
	if res.Files != 3 {
		t.Errorf("文件数应回落到 count=3，实际 %d", res.Files)
	}
	if _, has := f.receives[0]["file_id"]; has {
		t.Errorf("没有明细时不该带 file_id: %v", f.receives[0])
	}
}
