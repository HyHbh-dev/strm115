package resource

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("读夹具 %s 失败: %v", name, err)
	}
	return b
}

// 夹具是 QukanMovie 频道搜「流浪地球」时的真实页面，取了两条消息。
func TestParseChannelFixture(t *testing.T) {
	items := parseChannel(string(readFixture(t, "tg.html")))
	if len(items) != 2 {
		t.Fatalf("应解析出 2 条消息，实际 %d", len(items))
	}
	first := items[0]
	if first.Title == "" || strings.Contains(first.Title, "<") {
		t.Errorf("标题没清洗干净: %q", first.Title)
	}
	if strings.HasPrefix(first.Title, "名称：") {
		t.Errorf("标题不该带「名称：」前缀: %q", first.Title)
	}
	if !strings.HasPrefix(first.URL, "https://t.me/QukanMovie/") {
		t.Errorf("消息地址不对: %q", first.URL)
	}
	if len(first.Date) != 10 || first.Date[4] != '-' {
		t.Errorf("日期格式不对: %q", first.Date)
	}
	if len(first.Links) == 0 {
		t.Fatal("应该从正文里找出网盘链接")
	}
	found115 := false
	for _, l := range first.Links {
		if strings.Contains(l, "115.com/s/") {
			found115 = true
		}
		if strings.Contains(l, "t.me/") {
			t.Errorf("t.me 链接不该算资源: %q", l)
		}
	}
	if !found115 {
		t.Errorf("应识别出 115 分享链接，实际 %v", first.Links)
	}
	// 115 分享要顺手解析成「码 + 提取码」，前端才能一键转存
	if first.ShareCode != "swhbjqa3zrk" || first.SharePwd != "8013" {
		t.Errorf("115 分享解析不对: code=%q pwd=%q", first.ShareCode, first.SharePwd)
	}
	if first.ShareURL != "https://115.com/s/swhbjqa3zrk?password=8013" {
		t.Errorf("分享链接应为规范形式，实际 %q", first.ShareURL)
	}
	if first.Snippet == "" {
		t.Error("摘要不该为空")
	}
}

func TestSplitTitleStripsPrefix(t *testing.T) {
	title, snippet := splitTitle("名称：某部电影 2160p\n描述：一段剧情简介\n第二行")
	if title != "某部电影 2160p" {
		t.Errorf("标题 = %q", title)
	}
	if !strings.Contains(snippet, "一段剧情简介") || strings.HasPrefix(snippet, "描述：") {
		t.Errorf("摘要 = %q", snippet)
	}
}

func TestIsResourceLink(t *testing.T) {
	yes := []string{
		"https://115.com/s/swhbjqa3zrk?password=8013",
		"https://pan.baidu.com/s/1abc",
		"https://www.alipan.com/s/xyz",
		"https://pan.quark.cn/s/abc",
		"magnet:?xt=urn:btih:ABCDEF",
	}
	for _, u := range yes {
		if !isResourceLink(u) {
			t.Errorf("%s 应该算资源链接", u)
		}
	}
	no := []string{"https://t.me/QukanMovie/1106", "https://t.me/moxuan8023", "https://example.com/x"}
	for _, u := range no {
		if isResourceLink(u) {
			t.Errorf("%s 不该算资源链接", u)
		}
	}
}

func TestParseApibayFixture(t *testing.T) {
	items, err := parseApibay(readFixture(t, "apibay.json"), "oppenheimer", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(items))
	}
	it := items[0]
	if !strings.Contains(it.Title, "Oppenheimer") {
		t.Errorf("标题 = %q", it.Title)
	}
	if !strings.HasPrefix(it.Magnet, "magnet:?xt=urn:btih:") {
		t.Errorf("磁力没拼出来: %q", it.Magnet)
	}
	if it.Seeders <= 0 || it.Size <= 0 {
		t.Errorf("做种/体积没解析: seeders=%d size=%d", it.Seeders, it.Size)
	}
	if len(it.Date) != 10 {
		t.Errorf("时间戳没转成日期: %q", it.Date)
	}
	if it.Source != "海盗湾" || it.Kind != "torrent" {
		t.Errorf("来源字段不对: %+v", it)
	}
}

