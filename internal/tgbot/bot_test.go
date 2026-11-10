package tgbot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"strm115/internal/resource"
)

// fakeTG 假装是 api.telegram.org，把每次调用记下来。
//
// 不用 mock 掉 *api：让它真的走一遍 JSON 编码，能顺带发现
// reply_markup、parse_mode 这些字段拼错的问题 —— 那些错在真实环境里
// 只会表现为「消息没发出去」。
type fakeTG struct {
	mu   sync.Mutex
	call []tgCall
	seq  int64
}

type tgCall struct {
	method  string
	payload map[string]any
	// msgID 只有 sendMessage 有：机器人把搜索结果的消息 id 存在选择状态里，
	// 测试里点按钮就得用这个 id，否则一律被当成过期。
	msgID int64
}

func (f *fakeTG) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]any
		_ = json.Unmarshal(body, &p)
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		f.mu.Lock()
		f.seq++
		id := f.seq
		c := tgCall{method: method, payload: p}
		if method == "sendMessage" {
			c.msgID = id
		}
		f.call = append(f.call, c)
		f.mu.Unlock()

		// sendMessage 要返回 message_id，edit 之后要用
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok": true,
			"result": map[string]any{
				"message_id": id,
				"chat":       map[string]any{"id": p["chat_id"]},
				"text":       p["text"],
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeTG) api(srv *httptest.Server) *api {
	return &api{token: "t", base: srv.URL + "/botT", hc: srv.Client()}
}

// last 返回某个方法的最后一次调用。
func (f *fakeTG) last(method string) (tgCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.call) - 1; i >= 0; i-- {
		if f.call[i].method == method {
			return f.call[i], true
		}
	}
	return tgCall{}, false
}

// lastSentID 返回最后一条 sendMessage 的 message_id（点击按钮时要用它）。
func (f *fakeTG) lastSentID(t *testing.T) int64 {
	t.Helper()
	c, ok := f.last("sendMessage")
	if !ok {
		t.Fatal("没有发出任何消息")
	}
	return c.msgID
}

// keyboardOf 把某次调用里的 reply_markup 解回 InlineKeyboard，方便断言按钮文字。
func keyboardOf(t *testing.T, c tgCall) InlineKeyboard {
	t.Helper()
	markup, _ := c.payload["reply_markup"].(map[string]any)
	raw, _ := json.Marshal(markup["inline_keyboard"])
	var kb InlineKeyboard
	if err := json.Unmarshal(raw, &kb); err != nil {
		t.Fatalf("键盘解析失败: %v", err)
	}
	return kb
}

func (f *fakeTG) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.call {
		if c.method == method {
			n++
		}
	}
	return n
}

