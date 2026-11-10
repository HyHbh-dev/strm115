package resource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// UIndex 的搜索页和首页共用同一套 hp-table 行结构：磁力 href + details.php 锚文本 +
// hp-td-size/seed/leech。这里拿真页面裁下来的片段验证。
func TestParseUIndex(t *testing.T) {
	items := parseUIndex(fixture(t, "uindex_search.html"), "The Odyssey", 5)
	if len(items) != 4 {
		t.Fatalf("解析出 %d 条，想要 4 条（表头行要被跳过）", len(items))
	}
	it := items[0]
	if !strings.Contains(it.Title, "The Odyssey") {
		t.Errorf("标题 = %q", it.Title)
	}
	if !strings.HasPrefix(it.Magnet, "magnet:?xt=urn:btih:") {
		t.Errorf("磁力 = %q", it.Magnet)
	}
	if it.Magnet != "" && !strings.Contains(it.Magnet, "&dn=") {
		t.Errorf("磁力里的 &amp; 没反转义：%q", it.Magnet)
	}
	if it.Size <= 0 || it.Seeders <= 0 {
		t.Errorf("体积/做种没解析出来：size=%d seeders=%d", it.Size, it.Seeders)
	}
	if it.SourceKey != "uindex" || it.Kind != "torrent" || !strings.HasPrefix(it.URL, uindexBase) {
		t.Errorf("来源字段不对：%+v", it)
	}
}

func TestParseUIndexLimit(t *testing.T) {
	items := parseUIndex(fixture(t, "uindex_search.html"), "The Odyssey", 2)
	if len(items) != 2 {
		t.Fatalf("limit 没生效，拿到 %d 条", len(items))
	}
}

func TestIsCloudflare(t *testing.T) {
	page := []byte(`<!DOCTYPE html><title>Just a moment...</title><script src="https://challenges.cloudflare.com/..."></script>`)
	if !isCloudflare(page) {
		t.Fatal("Cloudflare 挑战页没认出来")
	}
	if isCloudflare(fixture(t, "uindex_search.html")) {
		t.Fatal("正常结果页被误判成挑战页")
	}
}

// TheRarBG 列表页没有磁力，只有 /post-detail/ 链接；标题、体积、做种、日期
// 都得从 td 的单字母 class 里取。
func TestParseTheRarBGList(t *testing.T) {
	items := parseTheRarBGList(fixture(t, "therarbg_list.html"), 5)
	if len(items) != 3 {
		t.Fatalf("解析出 %d 条，想要 3 条", len(items))
	}
	it := items[0]
	if it.Title != "Dune Part Two (2024) [1080p] [BluRay]" {
		t.Errorf("标题 = %q", it.Title)
	}
	if it.Size == 0 || it.Seeders != 465 || it.Leechers != 62 {
		t.Errorf("元数据没解析出来：size=%d se=%d le=%d", it.Size, it.Seeders, it.Leechers)
	}
	if it.Date != "2026-04-24" || it.Category != "Movies" {
		t.Errorf("日期/分类 = %q / %q", it.Date, it.Category)
	}
	if !strings.HasPrefix(it.URL, therarbgBase+"/post-detail/") || it.SourceKey != "therarbg" {
		t.Errorf("详情页链接/来源 = %q / %q", it.URL, it.SourceKey)
	}
}

// 详情页里补磁力：用假服务器，避免单测真连 therarbg.com。
func TestFillMagnetsFromDetails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(fixture(t, "therarbg_detail.html"))
	}))
	defer srv.Close()

	items := []Item{{Title: "Dune", URL: srv.URL}}
	fillMagnetsFromDetails(context.Background(), srv.Client(), items)
	if !strings.HasPrefix(items[0].Magnet, "magnet:?xt=urn:btih:") {
		t.Fatalf("没补上磁力：%q", items[0].Magnet)
	}
	if !strings.Contains(items[0].Magnet, "&dn=") {
		t.Errorf("磁力里的 &amp; 没反转义：%q", items[0].Magnet)
	}
}

func TestATOILoose(t *testing.T) {
	for in, want := range map[string]int{"21,716": 21716, "465": 465, "1\u00a0234": 1234, "": 0} {
		if got := atoiLoose(in); got != want {
			t.Errorf("atoiLoose(%q) = %d，想要 %d", in, got, want)
		}
	}
}

// ==================== 缓存 / 合并 ====================