func TestParseApibayRejectsHTML(t *testing.T) {
	if _, err := parseApibay([]byte("<html>blocked</html>"), "x", 10); err == nil {
		t.Fatal("被拦截时应该报错，而不是当空结果")
	}
}

func TestNyaaItems(t *testing.T) {
	items, err := parseRSS(readFixture(t, "nyaa.xml"))
	if err != nil {
		t.Fatal(err)
	}
	out := nyaaItems(items, 10)
	if len(out) == 0 {
		t.Fatal("没解析出条目")
	}
	it := out[0]
	if !strings.HasPrefix(it.Magnet, "magnet:?xt=urn:btih:") {
		t.Errorf("nyaa:infoHash 没取到，磁力是 %q", it.Magnet)
	}
	if it.Seeders <= 0 {
		t.Errorf("nyaa:seeders 没取到: %d", it.Seeders)
	}
	if it.Size <= 0 {
		t.Errorf("nyaa:size 没转换: %d", it.Size)
	}
	if !strings.HasPrefix(it.URL, "https://nyaa.si/view/") {
		t.Errorf("详情页地址不对: %q", it.URL)
	}
}

func TestDMHYItems(t *testing.T) {
	items, err := parseRSS(readFixture(t, "dmhy.xml"))
	if err != nil {
		t.Fatal(err)
	}
	out := dmhyItems(items, 10)
	if len(out) == 0 {
		t.Fatal("没解析出条目")
	}
	if !strings.HasPrefix(out[0].Magnet, "magnet:?xt=urn:btih:") {
		t.Errorf("enclosure 里的磁力没取到: %q", out[0].Magnet)
	}
	if !strings.Contains(out[0].Title, "龍珠") {
		t.Errorf("标题 = %q", out[0].Title)
	}
}

func TestMikanItems(t *testing.T) {
	items, err := parseRSS(readFixture(t, "mikan.xml"))
	if err != nil {
		t.Fatal(err)
	}
	out := mikanItems(items, 10)
	if len(out) == 0 {
		t.Fatal("没解析出条目")
	}
	it := out[0]
	if !strings.Contains(it.Torrent, "mikanani.me/Download/") {
		t.Errorf("种子直链没取到: %q", it.Torrent)
	}
	if it.Magnet != "" {
		t.Error("Mikan 不给 info hash，不该硬编磁力")
	}
	if it.Size <= 0 {
		t.Errorf("体积没取到: %d", it.Size)
	}
}

func TestParseSizeText(t *testing.T) {
	cases := map[string]int64{
		"1.4 GiB":  1503238553,
		"8.9GB":    9556302233,
		"700 MB":   734003200,
		"21662713": 21662713,
		"":         0,
		"whatever": 0,
	}
	for in, want := range cases {
		if got := parseSizeText(in); got != want {
			t.Errorf("parseSizeText(%q) = %d，想要 %d", in, got, want)
		}
	}
}

// 排序是「能直接用的排前面」：有磁力 > TG 消息 > 只能点开看。
func TestSortItems(t *testing.T) {
	items := []Item{
		{Title: "只有网页", Kind: "torrent", URL: "x"},
		{Title: "TG 消息", Kind: "tg", Links: []string{"115"}},
		{Title: "小种子", Kind: "torrent", Magnet: "magnet:?a", Seeders: 5},
		{Title: "大种子", Kind: "torrent", Magnet: "magnet:?b", Seeders: 500},
	}
	sortItems(items, "")
	got := []string{items[0].Title, items[1].Title, items[2].Title, items[3].Title}
	want := []string{"大种子", "小种子", "TG 消息", "只有网页"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("排序 = %v，想要 %v", got, want)
		}
	}
}