// anyText 把所有发出过的消息拼起来，用来断言「某句话说过」。
func anyText(f *fakeTG) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var b strings.Builder
	for _, c := range f.call {
		if c.method == "sendMessage" || c.method == "editMessageText" {
			if s, ok := c.payload["text"].(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

func lastText(t *testing.T, f *fakeTG) string {
	t.Helper()
	c, ok := f.last("sendMessage")
	if !ok {
		t.Fatal("没有发出任何消息")
	}
	s, _ := c.payload["text"].(string)
	return s
}

// newTestBot 造一个「已绑定 chat=1」的 bot，依赖都是记录用的闭包。
type recorder struct {
	transfers []string // "code|pwd"
	offlines  []string
	searches  []string
	searchENs []string // 每次资源搜索传进来的 en（TMDB original_title）
}

func (r *recorder) deps(f *fakeTG, items []resource.Item) Deps {
	return Deps{
		Token:      func() string { return "t" },
		Allow:      func(context.Context) []int64 { return []int64{1} },
		Bind:       func(context.Context, int64) error { return nil },
		Offset:     func(context.Context) int64 { return 0 },
		SaveOffset: func(context.Context, int64) {},
		Search: func(_ context.Context, q, en string, _ int) (*resource.Result, error) {
			r.searches = append(r.searches, q)
			r.searchENs = append(r.searchENs, en)
			if items == nil {
				return &resource.Result{Query: q}, nil
			}
			return &resource.Result{Query: q, Items: items}, nil
		},
		Transfer: func(_ context.Context, code, pwd string) (*Ingest, error) {
			r.transfers = append(r.transfers, code+"|"+pwd)
			if strings.Contains(code, "bad") {
				return nil, errors.New("115 分享已失效或已取消")
			}
			return &Ingest{Title: "沙丘2", Files: 12, Dir: "电影", Account: "主号"}, nil
		},
		Offline: func(_ context.Context, uri, _ string) (*Ingest, error) {
			r.offlines = append(r.offlines, uri)
			return &Ingest{Title: "沙丘2.2024.2160p", Pending: true}, nil
		},
		Log: nil,
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	}
}

func chatMsg(id int64, text string) *Message {
	return &Message{MessageID: 10, Chat: Chat{ID: id, Type: "private"}, Text: text}
}

// 115 分享链接直接转存，并把「入库信息」回给用户。
func TestShareLinkTransfers(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, nil))

	b.onMessage(context.Background(), f.api(srv),
		chatMsg(1, "沙丘2 https://115.com/s/swabcdef?password=1234 提取码 1234"))

	if len(r.transfers) != 1 {
		t.Fatalf("应该转存 1 次，实际 %v", r.transfers)
	}
	if !strings.HasPrefix(r.transfers[0], "swabcdef|") {
		t.Fatalf("分享码/提取码解析不对: %s", r.transfers[0])
	}
	text := lastText(t, f)
	for _, want := range []string{"已入库", "沙丘2", "12 个文件", "电影", "主号"} {
		if !strings.Contains(text, want) {
			t.Errorf("回复里应该有 %q，实际：\n%s", want, text)
		}
	}
}

// 分享失效时，把 115 的原话回给用户，而不是「失败了」。
func TestShareLinkFailureIsReported(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, nil))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "https://115.com/s/swbadbad"))

	text := lastText(t, f)
	if !strings.Contains(text, "入库失败") || !strings.Contains(text, "分享已失效") {
		t.Fatalf("失败原因没带回去：\n%s", text)
	}
}

// 磁力走离线，不转存。
func TestMagnetGoesOffline(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, nil))

	magnet := "magnet:?xt=urn:btih:abcdef0123456789abcdef0123456789abcdef01&dn=Dune"
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2\n"+magnet))

	if len(r.offlines) != 1 || r.offlines[0] != magnet {
		t.Fatalf("磁力应该整条交给离线，实际 %v", r.offlines)
	}
	if len(r.transfers) != 0 {
		t.Fatal("磁力不该走进转存")
	}
	if text := lastText(t, f); !strings.Contains(text, "已提交离线") {
		t.Fatalf("应该回「已提交离线」，实际：\n%s", text)
	}
}

// 片名走搜索：结果铺成清单并挂上带片名的键盘，点按钮或回序号都能入库。
func TestNameSearchesThenLists(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	items := []resource.Item{
		{Title: "沙丘2.2024.2160p.WEB-DL", Source: "QukanMovie", Size: 12 << 30, ShareCode: "swaaa"},
		{Title: "Dune.Part.Two.2024.1080p", Source: "Nyaa", Size: 3 << 30,
			Magnet: "magnet:?xt=urn:btih:1111111111111111111111111111111111111111"},
	}
	b := New(r.deps(f, items))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))

	if len(r.searches) != 1 || r.searches[0] != "沙丘2" {
		t.Fatalf("应该按片名搜一次，实际 %v", r.searches)
	}
	sent, _ := f.last("sendMessage")
	kb := keyboardOf(t, sent)
	if len(kb) < 3 { // 2 条结果 + 取消
		t.Fatalf("搜索结果应该挂键盘，实际 %d 行", len(kb))
	}
	if !strings.Contains(kb[1][0].Text, "Dune.Part.Two") {
		t.Errorf("按钮上应该是片名，实际 %q", kb[1][0].Text)
	}
	text, _ := sent.payload["text"].(string)
	for _, want := range []string{"<b>1.</b> 沙丘2", "🧲 12.0 GB", "<b>2.</b> Dune.Part.Two", "点下面的按钮"} {
		if !strings.Contains(text, want) {
			t.Fatalf("清单里缺少 %q：\n%s", want, text)
		}
	}

	// 回复 2 → 选第二条（磁力）走离线，结果单独发一条新消息
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "2"))
	if len(r.offlines) != 1 {
		t.Fatalf("回复 2 应该走离线，实际 %v", r.offlines)
	}
	if !strings.Contains(r.offlines[0], "btih:1111") {
		t.Fatalf("应该提交第 2 条的磁力，实际 %v", r.offlines[0])
	}

	// 同一条清单不能再被选一次
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "2"))
	if len(r.offlines) != 1 {
		t.Fatalf("同一条清单不该入库两次，实际 %v", r.offlines)
	}
}

