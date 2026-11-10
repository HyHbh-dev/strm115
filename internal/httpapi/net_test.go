package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path"
	"strings"
	"sync/atomic"
	"testing"

	"strm115/internal/douban"
	"strm115/internal/imdb"
)

// 中文名才值得去 IMDb 换英文名：英文标题本来就在英文索引里能搜到。
func TestHasCJK(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"沙丘2 2024", true},
		{"流浪地球 2", true},
		{"進撃の巨人", true}, // 日文汉字 + 假名
		{"Dune Part Two", false},
		{"Oppenheimer 2023", false},
		{"Spider-Man: Across the Spider-Verse", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hasCJK(c.in); got != c.want {
			t.Errorf("hasCJK(%q) = %v，想要 %v", c.in, got, c.want)
		}
	}
}

// hasLatin 用来挡掉 IMDb 返回的另一条中文片名。
func TestHasLatin(t *testing.T) {
	if !hasLatin("Dune: Part Two") {
		t.Error("英文标题应判定有拉丁字母")
	}
	if hasLatin("沙丘2") {
		t.Error("纯中文标题不该判定有拉丁字母")
	}
}

// enLookupState 决定前端要不要解释「种子站拿什么词搜的」。
// fail 这一档很关键：它正是「中文搜不到种子」时唯一能看到的线索。
func TestEnLookupState(t *testing.T) {
	cases := []struct {
		q, kind, siteQ, want string
	}{
		{"沙丘2", "", "Dune: Part Two 2024", "ok"},
		{"沙丘2", "", "", "fail"},
		{"沙丘2", "tg", "", ""},      // 只搜 TG，不走种子站
		{"Dune", "", "", ""},       // 本来英文，不用换
		{"沙丘2", "", "沙丘2", "fail"}, // 换了但没换出东西
	}
	for _, c := range cases {
		if got := enLookupState(c.q, c.kind, c.siteQ); got != c.want {
			t.Errorf("enLookupState(%q,%q,%q) = %q，想要 %q", c.q, c.kind, c.siteQ, got, c.want)
		}
	}
}

// queryYear 用来在 IMDb 结果里挑对年份 —— 「三体 2023」不能被 2024 版顶掉。
func TestQueryYear(t *testing.T) {
	cases := map[string]string{
		"沙丘2 2024":         "2024",
		"流浪地球2":            "",
		"Oppenheimer 2023": "2023",
		"tf1 1999":         "1999",
		"":                 "",
	}
	for in, want := range cases {
		if got := queryYear(in); got != want {
			t.Errorf("queryYear(%q) = %q，想要 %q", in, got, want)
		}
	}
}

// pickEnglish 只挑拉丁标题，且优先年份对得上的那一条。
func TestPickEnglish(t *testing.T) {
	resp := &imdb.SearchResponse{Results: []imdb.SearchResult{
		{Title: "3 Body Problem", Year: "2024"},
		{Title: "三体", Year: "2023"},
		{Title: "Three-Body", Year: "2023"},
	}}
	if got := pickEnglish(resp, "三体 2023"); got != "Three-Body 2023" {
		t.Errorf("应该挑 2023 年的 Three-Body，实际 %q", got)
	}
	// 查询里没年份时，取第一条英文结果
	if got := pickEnglish(resp, "三体"); got != "3 Body Problem 2024" {
		t.Errorf("没年份时应取第一条英文结果，实际 %q", got)
	}
	if got := pickEnglish(nil, "三体"); got != "" {
		t.Errorf("空结果应返回空串，实际 %q", got)
	}
}

// TG 频道用不带年份的英文词：帖子标题里通常不写年份，
// 但片名本身带年份的（2001: A Space Odyssey）不能把中间的数字切掉。
func TestChannelQueryFor(t *testing.T) {
	for _, tc := range []struct{ q, site, want string }{
		{"沙丘2", "Dune: Part Two 2024", "Dune: Part Two"},
		{"三体 2023", "Three-Body 2023", "Three-Body"},
		{"Dune", "Dune 2024", ""}, // 本来就是英文，别重复搜
		{"流浪地球", "流浪地球 2019", ""}, // 没换成英文名
		{"沙丘2", "", ""},
	} {
		if got := channelQueryFor(tc.q, tc.site); got != tc.want {
			t.Errorf("channelQueryFor(%q, %q) = %q，想要 %q", tc.q, tc.site, got, tc.want)
		}
	}
}

