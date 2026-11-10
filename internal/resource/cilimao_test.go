package resource

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// b64 造 page 夹具用：站点就是把整段 HTML base64 后塞进 atob 的。
func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// 磁力猫的结果页是「base64 + URL 编码」塞在 window.atob 里的，夹具直接用
// 真响应（搜「空枪」那一页），连解码这一层一起验。
func TestParseClm(t *testing.T) {
	items := parseClm(fixture(t, "clm_search.html"), 5)
	if len(items) != 5 {
		t.Fatalf("解析出 %d 条，想要 5 条", len(items))
	}
	it := items[0]
	if it.Title != "[电玩份子] P057 时空枪手4.rar" {
		t.Errorf("标题 = %q", it.Title)
	}
	// slug 是 infohash 的 base32，磁力应当由它直接拼出来，不用翻详情页
	if want := "magnet:?xt=urn:btih:8C5F5F5FE5F48A99AEE3CE4502C3853CA2056821"; !strings.HasPrefix(it.Magnet, want) {
		t.Errorf("磁力 = %q，应以 %q 开头", it.Magnet, want)
	}
	if !strings.Contains(it.Magnet, "&dn=") || !strings.Contains(it.Magnet, "&tr=") {
		t.Errorf("磁力缺 dn / tracker，115 容易拉不动：%q", it.Magnet)
	}
	if it.Size == 0 || it.Date != "2025-04-06" {
		t.Errorf("体积/日期没解析出来：size=%d date=%q", it.Size, it.Date)
	}
	if it.SourceKey != "clm" || it.Kind != "torrent" ||
		!strings.HasPrefix(it.URL, clmBase+"/information/") {
		t.Errorf("来源字段不对：%+v", it)
	}
}

// 中文标题里的加号不能变成空格。JS 那边是 decodeURIComponent，
// 我们如果在 Go 里用 QueryUnescape 就会把「国粤多音轨+中文字幕」解坏。
func TestParseClmKeepsPlus(t *testing.T) {
	items := parseClm(fixture(t, "clm_search.html"), 10)
	found := false
	for _, it := range items {
		if strings.Contains(it.Title, "国粤多音轨+中文字幕") {
			found = true
		}
		if strings.Contains(it.Title, "<em>") || strings.Contains(it.Title, "%E") {
			t.Errorf("标题里还留着标签/转义：%q", it.Title)
		}
	}
	if !found {
		t.Error("没找到「国粤多音轨+中文字幕」，加号很可能被解成了空格")
	}
}

// 搜不到东西时站点返回的是「很抱歉…」页，里面也有 <li>（提示文案），
// 但一条都不该被当成结果。
func TestParseClmNoResult(t *testing.T) {
	if items := parseClm(fixture(t, "clm_no_result.html"), 20); len(items) != 0 {
		t.Fatalf("空结果页解析出 %d 条：%+v", len(items), items[0])
	}
}

func TestClmWord(t *testing.T) {
	// 站点自己的链接就是这个形式：/search?word=56m65p6q 对应「空枪」。
	// 中文一个字 3 字节，base64 天然不带 = ；ASCII 要按 RawStdEncoding 去掉填充。
	for in, want := range map[string]string{
		"空枪":   "56m65p6q",
		"流浪地球": "5rWB5rWq5Zyw55CD",
		"ab":   "YWI",
	} {
		if got := clmWord(in); got != want {
			t.Errorf("clmWord(%q) = %q，想要 %q", in, got, want)
		}
	}
}

func TestBase32InfoHash(t *testing.T) {
	// 拿真详情页对过的样本：/information/savdhddo… 的磁力就是 902A338C…
	if got := base32InfoHash("savdhddo655bizlchtosfxpjii6kum5s"); got != "902A338C6EF77A1465623CDD22DDE9423CAA33B2" {
		t.Errorf("base32InfoHash = %q", got)
	}
	// 站点哪天换成随机 slug，这里必须认输（返回空串），不能拼出一条假磁力
	for _, bad := range []string{"", "abc", "1111111111111111111111111111111x"} {
		if got := base32InfoHash(bad); got != "" {
			t.Errorf("base32InfoHash(%q) = %q，应该是空串", bad, got)
		}
	}
}

// 已经有磁力的条目不翻详情页：磁力猫正常走不到详情页，别白打十几次请求。
func TestFillMagnetsSkipsFilled(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte(`<a href="magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567">x</a>`))
	}))
	defer srv.Close()

	items := []Item{
		{Title: "有磁力", URL: srv.URL, Magnet: "magnet:?xt=urn:btih:AAAA"},
		{Title: "没磁力", URL: srv.URL},
	}
	fillMagnetsFromDetails(context.Background(), srv.Client(), items)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Errorf("详情页请求了 %d 次，应该只有没磁力的那一条（1 次）", n)
	}
	if !strings.Contains(items[1].Magnet, "0123456789abcdef") {
		t.Errorf("兜底没补上磁力：%q", items[1].Magnet)
	}
	if items[0].Magnet != "magnet:?xt=urn:btih:AAAA" {
		t.Errorf("已有磁力被覆盖了：%q", items[0].Magnet)
	}
}

// 详情页正文同样可能塞在 window.atob 里，兜底要先解一层再找磁力。
func TestFillMagnetsDecodesPayload(t *testing.T) {
	inner := `\t<a id="down-url" href="magnet:?xt=urn:btih:9ABCDEF0123456789ABCDEF0123456789ABCDEF0">m</a>`
	page := `<html><script>document.getElementById("search-box").innerHTML=decodeURIComponent(` +
		`window.atob("` + b64(inner) + `"))</script></html>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	items := []Item{{Title: "t", URL: srv.URL}}
	fillMagnetsFromDetails(context.Background(), srv.Client(), items)
	if !strings.Contains(items[0].Magnet, "9ABCDEF0123456789ABCDEF0123456789ABCDEF0") {
		t.Fatalf("没从 payload 里解出磁力：%q", items[0].Magnet)
	}
}