// 清单铺开：10 条结果全部列出，回复第 9 条能对上 btih:8。
// 搜索结果要挂键盘：按钮上是片名（不是序号），并带翻页。
// 回复序号这条路同时保留，编号和按钮前缀用同一套。
func TestSearchSendsNameKeyboardAndNumberReply(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	var items []resource.Item
	for i := 0; i < 10; i++ {
		it := torrentItem(i)
		it.Title = fmt.Sprintf("Dune Prophecy S01E%02d", i+1)
		items = append(items, it)
	}
	b := New(r.deps(f, items))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘：预言"))

	sent, _ := f.last("sendMessage")
	text, _ := sent.payload["text"].(string)
	// 一屏只铺 8 条，第 9/10 条翻页才出现（消息长度和键盘一一对应）
	if !strings.Contains(text, "<b>8.</b> Dune Prophecy S01E08") {
		t.Fatalf("第 8 条应该在第一屏：\n%s", text)
	}
	if strings.Contains(text, "<b>9.</b>") {
		t.Fatalf("第 9 条不该挤进第一屏：\n%s", text)
	}

	kb := keyboardOf(t, sent)
	if len(kb) != pageSize+2 { // 8 条结果 + 翻页行 + 取消
		t.Fatalf("键盘行数不对：%d 行", len(kb))
	}
	if !strings.Contains(kb[0][0].Text, "Dune Prophecy S01E01") {
		t.Errorf("按钮上应该是片名而不是数字，实际 %q", kb[0][0].Text)
	}
	if kb[0][0].CallbackData != "p:0" {
		t.Errorf("第一个按钮的回调应该是 p:0，实际 %q", kb[0][0].CallbackData)
	}

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "3"))
	if len(r.offlines) != 1 || !strings.Contains(r.offlines[0], "btih:2") {
		t.Fatalf("回复 3 应该提交第 3 条（btih:2），实际 %v", r.offlines)
	}
}

// 点按钮选片：回调里带的是全局序号，直接对到那一条。
func TestPickByButton(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	// 两条都是磁力：balanceOrder 只在一半是 TG 分享时才交错，
	// 这样下标 1 一定是第二条，断言不会随排序变化而漂移。
	items := []resource.Item{torrentItem(1), torrentItem(2)}
	b := New(r.deps(f, items))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	msgID := f.lastSentID(t)
	b.onCallback(context.Background(), f.api(srv), &CallbackQuery{
		ID:      "cb1",
		Data:    "p:1",
		Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}},
	})

	if len(r.offlines) != 1 || !strings.Contains(r.offlines[0], "btih:2") {
		t.Fatalf("点第 2 个按钮应该提交第 2 条，实际 %v", r.offlines)
	}
	// 必须 answer，否则手机上按钮一直转圈
	if f.count("answerCallbackQuery") == 0 {
		t.Fatal("没调 answerCallbackQuery")
	}
	// 清单被入库回执替换掉，按钮随之消失：同一条不可能点两次
	edited, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("应该原地改写那条清单")
	}
	kb, _ := edited.payload["reply_markup"].(map[string]any)
	if arr, _ := kb["inline_keyboard"].([]any); len(arr) != 0 {
		t.Errorf("改写后不该还留着按钮：%v", kb)
	}
}