func TestStripYear(t *testing.T) {
	for in, want := range map[string]string{
		"Dune Part Two 2024":    "Dune Part Two",
		"Three-Body 2023":       "Three-Body",
		"2001: A Space Odyssey": "2001: A Space Odyssey",
		"Dune":                  "Dune",
		"沙丘2 2024":              "沙丘2",
		"2024":                  "2024",
	} {
		if got := stripYear(in); got != want {
			t.Errorf("stripYear(%q) = %q，想要 %q", in, got, want)
		}
	}
}

/* ==================== 资源搜索的豆瓣换名 ====================
 * 「找资源国内为什么也刷新慢」的根因：中文查询一律 TMDB(3s) → IMDb(4s)，
 * 国产剧根本不需要英文名，白等一轮还多打一次 t.me。下面这些用例钉住
 * 「豆瓣认片 → 国产跳过 / 外国片用 original_title / 关掉就退回老链路」。
 */

// fakeDoubanName 同时扮演 suggest 与 rexxar 两个域名（真实反代也是这么分流）。
type fakeDoubanName struct {
	srv   *httptest.Server
	calls atomic.Int32
}

func withFakeDoubanName(t *testing.T, s *Server, suggests, details map[string]string) *fakeDoubanName {
	t.Helper()
	f := &fakeDoubanName{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/j/subject_suggest":
			body := suggests[r.URL.Query().Get("q")]
			if body == "" {
				body = "[]"
			}
			_, _ = w.Write([]byte(body))
		case strings.HasPrefix(r.URL.Path, "/rexxar/api/v2/movie/") ||
			strings.HasPrefix(r.URL.Path, "/rexxar/api/v2/tv/"):
			body, ok := details[path.Base(r.URL.Path)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.srv.Close)
	if err := s.st.SetSetting(t.Context(), "douban_base", f.srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetSetting(t.Context(), "douban_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	return f
}

const domSuggest = `[{"id":"1001","title":"漫长的季节","type":"tv","year":"2023",
 "sub_title":"范伟 / 秦昊","episode":"12","img":""}]`
const domDetail = `{"id":"1001","title":"漫长的季节","year":"2023","type":"tv",
 "card_subtitle":"2023 / 中国大陆 / 剧情","pubdate":["2023-04-22(中国大陆)"],
 "original_title":"漫长的季节","episodes_count":"12"}`
const foreignSuggest = `[{"id":"2002","title":"沙丘2","type":"movie","year":"2024",
 "sub_title":"提莫西·查拉梅 / 赞达亚","img":""}]`
const foreignDetail = `{"id":"2002","title":"沙丘2","year":"2024","type":"movie",
 "card_subtitle":"2024 / 美国 / 科幻","pubdate":["2024-03-08(美国)"],
 "original_title":"Dune: Part Two"}`

// 国产剧：豆瓣认出来之后 siteQ 必须是空（按中文搜），而且一次都不该问 TMDB/IMDb。
func TestResolveSearchNamesDomesticSkipsEnglishLookup(t *testing.T) {
	s, _ := newServerWithStore(t)
	f := withFakeDoubanName(t, s,
		map[string]string{"漫长的季节": domSuggest},
		map[string]string{"1001": domDetail})
	fallbacks := 0
	got := s.resolveSearchNamesWith(t.Context(), "漫长的季节", "", func(context.Context, string) string {
		fallbacks++
		return "TMDB-GUESS"
	})
	if !got.Domestic || got.SiteQuery != "" || got.Source != "douban" {
		t.Fatalf("国产剧不该换英文名: %+v", got)
	}
	if fallbacks != 0 {
		t.Errorf("国产剧还去问了 TMDB/IMDb %d 次", fallbacks)
	}
	if n := f.calls.Load(); n != 2 {
		t.Errorf("该只有 suggest + detail 两个请求，实际 %d", n)
	}
}

// 外国片：用豆瓣的 original_title，后面仍然不用打 TMDB/IMDb。
func TestResolveSearchNamesDoubanOriginalTitle(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeDoubanName(t, s,
		map[string]string{"沙丘2": foreignSuggest},
		map[string]string{"2002": foreignDetail})
	fallbacks := 0
	got := s.resolveSearchNamesWith(t.Context(), "沙丘2 2024", "", func(context.Context, string) string {
		fallbacks++
		return "TMDB-GUESS"
	})
	if got.Domestic || got.Source != "douban" {
		t.Fatalf("外国片该走豆瓣: %+v", got)
	}
	if got.SiteQuery != "Dune: Part Two 2024" {
		t.Errorf("英文原名没拼上年份: %q", got.SiteQuery)
	}
	if fallbacks != 0 {
		t.Error("豆瓣换到了英文名还去问了 TMDB/IMDb")
	}
}

// 用户/认片器给了英文名，或查询本来就是英文：一个豆瓣请求都不该发。
func TestResolveSearchNamesExplicitOrEnglish(t *testing.T) {
	s, _ := newServerWithStore(t)
	f := withFakeDoubanName(t, s, nil, nil)
	fallbacks := 0
	fb := func(context.Context, string) string { fallbacks++; return "X" }

	if got := s.resolveSearchNamesWith(t.Context(), "沙丘2", "Dune: Part Two", fb); got.Source != "explicit" || got.SiteQuery != "Dune: Part Two" {
		t.Errorf("显式英文名没被采用: %+v", got)
	}
	if got := s.resolveSearchNamesWith(t.Context(), "Dune Part Two", "", fb); got.Source != "none" || got.SiteQuery != "" {
		t.Errorf("英文查询不该换名: %+v", got)
	}
	if n := f.calls.Load(); n != 0 {
		t.Errorf("这两种情况不该打豆瓣，实际 %d 个请求", n)
	}
	if fallbacks != 0 {
		t.Errorf("这两种情况不该退回 TMDB/IMDb，实际 %d 次", fallbacks)
	}
}

// 豆瓣关掉 / 认不出来 → 退回原来的 TMDB → IMDb，不能让整轮搜索没了结果。
func TestResolveSearchNamesFallsBack(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeDoubanName(t, s,
		map[string]string{"某部查不到的片": `[{"id":"9999","title":"另一部完全不相干的片","type":"movie","year":"2001"}]`},
		nil)
	if err := s.st.SetSetting(t.Context(), "douban_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	fallbacks := 0
	got := s.resolveSearchNamesWith(t.Context(), "某部查不到的片", "", func(context.Context, string) string {
		fallbacks++
		return "TMDB-GUESS"
	})
	if got.Source != "fallback" || got.SiteQuery != "TMDB-GUESS" || got.Domestic {
		t.Fatalf("关掉豆瓣该退回老链路: %+v", got)
	}
	if fallbacks != 1 {
		t.Errorf("fallback 该调 1 次，实际 %d", fallbacks)
	}

	// 开着豆瓣但 suggest 第一条是别的片，也不能硬拿那条去搜。
	if err := s.st.SetSetting(t.Context(), "douban_enabled", "1"); err != nil {
		t.Fatal(err)
	}
	got = s.resolveSearchNamesWith(t.Context(), "某部查不到的片", "", func(context.Context, string) string {
		return "TMDB-GUESS"
	})
	if got.Source != "fallback" {
		t.Errorf("豆瓣认错了片不该采用: %+v", got)
	}
}

func TestTitleLooksLike(t *testing.T) {
	cases := []struct {
		q, hit string
		want   bool
	}{
		{"沙丘2 2024", "沙丘2", true},
		{"漫长的季节", "漫长的季节", true},
		{"三体", "三体 第一季", true},
		{"沙丘2", "沙丘", true},
		{"沙丘2", "完全不相干", false},
		{"", "沙丘2", false},
	}
	for _, c := range cases {
		if got := titleLooksLike(c.q, c.hit); got != c.want {
			t.Errorf("titleLooksLike(%q,%q) = %v，想要 %v", c.q, c.hit, got, c.want)
		}
	}
}

func TestIsDomesticDouban(t *testing.T) {
	cases := []struct {
		card, release string
		want          bool
	}{
		{"2023 / 中国大陆 / 剧情", "", true},
		{"2024 / 中国香港 / 动作", "", true},
		{"", "2023-04-22(中国大陆)", true},
		{"2024 / 美国 / 科幻", "2024-03-08(美国)", false},
		{"", "", false},
	}
	for _, c := range cases {
		it := &douban.Item{CardSubtitle: c.card, ReleaseText: c.release}
		if got := isDomesticDouban(it); got != c.want {
			t.Errorf("isDomesticDouban(%q,%q) = %v，想要 %v", c.card, c.release, got, c.want)
		}
	}
}
