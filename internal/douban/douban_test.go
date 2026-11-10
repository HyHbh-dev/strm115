package douban

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 夹具照抄真实响应（只留用到的字段）。豆瓣的字段名和层次都不能猜：
// 榜单是 subject_collection_items，详情是拍平的，搜索又是另一套。
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *int) {
	t.Helper()
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New("")
	c.setBases(srv.URL, srv.URL)
	return c, &hits
}

func TestSearchNormalizesAndDetectsKind(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("User-Agent"); !strings.Contains(got, "Mozilla") {
			t.Errorf("必须带浏览器 UA，否则豆瓣 418：%q", got)
		}
		if r.Header.Get("Referer") == "" {
			t.Error("必须带 Referer")
		}
		w.Write([]byte(`[
			{"episode":"","img":"http://img3.doubanio.com/a.jpg","title":"沙丘2",
			 "url":"https://movie.douban.com/subject/35575567/","type":"movie","year":"2024",
			 "sub_title":"Dune: Part Two","id":"35575567"},
			{"episode":"12","img":"https://img9.doubanio.com/b.jpg","title":"漫长的季节",
			 "type":"movie","year":"2023","sub_title":"漫长的季节","id":"35588177"}
		]`))
	})

	items, err := c.Search(context.Background(), "沙丘2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应返回 2 条，实际 %d", len(items))
	}
	if items[0].Kind != "movie" || items[0].Year != 2024 || items[0].ID != 35575567 {
		t.Fatalf("电影归一化不对: %+v", items[0])
	}
	if items[0].Poster != "https://img3.doubanio.com/a.jpg" {
		t.Errorf("http 图床要换成 https: %q", items[0].Poster)
	}
	// 豆瓣把剧的 type 也写成 movie，只认 type 会把它当电影
	if items[1].Kind != "tv" {
		t.Errorf("有 episode 的应该认成剧集: %+v", items[1])
	}
}

func TestDetailFallsBackToOtherKind(t *testing.T) {
	var paths []string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if strings.Contains(r.URL.Path, "/movie/") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Write([]byte(`{"id":35588177,"title":"漫长的季节","year":"2023","type":"tv",
			"episodes_count":12,"cover_url":"https://img1.doubanio.com/c.jpg",
			"rating":{"value":9.4},"intro":"桦林…","pubdate":["2023-04-22(中国大陆)"],
			"url":"https://movie.douban.com/subject/35588177/"}`))
	})

	// 传错类型（电影）也必须能拿到，并把 Kind 纠正成 tv
	it, err := c.Detail(context.Background(), "movie", 35588177)
	if err != nil {
		t.Fatal(err)
	}
	if it.Kind != "tv" {
		t.Errorf("应该纠正类型: %+v", it)
	}
	if it.Episodes != 12 || it.Rating != 9.4 {
		t.Errorf("详情字段没取到: %+v", it)
	}
	if it.ReleaseDate != "2023-04-22" {
		t.Errorf("上映日期应统一成 YYYY-MM-DD: %q（原文 %q）", it.ReleaseDate, it.ReleaseText)
	}
	if len(paths) != 2 || !strings.Contains(paths[0], "/movie/") {
		t.Errorf("应该先试 movie 再退回 tv: %v", paths)
	}
}

func TestCollectionReleaseDate(t *testing.T) {
	now := time.Now()
	// 「即将上映」给的是 11.10 这种没有年份的档期
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"start":0,"count":2,"total":16,"subject_collection_items":[
			{"id":36154923,"title":"爷爷奶奶那些事","type":"movie","year":"` +
			itoa(now.Year()) + `","release_date":"11.10","most_recent_release_date":"11月10日",
			 "cover":{"url":"https://img3.doubanio.com/d.jpg"},"card_subtitle":"2024 / 中国大陆 / 纪录片"},
			{"id":37191789,"title":"嫁金钗","type":"tv","year":"2026","episodes_info":"更新至28集",
			 "rating":{"value":7.0},"pic":{"large":"https://img9.doubanio.com/e.jpg"}}
		]}`))
	})

	coll, err := c.Collection(context.Background(), CollMovieSoon, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if coll.Total != 16 || len(coll.Items) != 2 {
		t.Fatalf("条目数不对: %+v", coll)
	}
	if coll.Items[0].ReleaseDate == "" || !strings.HasSuffix(coll.Items[0].ReleaseDate, "-11-10") {
		t.Errorf("11.10 应补全到具体年份: %q", coll.Items[0].ReleaseDate)
	}
	if coll.Items[1].EpisodesInfo != "更新至28集" || coll.Items[1].Rating != 7.0 {
		t.Errorf("剧集进度/评分没取到: %+v", coll.Items[1])
	}
	// 榜单里的剧集海报走 pic.large
	if coll.Items[1].Poster != "https://img9.doubanio.com/e.jpg" {
		t.Errorf("海报兜底不对: %q", coll.Items[1].Poster)
	}
}

// 限流必须报成「限流」而不是「解析失败」：用户要能看懂该怎么办。
func TestRateLimitError(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(418)
	})
	_, err := c.Collection(context.Background(), CollTVHot, 0, 5)
	if err == nil || !strings.Contains(err.Error(), "限流") {
		t.Fatalf("418 应该说清是限流: %v", err)
	}
}

func TestParseRelease(t *testing.T) {
	now := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	cases := map[string]string{
		"2023-04-22(中国大陆)": "2023-04-22",
		"2024-1-5":         "2024-01-05",
		"11.10":            "2026-11-10",
		"11月10日":           "2026-11-10",
		"01.03":            "2027-01-03", // 已经过去两个多月 → 当明年档期
		"":                 "",
		"待定":               "",
		"13.40":            "",
	}
	for in, want := range cases {
		if got := parseRelease(in, 0, now); got != want {
			t.Errorf("parseRelease(%q) = %q，期望 %q", in, got, want)
		}
	}
	// 榜单给了作品年份时优先它（跨年档期）
	if got := parseRelease("01.03", 2027, now); got != "2027-01-03" {
		t.Errorf("有作品年份时应该用它: %q", got)
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