// 翻页：只改这一屏，清单本身留着，点回去还能用。
func TestSearchPagination(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	var items []resource.Item
	for i := 0; i < 10; i++ {
		items = append(items, torrentItem(i))
	}
	b := New(r.deps(f, items))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	msgID := f.lastSentID(t)
	q := &CallbackQuery{ID: "cb", Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}}}

	q.Data = "pg:1"
	b.onCallback(context.Background(), f.api(srv), q)

	edited, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("翻页应该改写同一条消息")
	}
	text, _ := edited.payload["text"].(string)
	if !strings.Contains(text, "<b>10.</b>") || strings.Contains(text, "<b>1.</b>") {
		t.Fatalf("第二屏应该从第 9 条开始：\n%s", text)
	}
	kb := keyboardOf(t, edited)
	if !strings.Contains(kb[len(kb)-2][0].Text, "上一页") {
		t.Errorf("第二屏应该有「上一页」：%v", kb[len(kb)-2])
	}
	if len(r.offlines) != 0 {
		t.Fatalf("翻页不该触发入库，实际 %v", r.offlines)
	}
}

// 过期清单上的按钮：回一句「过期了」，绝不入库。
func TestExpiredButtonPick(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	now := time.Now()
	d := r.deps(f, []resource.Item{torrentItem(0)})
	d.Now = func() time.Time { return now }
	b := New(d)

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	msgID := f.lastSentID(t)
	now = now.Add(pickTTL + time.Minute)

	b.onCallback(context.Background(), f.api(srv), &CallbackQuery{
		ID: "cb", Data: "p:0", Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}},
	})

	if len(r.offlines) != 0 {
		t.Fatalf("过期清单不能入库，实际 %v", r.offlines)
	}
	ans, ok := f.last("answerCallbackQuery")
	if !ok || !strings.Contains(fmt.Sprint(ans.payload["text"]), "过期") {
		t.Fatalf("应该告诉用户清单过期了：%v", ans.payload)
	}
}

// 序号只对最近一次搜索有效：中间又搜了一次，旧的清单就不该再被选中。
func TestNumberReplyUsesLatestSearch(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	d := r.deps(f, nil)
	d.Search = func(_ context.Context, q, en string, _ int) (*resource.Result, error) {
		it := torrentItem(1)
		it.Title = q
		if q == "沙丘3" {
			it = torrentItem(2)
			it.Title = q
		}
		return &resource.Result{Query: q, Items: []resource.Item{it}}, nil
	}
	b := New(d)

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘3"))
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "1"))

	if len(r.offlines) != 1 || !strings.Contains(r.offlines[0], "btih:2") {
		t.Fatalf("应该选最近一次搜索（btih:2），实际 %v", r.offlines)
	}
}

// 点 115 分享那一行 → 转存。
func TestPickShareTransfers(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	items := []resource.Item{{Title: "沙丘2", Source: "QukanMovie", ShareCode: "swbbb", SharePwd: "8888"}}
	b := New(r.deps(f, items))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "1"))

	if len(r.transfers) != 1 || r.transfers[0] != "swbbb|8888" {
		t.Fatalf("应该带着提取码转存，实际 %v", r.transfers)
	}
}

// 清单过期（超过 30 分钟）之后回序号：不能再入库，得提示重新搜。
func TestExpiredPick(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	now := time.Now()
	d := r.deps(f, []resource.Item{torrentItem(0)})
	d.Now = func() time.Time { return now }
	b := New(d)

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	now = now.Add(pickTTL + time.Minute)
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "1"))

	// 过期的清单不该入库；这次「1」会被当成片名再搜一次，
	// 也就是说不会出现任何离线/转存动作。
	if len(r.offlines) != 0 || len(r.transfers) != 0 {
		t.Fatal("过期清单不能触发任何入库动作")
	}
}

// 白名单外的会话：拒绝，并把 chat id 回给它（方便用户加白名单）。
func TestUnauthorizedChatRejected(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	d := r.deps(f, nil)
	d.Allow = func(context.Context) []int64 { return []int64{999} }
	b := New(d)

	b.onMessage(context.Background(), f.api(srv), chatMsg(222, "https://115.com/s/swabc"))

	if len(r.transfers) != 0 {
		t.Fatal("未授权的会话绝对不能转存")
	}
	if text := lastText(t, f); !strings.Contains(text, "222") {
		t.Fatalf("应该把 chat id 回给用户，实际：\n%s", text)
	}
}