// 搜中文时海盗湾会返回全站热门，这些条目的标题里没有关键词，必须被筛掉。
func TestParseApibayDropsIrrelevant(t *testing.T) {
	fixture := `[{"id":"1","name":"Spider-Man Brand New Day (2026) [1080p]","info_hash":"AAA","seeders":"99","leechers":"1","size":"100","added":"1700000000","category":"207"},
	             {"id":"2","name":"流浪地球2.The.Wandering.Earth.II.2023.mkv","info_hash":"BBB","seeders":"5","leechers":"1","size":"200","added":"1700000000","category":"207"}]`
	items, err := parseApibay([]byte(fixture), "流浪地球", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Title, "流浪地球") {
		t.Fatalf("应只留下标题命中的那条，实际 %+v", items)
	}
}

// 相关性优先于「有没有磁力」：搜中文时 TG 帖该排在海盗湾的不相关结果前面。
func TestSortItemsPrefersRelevance(t *testing.T) {
	items := []Item{
		{Title: "Spider-Man (2026)", Kind: "torrent", Magnet: "magnet:?a", Seeders: 900},
		{Title: "流浪地球2 2160p 内封中字", Kind: "tg", Links: []string{"115"}},
	}
	sortItems(items, "流浪地球")
	if items[0].Kind != "tg" {
		t.Fatalf("中文命中项应排第一，实际 %q", items[0].Title)
	}
}

func TestNormalizeChannels(t *testing.T) {
	raw := "@QukanMovie, https://t.me/shareAliyun\nbdwpzhpd；iiShare  @QukanMovie"
	got := NormalizeChannels(raw)
	want := []string{"QukanMovie", "shareAliyun", "bdwpzhpd", "iiShare"}
	if len(got) != len(want) {
		t.Fatalf("解析出 %v，想要 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 个 = %q，想要 %q", i, got[i], want[i])
		}
	}
}

func TestNormalizeSitesFiltersUnknown(t *testing.T) {
	got := NormalizeSites("nyaa, 1337x, tpb,mikan")
	if len(got) != 3 || got[0] != "nyaa" || got[1] != "tpb" || got[2] != "mikan" {
		t.Errorf("应只保留认识的站，实际 %v", got)
	}
}

func TestSiteNamesCoversAll(t *testing.T) {
	names := SiteNames()
	for _, s := range Sites() {
		if names[s.Key] == "" {
			t.Errorf("%s 缺显示名", s.Key)
		}
	}
	if len(names) != len(DefaultSites) {
		t.Errorf("内置站数量 = %d，DefaultSites 有 %d 个（加了站忘了登记？）", len(names), len(DefaultSites))
	}
	// 这个断言看着傻，但它挡住「加了新站忘了登记默认值」这种低级错
	b, _ := json.Marshal(names)
	if !strings.Contains(string(b), "nyaa") {
		t.Errorf("序列化异常: %s", b)
	}
}

// Torrent Search Bot 🔎 风格的结果帖转发进频道后，抬头和图标字段不是片名。
func TestSplitTitleSkipsTorrentBotHeader(t *testing.T) {
	plain := "📜 Results for Dune Part Two\n" +
		"💾 3.1 GB · 🟢 465 · 📅 2026-04-24\n" +
		"Dune.Part.Two.2024.2160p.BluRay.x265\n" +
		"🧲 Magnet"
	title, _ := splitTitle(plain)
	if title != "Dune.Part.Two.2024.2160p.BluRay.x265" {
		t.Fatalf("标题取错了：%q", title)
	}
}

func TestApplyHuntMeta(t *testing.T) {
	it := &Item{}
	applyHuntMeta(it, "💾 3.1 GB · 🟢 465 · 🔴 62 · 📅 2026-04-24")
	gb := 3.1
	wantSize := int64(gb * (1 << 30))
	if it.Size != wantSize || it.Seeders != 465 || it.Leechers != 62 || it.Date != "2026-04-24" {
		t.Fatalf("图标字段没解析出来：%+v", it)
	}
	// 已有值不能被帖子里的文案覆盖
	it2 := &Item{Size: 1 << 30, Seeders: 9}
	applyHuntMeta(it2, "💾 3.1 GB · 🟢 465")
	if it2.Size != 1<<30 || it2.Seeders != 9 {
		t.Fatalf("已有元数据被覆盖了：%+v", it2)
	}
}
