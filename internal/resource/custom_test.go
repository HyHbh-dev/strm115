package resource

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// 用户会怎么抄地址：竖线、全角竖线、空格、只有地址、带注释。
func TestParseCustomSites(t *testing.T) {
	raw := strings.Join([]string{
		"馒头 | https://pt.example.com/api/torznab/api?apikey=abc&q={q}",
		"高清时光｜https://hd.example.com/rss?search={query}",
		"MyIndexer https://jackett.local/api/v2.0/indexers/all/results/torznab/api?apikey=k",
		"https://nyaa.si/?page=rss&q={q}",
		"# 下面这行是坏的",
		"没地址的一行",
		"坏地址 | ftp://example.com/rss",
		// 和第一条一样，去重
		"重复 | https://pt.example.com/api/torznab/api?apikey=abc&q={q}",
	}, "\n")

	got := ParseCustomSites(raw)
	if len(got) != 4 {
		t.Fatalf("应解析出 4 个源，实际 %d：%+v", len(got), got)
	}
	wantNames := []string{"馒头", "高清时光", "MyIndexer", "nyaa.si"}
	for i, want := range wantNames {
		if got[i].Name != want {
			t.Errorf("第 %d 个源名称 = %q，期望 %q", i, got[i].Name, want)
		}
	}
	// key 里不能出现明文地址（会进接口响应，带着 apikey）
	for _, c := range got {
		if strings.Contains(c.Key, "apikey") || strings.Contains(c.Key, "example.com") {
			t.Errorf("key 泄露了地址：%q", c.Key)
		}
		if !strings.HasPrefix(c.Key, "custom:") {
			t.Errorf("key 应带 custom: 前缀，实际 %q", c.Key)
		}
	}
	// 缺省名称取域名
	if got[3].Name != "nyaa.si" {
		t.Errorf("只填地址时应取域名，实际 %q", got[3].Name)
	}
}

func TestCustomSiteSearchURL(t *testing.T) {
	cases := []struct{ url, q, want string }{
		{"https://a.com/rss?q={q}", "沙丘 2", "https://a.com/rss?q=%E6%B2%99%E4%B8%98+2"},
		{"https://a.com/rss?search={query}", "dune", "https://a.com/rss?search=dune"},
		{"https://a.com/rss", "dune", "https://a.com/rss?q=dune"},
		{"https://a.com/rss?apikey=k", "dune", "https://a.com/rss?apikey=k&q=dune"},
	}
	for _, c := range cases {
		site := CustomSite{URL: c.url}
		if got := site.searchURL(c.q); got != c.want {
			t.Errorf("searchURL(%q, %q) = %q，期望 %q", c.url, c.q, got, c.want)
		}
	}
}

// Torznab 的字段全在 <torznab:attr name=... value=.../> 里，
// 不认这个格式，自定义源就只能拿到标题、没有磁力可点。
func TestCustomItemsTorznab(t *testing.T) {
	items, err := parseRSS(readFixture(t, "torznab.xml"))
	if err != nil {
		t.Fatalf("解析失败：%v", err)
	}
	got := customItems(items, CustomSite{Key: "custom:abc", Name: "馒头"}, 10)
	if len(got) != 3 {
		t.Fatalf("应得 3 条（空标题那条要丢掉），实际 %d", len(got))
	}

	// 第一条：infohash 拼磁力、seeders/size 都从 attr 读
	first := got[0]
	if !strings.HasPrefix(first.Magnet, "magnet:?xt=urn:btih:0123456789ABCDEF0123456789ABCDEF01234567") {
		t.Errorf("磁力没拼出来：%q", first.Magnet)
	}
	if first.Seeders != 42 {
		t.Errorf("做种数 = %d，期望 42", first.Seeders)
	}
	if first.Size != 21474836480 {
		t.Errorf("体积 = %d，期望 21474836480", first.Size)
	}
	if first.Torrent != "https://pt.example.com/download/1001.torrent" {
		t.Errorf("种子直链 = %q", first.Torrent)
	}
	if first.Source != "馒头" || first.SourceKey != "custom:abc" || first.Kind != "torrent" {
		t.Errorf("来源标记不对：%+v", first)
	}
	if first.Date != "2026-11-07" {
		t.Errorf("日期 = %q，期望 2026-11-07", first.Date)
	}

	// 第二条：enclosure 就是磁力，原样保留（xml 实体要还原）
	second := got[1]
	if !strings.HasPrefix(second.Magnet, "magnet:?xt=urn:btih:AAAABBBBCCCCDDDDEEEEFFFF0000111122223333") {
		t.Errorf("magnet enclosure 没识别：%q", second.Magnet)
	}
	if strings.Contains(second.Magnet, "&amp;") {
		t.Errorf("磁力里的 & 没还原：%q", second.Magnet)
	}

	// 第三条：只有 .torrent，没有 infohash → Torrent 有值、Magnet 为空
	third := got[2]
	if third.Magnet != "" {
		t.Errorf("没有 infohash 时不该拼磁力：%q", third.Magnet)
	}
	if !strings.HasSuffix(third.Torrent, ".torrent") {
		t.Errorf("种子直链没识别：%q", third.Torrent)
	}
	if third.Size != 16106127360 {
		t.Errorf("enclosure length 没兜住：%d", third.Size)
	}
}