// 白名单为空时，第一个说话的会话成为主人。
func TestFirstChatBinds(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	var bound int64
	d := r.deps(f, nil)
	d.Allow = func(context.Context) []int64 { return nil }
	d.Bind = func(_ context.Context, id int64) error { bound = id; return nil }
	b := New(d)

	b.onMessage(context.Background(), f.api(srv), chatMsg(4242, "沙丘2"))

	if bound != 4242 {
		t.Fatalf("应该绑定第一个会话，实际绑定了 %d", bound)
	}
	if text := anyText(f); !strings.Contains(text, "已绑定") {
		t.Fatalf("应该告诉用户已经绑定，实际：\n%s", text)
	}
	// 绑定完之后同一条消息继续往下处理：用户第一句就是片名时不用再发一遍
	if len(r.searches) != 1 {
		t.Fatalf("绑定后应该继续处理这条消息（搜一次），实际 %v", r.searches)
	}
}

// 只有链接、认不出来时给出明确指引，不要去搜一串 URL。
func TestUnknownLinkDoesNotSearch(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, nil))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "https://example.com/movie/123"))

	if len(r.searches) != 0 {
		t.Fatalf("URL 不该当片名去搜，实际 %v", r.searches)
	}
	if text := lastText(t, f); !strings.Contains(text, "认不出来") {
		t.Fatalf("应该提示链接认不出来，实际：\n%s", text)
	}
}

// 命令：/search 带参数直接搜，/id 回 chat id。
func TestCommands(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, nil))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "/search@my_bot 沙丘2"))
	if len(r.searches) != 1 || r.searches[0] != "沙丘2" {
		t.Fatalf("/search@bot 参数解析不对，实际 %v", r.searches)
	}

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "/id"))
	if text := lastText(t, f); !strings.Contains(text, "Chat ID") || !strings.Contains(text, "1") {
		t.Fatalf("/id 应该回 chat id，实际：\n%s", text)
	}
}

// 片名里的 & < > 必须转义，否则 Telegram 会 400。
func TestRenderEscapesHTML(t *testing.T) {
	res := &resource.Result{Items: []resource.Item{{Title: "Tom & Jerry <2021>"}}}
	out := renderResults((&pick{query: "Tom & Jerry", res: res, items: res.Items}).view())
	if strings.Contains(out, "& Jerry <2021>") {
		t.Fatalf("HTML 特殊字符没转义：%s", out)
	}
	if !strings.Contains(out, "&amp;") || !strings.Contains(out, "&lt;2021&gt;") {
		t.Fatalf("转义结果不对：%s", out)
	}
}

