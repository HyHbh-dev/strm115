package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// discoverBody 是照真实响应裁过的：电影有 title/original_title/release_date，
// 剧集是 name/original_name/first_air_date —— 两套字段名不一致是 TMDB 的历史遗留。
const discoverBody = `{
  "page": 2,
  "total_pages": 500,
  "total_results": 10000,
  "results": [
    {
      "id": 687163,
      "title": "挽救计划",
      "original_title": "Project Hail Mary",
      "overview": "太阳正在被吃掉。",
      "poster_path": "/qf8HaEGNUFXU6nVAq9BVEk4Tiqb.jpg",
      "backdrop_path": "/8Tfys3mDZVp4tNoH2ktm06a0Tau.jpg",
      "vote_average": 8.631,
      "vote_count": 8708,
      "genre_ids": [878, 12],
      "release_date": "2026-03-15",
      "original_language": "en"
    },
    {
      "id": 999,
      "title": "没有海报的片",
      "original_title": "没有海报的片",
      "vote_average": 0,
      "vote_count": 0,
      "genre_ids": [],
      "release_date": ""
    }
  ]
}`

func newTestClient(t *testing.T, h http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("test-key")
	c.Base = srv.URL
	return c, srv
}

func TestDiscoverParsesChineseMeta(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		if r.URL.Path != "/discover/movie" {
			t.Errorf("路径不对: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(discoverBody))
	})

	pg, err := c.Discover(context.Background(), Query{
		Kind:      KindMovie,
		Providers: []int{8, 9},
		Genre:     878,
		Sort:      SortTop,
		Page:      2,
	})
	if err != nil {
		t.Fatalf("Discover 失败: %v", err)
	}

	if pg.Page != 2 || !pg.HasMore {
		t.Errorf("分页信息不对: page=%d hasMore=%v", pg.Page, pg.HasMore)
	}
	if len(pg.Items) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(pg.Items))
	}
	it := pg.Items[0]
	if it.Title != "挽救计划" || it.Original != "Project Hail Mary" {
		t.Errorf("标题解析不对: %+v", it)
	}
	if it.Year != 2026 {
		t.Errorf("年份应取自 release_date，实际 %d", it.Year)
	}
	if it.Rating != 8.6 {
		t.Errorf("评分应保留一位小数，实际 %v", it.Rating)
	}
	if !strings.HasPrefix(it.Poster, "https://image.tmdb.org/t/p/"+PosterSize+"/") {
		t.Errorf("海报应是完整地址，实际 %q", it.Poster)
	}
	if len(it.Genres) != 2 || it.Genres[0] != "科幻" || it.Genres[1] != "冒险" {
		t.Errorf("类型应翻成中文，实际 %v", it.Genres)
	}
	// 原名和中文名一样时不该留一份重复的
	if pg.Items[1].Original != "" {
		t.Errorf("原名等于标题时应清空，实际 %q", pg.Items[1].Original)
	}
	if pg.Items[1].Poster != "" {
		t.Errorf("没海报时应返回空串，实际 %q", pg.Items[1].Poster)
	}

	// 平台筛选没带 watch_region 会被 TMDB 静默忽略 —— 这是最难查的一类 bug。
	for _, want := range []string{
		"with_watch_providers=8%7C9", "watch_region=US", "with_genres=878",
		"sort_by=vote_average.desc", "vote_count.gte=300", "language=zh-CN", "page=2",
	} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("查询串缺 %q: %s", want, gotQuery)
		}
	}
}

func TestDiscoverSortNewUsesDateField(t *testing.T) {
	var gotQuery string
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"page":1,"total_pages":1,"results":[]}`))
	})
	if _, err := c.Discover(context.Background(), Query{Kind: KindTV, Sort: SortNew}); err != nil {
		t.Fatalf("Discover 失败: %v", err)
	}
	for _, want := range []string{"sort_by=first_air_date.desc", "first_air_date.lte="} {
		if !strings.Contains(gotQuery, want) {
			t.Errorf("剧集新片榜要按首播日期排，缺 %q: %s", want, gotQuery)
		}
	}
	// 没选平台时不该带 with_watch_providers，但可以带流媒体过滤
	if strings.Contains(gotQuery, "with_watch_providers") {
		t.Errorf("没选平台不该带上平台过滤: %s", gotQuery)
	}
}

func TestFillIMDbConcurrentAndCached(t *testing.T) {
	var calls int32
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		// /movie/7001/external_ids
		id := strings.TrimPrefix(r.URL.Path, "/movie/")
		id = strings.TrimSuffix(id, "/external_ids")
		n, _ := strconv.Atoi(id)
		_, _ = w.Write([]byte(`{"id":` + id + `,"imdb_id":"tt` + strconv.Itoa(70000000+n) + `"}`))
	})

	items := []Item{
		{ID: 7001, Kind: "movie"},
		{ID: 7002, Kind: "movie"},
		{ID: 7003, Kind: "movie"},
	}
	c.FillIMDb(context.Background(), items, 4)
	for _, it := range items {
		if it.IMDb == "" {
			t.Fatalf("id=%d 没补到 IMDb", it.ID)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("三条应各查一次，实际 %d 次", n)
	}

	// 第二轮同 ID 应直接吃缓存，不再打网络
	again := []Item{{ID: 7001, Kind: "movie"}}
	c.FillIMDb(context.Background(), again, 4)
	if again[0].IMDb != items[0].IMDb {
		t.Errorf("缓存没生效: %q vs %q", again[0].IMDb, items[0].IMDb)
	}
	if n := atomic.LoadInt32(&calls); n != 3 {
		t.Errorf("命中缓存后不该再请求，实际总共 %d 次", n)
	}
}

func TestNoKey(t *testing.T) {
	c := NewClient("")
	if _, err := c.Discover(context.Background(), Query{}); err != ErrNoKey {
		t.Errorf("没 Key 应返回 ErrNoKey，实际 %v", err)
	}
}

func TestAPIErrorSurfacesMessage(t *testing.T) {
	c, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status_code":7,"status_message":"Invalid API key: You must be granted a valid key."}`))
	})
	_, err := c.Discover(context.Background(), Query{})
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("应把 TMDB 的报错原文带出来，实际 %v", err)
	}
}

func TestGenreTables(t *testing.T) {
	// 剧集没有「恐怖」，电影有 —— 两张表不能混用
	if GenreID(KindMovie, "hrr") == 0 {
		t.Error("电影应有恐怖类型")
	}
	if GenreID(KindTV, "hrr") != 0 {
		t.Error("剧集没有恐怖类型，应返回 0")
	}
	// 同名 code 在两个 kind 下 ID 不同
	if GenreID(KindMovie, "scf") != 878 || GenreID(KindTV, "scf") != 10765 {
		t.Errorf("科幻 ID 不对: movie=%d tv=%d", GenreID(KindMovie, "scf"), GenreID(KindTV, "scf"))
	}
	if len(Genres(KindMovie)) < 10 || len(Genres(KindTV)) < 10 {
		t.Error("类型清单太短")
	}
}

func TestPageURL(t *testing.T) {
	if got := PageURL(KindMovie, 12); got != "https://www.themoviedb.org/movie/12" {
		t.Errorf("电影链接不对: %s", got)
	}
	if got := PageURL("series", 12); got != "https://www.themoviedb.org/tv/12" {
		t.Errorf("剧集链接不对: %s", got)
	}
}