func TestCustomItemsLimit(t *testing.T) {
	items, err := parseRSS(readFixture(t, "torznab.xml"))
	if err != nil {
		t.Fatal(err)
	}
	if got := customItems(items, CustomSite{Name: "x"}, 2); len(got) != 2 {
		t.Fatalf("limit 没生效，实际 %d 条", len(got))
	}
}

// 测试接口的参数校验：这些错误要让用户看到「你填错了」，
// 而不是一个 15 秒的空白等待。
func TestServiceTestRejectsBadInput(t *testing.T) {
	s := New(nil)
	ctx := context.Background()

	if _, err := s.Test(ctx, TestOptions{Kind: "tg", Target: "  "}); err == nil {
		t.Error("空目标应该报错")
	}
	if _, err := s.Test(ctx, TestOptions{Kind: "site", Target: "不存在的站"}); err == nil ||
		!strings.Contains(err.Error(), "不认识") {
		t.Errorf("未知站点应给出可读错误，实际 %v", err)
	}
	if _, err := s.Test(ctx, TestOptions{Kind: "custom", Target: "我就随便写写"}); err == nil {
		t.Error("不像地址的一行应该报错")
	}
	if _, err := s.Test(ctx, TestOptions{Kind: "magic", Target: "x"}); err == nil {
		t.Error("未知 kind 应该报错")
	}
}

// 测试结果里不能回显 apikey：这行会显示在界面上，截图发出去就泄露了。
func TestMaskURL(t *testing.T) {
	cases := map[string]string{
		"https://pt.example.com/api?apikey=secret&q=dune":  "https://pt.example.com/api?apikey=***&q=dune",
		"https://pt.example.com/api?q=dune&passkey=abc123": "https://pt.example.com/api?q=dune&passkey=***",
		"https://pt.example.com/rss?q=dune":                "https://pt.example.com/rss?q=dune",
		"https://pt.example.com/rss":                       "https://pt.example.com/rss",
	}
	for in, want := range cases {
		if got := maskURL(in); got != want {
			t.Errorf("maskURL(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// t.me 会把同一频道的连续请求挂到超时，所以频道页有 60 秒缓存。
// 这里验四件事：第二次不再抓、不同关键词分开算、过期后重抓、失败不缓存。
func TestTelegramCache(t *testing.T) {
	s := New(nil)
	calls := 0
	var fail bool
	s.fetchTG = func(ctx context.Context, hc *http.Client, ch, q string, limit int) ([]Item, error) {
		calls++
		if fail {
			return nil, errors.New("t.me 返回 429")
		}
		return []Item{{Title: ch + "/" + q}}, nil
	}
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return at }
	ctx := context.Background()

	// 第一次：真抓
	items, cached, err := s.telegram(ctx, nil, "QukanMovie", "沙丘", 5, false)
	if err != nil || cached || calls != 1 || len(items) != 1 {
		t.Fatalf("第一次应真抓：cached=%v calls=%d err=%v", cached, calls, err)
	}

	// 第二次同参：命中缓存，不再抓
	_, cached, _ = s.telegram(ctx, nil, "QukanMovie", "沙丘", 5, false)
	if !cached || calls != 1 {
		t.Fatalf("第二次应命中缓存：cached=%v calls=%d", cached, calls)
	}

	// 关键词不同：必须重新抓，否则搜什么都是上一次的结果
	_, cached, _ = s.telegram(ctx, nil, "QukanMovie", "繁花", 5, false)
	if cached || calls != 2 {
		t.Fatalf("换关键词应重新抓：cached=%v calls=%d", cached, calls)
	}

	// 过期（61 秒后）：重抓
	at = at.Add(61 * time.Second)
	_, cached, _ = s.telegram(ctx, nil, "QukanMovie", "沙丘", 5, false)
	if cached || calls != 3 {
		t.Fatalf("过期后应重新抓：cached=%v calls=%d", cached, calls)
	}

	// 手动刷新：即使没过期也重抓（界面上的「重搜」）
	_, cached, _ = s.telegram(ctx, nil, "QukanMovie", "沙丘", 5, true)
	if cached || calls != 4 {
		t.Fatalf("refresh 应强制重抓：cached=%v calls=%d", cached, calls)
	}

	// 失败不写缓存：修好之后应当立刻能拿到结果，而不是继续吃 60 秒的错
	fail = true
	if _, _, err := s.telegram(ctx, nil, "quarkshare", "沙丘", 5, false); err == nil {
		t.Fatal("应返回错误")
	}
	fail = false
	before := calls
	if _, cached, _ := s.telegram(ctx, nil, "quarkshare", "沙丘", 5, false); cached || calls != before+1 {
		t.Fatalf("失败不该被缓存：cached=%v calls=%d", cached, calls)
	}
}