func TestFindOfflineURI(t *testing.T) {
	cases := map[string]string{
		"magnet:?xt=urn:btih:abc&dn=x":        "magnet:?xt=urn:btih:abc&dn=x",
		"片名\ned2k://|file|x.mkv|123|HASH|/":   "ed2k://|file|x.mkv|123|HASH|/",
		"看这个 https://x.test/a.torrent?x=1 好片": "https://x.test/a.torrent?x=1",
		"沙丘2":                     "",
		"https://115.com/s/swabc": "",
	}
	for in, want := range cases {
		if got := findOfflineURI(in); got != want {
			t.Errorf("findOfflineURI(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestParseCommand(t *testing.T) {
	cmd, arg, ok := parseCommand("/search 沙丘 2")
	if !ok || cmd != "search" || arg != "沙丘 2" {
		t.Fatalf("解析错误：%q %q %v", cmd, arg, ok)
	}
	if _, _, ok := parseCommand("沙丘2"); ok {
		t.Fatal("普通文字不该被当成命令")
	}
	cmd, arg, _ = parseCommand("/help")
	if cmd != "help" || arg != "" {
		t.Fatalf("/help 解析错误：%q %q", cmd, arg)
	}
}

func TestHumanSize(t *testing.T) {
	cases := map[int64]string{0: "", 512: "512 B", 1 << 30: "1.0 GB", 12 * (1 << 30): "12.0 GB"}
	for in, want := range cases {
		if got := humanSize(in); got != want {
			t.Errorf("humanSize(%d) = %q，期望 %q", in, got, want)
		}
	}
}

// 一条超长列表被截断时，不能把 &amp; 切成 &am —— 那会让整条消息发不出去。
func TestRenderResultsTruncatesSafely(t *testing.T) {
	var items []resource.Item
	for i := 0; i < 60; i++ {
		items = append(items, resource.Item{
			Title:  "Tom & Jerry & Friends 沙丘2 2024 2160p WEB-DL H265 DDP5.1 国英双语 内封字幕",
			Source: "QukanMovie", Size: 12 << 30,
		})
	}
	res := &resource.Result{Items: items}
	out := renderResults((&pick{query: "Tom & Jerry", res: res, items: items}).view())

	if n := len([]rune(out)); n > 4000 {
		t.Fatalf("截断后仍然太长：%d 字符", n)
	}
	// 末尾不该出现半个实体
	if i := strings.LastIndex(out, "&"); i >= 0 {
		tail := out[i:]
		if !strings.Contains(tail, ";") && !strings.ContainsAny(tail, "\n …") {
			t.Fatalf("截断点落在实体中间：%q", tail)
		}
	}
}

// ==================== 新流程：先 TMDB 认片、再点名字入库 ====================

// tmdbHits 造一批认片候选：中文名「蝙蝠侠N」、原名「Batman N」，编号和
// 正文里的全局序号对齐，方便断言 tN 到底选了哪部。
func tmdbHits(n int) []TitleHit {
	out := make([]TitleHit, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, TitleHit{
			ID: 100 + i, Title: fmt.Sprintf("蝙蝠侠%d", i+1),
			Original: fmt.Sprintf("Batman %d", i+1), Year: "2008",
			Kind: "movie", Rating: 8.5,
		})
	}
	return out
}

// newDeepLinkBot 造一个「拿到了 bot 用户名 + 接了 TMDB 认片」的 bot。
//
// 用户名用 setBotUsername 直接注入（单测不走 poll/getMe），认片用闭包喂假
// 候选，整条链路都不连外网。
func newDeepLinkBot(f *fakeTG, r *recorder, items []resource.Item, hits []TitleHit, tmdbErr error) *Bot {
	d := r.deps(f, items)
	d.SearchTMDB = func(_ context.Context, _ string, _ int) ([]TitleHit, error) {
		return hits, tmdbErr
	}
	b := New(d)
	b.setBotUsername("strm_test_bot")
	return b
}

// 发片名先出 TMDB 候选：标题行、8 条一页、片名是 deep link、键盘只留翻页与取消。
func TestTMDBCandidatesHaveDeepLinksAndPagination(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := newDeepLinkBot(f, r, []resource.Item{torrentItem(0)}, tmdbHits(20), nil)

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))

	// 认片阶段不该碰资源搜索：先让用户点准哪部
	if len(r.searches) != 0 {
		t.Fatalf("认片阶段不该搜资源，实际 %v", r.searches)
	}
	sent, _ := f.last("sendMessage")
	text, _ := sent.payload["text"].(string)
	for _, want := range []string{
		"TMDB 找到 20 部", "第 1/3 页",
		"https://t.me/strm_test_bot?start=t1",
		"（2008）", "8.5分", "<b>8.</b>",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("候选清单缺少 %q：\n%s", want, text)
		}
	}
	if strings.Contains(text, "start=t9") {
		t.Errorf("第一屏不该出现第 9 部：\n%s", text)
	}

	kb := keyboardOf(t, sent)
	// 20 部 3 页，一屏 8 部：8 行选片按钮 + 翻页行 + 取消行。
	//
	// 以前「有用户名就只留翻页」是为了版面清爽，结果点正文深链会让 Telegram
	// 发一条 /start tN 出来，用户以为「我已经选过了为什么还要发 /start」。
	// 现在每部片都有按钮，点一下原地选定，不产生消息。
	if len(kb) != 10 {
		t.Fatalf("候选键盘应有 8 行选片 + 翻页 + 取消，实际 %d 行：%v", len(kb), kb)
	}
	if got := kb[0][0].CallbackData; got != "t:1" {
		t.Errorf("第一部应有 t:1 按钮，实际 %q", got)
	}
	next := false
	for _, btn := range kb[len(kb)-2] {
		if btn.CallbackData == "tp:1" {
			next = true
		}
	}
	if !next {
		t.Errorf("翻页行该有「下一页」tp:1，实际 %v", kb[len(kb)-2])
	}
	if last := kb[len(kb)-1][0].CallbackData; last != "tx" {
		t.Errorf("最后一行的取消回调应是 tx，实际 %q", last)
	}
}