// 「全部 → 点种子站」不该重新打网络：同一 key 第二次直接拿缓存。
func TestCachedSource(t *testing.T) {
	s := New(nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	calls := 0
	fetch := func() ([]Item, error) {
		calls++
		return []Item{{Title: "A"}}, nil
	}

	for i := 0; i < 3; i++ {
		_, cached, err := s.cachedSource("site\u0010dune", siteCacheTTL, false, fetch)
		if err != nil {
			t.Fatal(err)
		}
		if i > 0 && !cached {
			t.Errorf("第 %d 次没命中缓存", i+1)
		}
	}
	if calls != 1 {
		t.Fatalf("实际抓了 %d 次，想要 1 次", calls)
	}

	// 过期后要重新抓
	now = now.Add(siteCacheTTL + time.Second)
	if _, cached, _ := s.cachedSource("site\u0010dune", siteCacheTTL, false, fetch); cached {
		t.Error("过期了还在用缓存")
	}
	if calls != 2 {
		t.Fatalf("过期后没重新抓，calls=%d", calls)
	}

	// 手动刷新强制绕过缓存
	if _, cached, _ := s.cachedSource("site\u0010dune", siteCacheTTL, true, fetch); cached {
		t.Error("refresh 应该强制跳过缓存")
	}
	if calls != 3 {
		t.Fatalf("refresh 没重新抓，calls=%d", calls)
	}
}

// 失败做短 TTL 负缓存：切换筛选时不必每次都重等一个已挂掉的源超时，
// 但 30 秒后必须自己重试，不能把「站点恢复了」一直藏着。
func TestCachedSourceFailureHasShortTTL(t *testing.T) {
	s := New(nil)
	now := time.Now()
	s.now = func() time.Time { return now }
	calls := 0
	fail := func() ([]Item, error) {
		calls++
		return nil, context.DeadlineExceeded
	}
	for i := 0; i < 2; i++ {
		_, cached, err := s.cachedSource("bad", siteCacheTTL, false, fail)
		if err == nil {
			t.Fatal("应该把错误传出去")
		}
		if i == 1 && !cached {
			t.Fatal("第二次应该命中负缓存")
		}
	}
	if calls != 1 {
		t.Fatalf("负缓存没生效，抓了 %d 次", calls)
	}
	now = now.Add(failCacheTTL + time.Second)
	if _, cached, _ := s.cachedSource("bad", siteCacheTTL, false, fail); cached || calls != 2 {
		t.Fatalf("负缓存过期后应重试：cached=%v calls=%d", cached, calls)
	}
}

func TestMergeItemsDedup(t *testing.T) {
	a := []Item{{Title: "A", URL: "https://t.me/x/1"}, {Title: "B", Magnet: "magnet:?xt=urn:btih:B"}}
	b := []Item{{Title: "A 重复", URL: "https://t.me/x/1"}, {Title: "C", Magnet: "magnet:?xt=urn:btih:C"}}
	got := mergeItems(a, b)
	if len(got) != 3 {
		t.Fatalf("合并后 %d 条，想要 3 条：%+v", len(got), got)
	}
	if got[0].Title != "A" || got[2].Title != "C" {
		t.Errorf("顺序/去重不对：%+v", got)
	}
}

// TG 频道要按中文 + TMDB 英文名各搜一遍，合并去重；整轮结果还要能被
// 「全部 → 种子站」这种切换复用，不能重新打 t.me。
func TestSearchChannelQueryAndCache(t *testing.T) {
	s := New(nil)
	var mu sync.Mutex
	var calls []string
	s.fetchTG = func(_ context.Context, _ *http.Client, _, q string, _ int) ([]Item, error) {
		mu.Lock()
		calls = append(calls, q)
		mu.Unlock()
		tag := "中文"
		if q == "Dune Part Two" {
			tag = "英文"
		}
		return []Item{{
			Title: "A " + tag, URL: "https://t.me/ch/" + tag,
			Kind: "tg", SourceKey: "tg:ch",
		}}, nil
	}

	opt := Options{
		Query: "沙丘2", ChannelQuery: "Dune Part Two",
		Channels: []string{"ch"}, Sites: []string{"__no_such_site__"}, Limit: 5,
	}
	res, err := s.Search(context.Background(), opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("频道应该查两次（中文 + 英文），实际 %v", calls)
	}
	if len(res.Items) != 2 || res.ChannelQuery != "Dune Part Two" {
		t.Fatalf("合并结果不对：%+v", res)
	}
	if res.Cached {
		t.Fatal("第一轮不该标成缓存")
	}

	// 第二轮：全部命中源级缓存，一次网络都不打
	res2, err := s.Search(context.Background(), opt)
	if err != nil || !res2.Cached || len(calls) != 2 {
		t.Fatalf("第二轮应该全走缓存：cached=%v calls=%v err=%v", res2.Cached, calls, err)
	}
	for _, st := range res2.Sources {
		if !st.Cached {
			t.Errorf("源 %s 没标缓存", st.Key)
		}
	}

	// 手动重搜：跳过缓存，重新查两种词
	opt.Refresh = true
	if _, err := s.Search(context.Background(), opt); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("refresh 后应该重新查两次，累计 %v", calls)
	}
}

func TestSourceCacheKeyVariegated(t *testing.T) {
	base := sourceCacheKey("tpb", "Dune", "", 20)
	for _, k := range []string{
		sourceCacheKey("tpb", "Dune 2024", "", 20),
		sourceCacheKey("tpb", "Dune", "http://127.0.0.1:7890", 20),
		sourceCacheKey("tpb", "Dune", "", 30),
		sourceCacheKey("nyaa", "Dune", "", 20),
	} {
		if k == base {
			t.Fatalf("缓存键撞车了：%q", k)
		}
	}
}

func TestSortItemsUsesAllQueries(t *testing.T) {
	items := []Item{
		{Title: "完全无关的标题", Magnet: "magnet:?1"},
		{Title: "Dune Part Two 2024", Magnet: "magnet:?2"},
	}
	// 只按中文名排时两条都是 0 分、维持原序；补上英文词后英文条目应该上位。
	sortItems(items, "沙丘2", "Dune Part Two")
	if items[0].Title != "Dune Part Two 2024" {
		t.Fatalf("英文查询没参与打分，第一条是 %q", items[0].Title)
	}
}