// 翻页：第二屏从第 9 部开始，deep link 的全局序号跟着往后走。
func TestTMDBCandidatePagination(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := newDeepLinkBot(f, r, []resource.Item{torrentItem(0)}, tmdbHits(20), nil)

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))
	msgID := f.lastSentID(t)
	b.onCallback(context.Background(), f.api(srv), &CallbackQuery{
		ID: "cb", Data: "tp:1", Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}},
	})

	edited, ok := f.last("editMessageText")
	if !ok {
		t.Fatal("翻页应改写同一条候选消息")
	}
	text, _ := edited.payload["text"].(string)
	for _, want := range []string{"第 2/3 页", "https://t.me/strm_test_bot?start=t9", "<b>9.</b>"} {
		if !strings.Contains(text, want) {
			t.Errorf("第二屏缺少 %q：\n%s", want, text)
		}
	}
}

// 点 tN（deep link）后用该候选的中文名当 query、original_title 当 en 搜资源。
// 同时验证 /start t2@botname 这种带尾巴的写法也能认。
func TestStartTNUsesOriginalTitleForResourceSearch(t *testing.T) {
	for _, arg := range []string{"/start t2", "/start t2@strm_test_bot"} {
		t.Run(arg, func(t *testing.T) {
			f := &fakeTG{}
			srv := f.server(t)
			r := &recorder{}
			items := []resource.Item{{Title: "Batman.2008.1080p", Kind: "torrent", Magnet: "magnet:?xt=urn:btih:aaaa"}}
			b := newDeepLinkBot(f, r, items, tmdbHits(3), nil)

			b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))
			b.onMessage(context.Background(), f.api(srv), chatMsg(1, arg))

			if len(r.searches) != 1 || r.searches[0] != "蝙蝠侠2" {
				t.Fatalf("query 该用认片中文名「蝙蝠侠2」，实际 %v", r.searches)
			}
			if len(r.searchENs) != 1 || r.searchENs[0] != "Batman 2" {
				t.Fatalf("en 该用 original_title「Batman 2」，实际 %v", r.searchENs)
			}
			sent, _ := f.last("sendMessage")
			text, _ := sent.payload["text"].(string)
			if !strings.Contains(text, "https://t.me/strm_test_bot?start=p1") {
				t.Errorf("资源清单的种子名该是可点 deep link：\n%s", text)
			}
		})
	}
}

// 点 pN：直接走 act() 入库，不用再点底部按钮、也不用回序号。
func TestStartPNIngestsDirectly(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	items := []resource.Item{
		{Title: "Batman.A", Kind: "torrent", Magnet: "magnet:?xt=urn:btih:1111"},
		{Title: "Batman.B", Kind: "torrent", Magnet: "magnet:?xt=urn:btih:2222"},
	}
	// 不接认片：先拿到一份资源清单，再点它的种子名
	b := New(r.deps(f, items))
	b.setBotUsername("strm_test_bot")

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))
	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "/start p2"))

	if len(r.offlines) != 1 || !strings.Contains(r.offlines[0], "btih:2222") {
		t.Fatalf("点 p2 该直接入库第 2 条（btih:2222），实际 %v", r.offlines)
	}
}

// TMDB 挂了 / 没候选：必须自动降级成「直接按片名搜资源」，bot 不能哑掉。
func TestTMDBFailureFallsBackToDirectSearch(t *testing.T) {
	cases := []struct {
		name string
		hits []TitleHit
		err  error
	}{
		{"报错", nil, errors.New("TMDB 502")},
		{"空候选", nil, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeTG{}
			srv := f.server(t)
			r := &recorder{}
			items := []resource.Item{{Title: "Batman.2008", Kind: "torrent", Magnet: "magnet:?xt=urn:btih:aaaa"}}
			b := newDeepLinkBot(f, r, items, tc.hits, tc.err)

			b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))

			if len(r.searches) != 1 || r.searches[0] != "蝙蝠侠" {
				t.Fatalf("TMDB 不可用时应降级直接搜资源，实际 %v", r.searches)
			}
			if len(r.searchENs) != 1 || r.searchENs[0] != "" {
				t.Errorf("降级时 en 应为空，实际 %v", r.searchENs)
			}
			sent, _ := f.last("sendMessage")
			text, _ := sent.payload["text"].(string)
			if !strings.Contains(text, "https://t.me/strm_test_bot?start=p1") {
				t.Errorf("降级后仍应发出带 deep link 的资源清单：\n%s", text)
			}
		})
	}
}

// 拿到用户名后，资源清单正文的种子名是链接，但**按钮仍然要在**：
// 点正文链接会发 /start pN（群聊里还会跳到私聊导致清单丢失），按钮才是可靠路径。
func TestResultListKeepsPickButtonsWhenUsernameKnown(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	var items []resource.Item
	for i := 0; i < 10; i++ {
		items = append(items, torrentItem(i))
	}
	b := New(r.deps(f, items))
	b.setBotUsername("strm_test_bot")

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))

	sent, _ := f.last("sendMessage")
	kb := keyboardOf(t, sent)
	// 10 条 2 页，一屏 8 条：8 行选片 + 翻页 + 取消
	if len(kb) != 10 {
		t.Fatalf("资源清单应有 8 行选片 + 翻页 + 取消，实际 %d 行：%v", len(kb), kb)
	}
	if got := kb[0][0].CallbackData; got != "p:0" {
		t.Errorf("第一条应有 p:0 按钮，实际 %q", got)
	}
	text, _ := sent.payload["text"].(string)
	if !strings.Contains(text, "https://t.me/strm_test_bot?start=p1") {
		t.Fatalf("种子名该是 deep link：\n%s", text)
	}
	if !strings.Contains(text, "点上面的片名") {
		t.Errorf("文案该引导点正文片名，别再宣传按钮 / 回复序号：\n%s", text)
	}
}

// 没拿到用户名：片名不带链接，降级回按钮选定，并在文案里说明原因。
func TestNoUsernameFallsBackToButtons(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := newDeepLinkBot(f, r, []resource.Item{torrentItem(0)}, tmdbHits(2), nil)
	b.setBotUsername("") // getMe 没返回用户名的场景

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "蝙蝠侠"))

	sent, _ := f.last("sendMessage")
	text, _ := sent.payload["text"].(string)
	if strings.Contains(text, "https://t.me/") {
		t.Errorf("没用户名时不该有 deep link：\n%s", text)
	}
	if !strings.Contains(text, "没拿到 Bot 用户名") {
		t.Errorf("文案该写明降级原因：\n%s", text)
	}
	kb := keyboardOf(t, sent)
	// 2 部候选（1 页）：每部一个 t: 按钮 + 取消
	if len(kb) != 3 {
		t.Fatalf("降级时该给每部片一个按钮，实际 %d 行：%v", len(kb), kb)
	}
	if kb[0][0].CallbackData != "t:1" || kb[1][0].CallbackData != "t:2" {
		t.Errorf("降级按钮应是 t:1 / t:2，实际 %v", kb)
	}

	// 降级按钮点了照样能选中，且仍用 original_title 去搜资源
	b.onCallback(context.Background(), f.api(srv), &CallbackQuery{
		ID: "cb", Data: "t:2", Message: &Message{MessageID: f.lastSentID(t), Chat: Chat{ID: 1}},
	})
	if len(r.searchENs) != 1 || r.searchENs[0] != "Batman 2" {
		t.Fatalf("降级按钮选中后也该用 original_title 搜资源，实际 %v", r.searchENs)
	}
}

// 老消息上的 x（取消）按钮不能点了没反应。
func TestLegacyCancelCallbackStillWorks(t *testing.T) {
	f := &fakeTG{}
	srv := f.server(t)
	r := &recorder{}
	b := New(r.deps(f, []resource.Item{torrentItem(0)}))

	b.onMessage(context.Background(), f.api(srv), chatMsg(1, "沙丘2"))
	msgID := f.lastSentID(t)
	b.onCallback(context.Background(), f.api(srv), &CallbackQuery{
		ID: "cb", Data: "x", Message: &Message{MessageID: msgID, Chat: Chat{ID: 1}},
	})

	if f.count("answerCallbackQuery") == 0 {
		t.Fatal("x 也要 answer，否则手机上按钮一直转圈")
	}
	edited, ok := f.last("editMessageText")
	if !ok || !strings.Contains(fmt.Sprint(edited.payload["text"]), "重新搜") {
		t.Fatalf("x 应把清单改写成「重新搜」提示：%v", edited.payload)
	}
}
