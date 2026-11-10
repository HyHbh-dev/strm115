package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeMetaServer 记下每次请求的路径与 query，再按路径返回固定响应。
type fakeMetaServer struct {
	mu   sync.Mutex
	seen []string
}

func (f *fakeMetaServer) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.URL.Path+"?"+r.URL.RawQuery)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		q := r.URL.Query()
		switch {
		case strings.HasPrefix(p, "/search/movie"):
			// 带年份故意返回空，验证「去掉年份再查一次」。
			if q.Get("year") != "" {
				w.Write([]byte(`{"results":[]}`))
				return
			}
			w.Write([]byte(`{"results":[{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","popularity":900.1,"vote_average":8.2,"poster_path":"/dune.jpg","backdrop_path":"/dune-bg.jpg","overview":"保罗的旅程"}]}`))
		case strings.HasPrefix(p, "/search/tv"):
			w.Write([]byte(`{"results":[{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","popularity":500,"vote_average":9.5,"poster_path":"/bb.jpg"}]}`))
		case strings.HasPrefix(p, "/find/"):
			w.Write([]byte(`{"movie_results":[{"id":693134}],"tv_results":[{"id":1396}]}`))
		case strings.HasPrefix(p, "/movie/"):
			w.Write([]byte(`{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","vote_average":8.2,"runtime":166,"overview":"保罗的旅程","poster_path":"/dune.jpg","backdrop_path":"/dune-bg.jpg","genres":[{"name":"科幻"},{"name":"冒险"}],"external_ids":{"imdb_id":"tt15239678"}}`))
		case strings.HasPrefix(p, "/tv/"):
			if strings.Contains(p, "/season/") {
				w.Write([]byte(`{"episodes":[{"episode_number":1,"name":"试播集","air_date":"2008-01-20","overview":"第一集"},{"episode_number":2,"name":"第二集","air_date":"2008-01-27"}]}`))
				return
			}
			w.Write([]byte(`{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","vote_average":9.5,"episode_run_time":[49],"overview":"化学老师","poster_path":"/bb.jpg","genres":[{"name":"剧情"}],"seasons":[{"season_number":0,"episode_count":5},{"season_number":1,"episode_count":7},{"season_number":2,"episode_count":13}],"external_ids":{"imdb_id":"tt0903747"}}`))
		default:
			http.NotFound(w, r)
		}
	}
}

func newMetaTestClient(t *testing.T) (*Client, *fakeMetaServer) {
	t.Helper()
	f := &fakeMetaServer{}
	srv := httptest.NewServer(f.handler())
	t.Cleanup(srv.Close)
	c := NewClient("test-key")
	c.Base = srv.URL
	return c, f
}

func TestSearchMetaRetriesWithoutYearAndCoversBothKinds(t *testing.T) {
	c, f := newMetaTestClient(t)
	hits, err := c.SearchMeta(context.Background(), "沙丘2", "2024", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 2 {
		t.Fatalf("期望电影+剧集两条，拿到 %d 条", len(hits))
	}
	var movie, tv *MetaHit
	for i := range hits {
		switch hits[i].Kind {
		case KindMovie:
			movie = &hits[i]
		case KindTV:
			tv = &hits[i]
		}
	}
	if movie == nil || movie.ID != 693134 || movie.Title != "沙丘2" || movie.Original != "Dune: Part Two" || movie.Year != "2024" {
		t.Fatalf("电影结果不对: %+v", movie)
	}
	if movie.Poster == "" || !strings.Contains(movie.Poster, "/w342/dune.jpg") {
		t.Fatalf("海报没按图片根拼: %q", movie.Poster)
	}
	if tv == nil || tv.ID != 1396 || tv.Title != "绝命毒师" || tv.Original != "Breaking Bad" {
		t.Fatalf("剧集结果不对: %+v", tv)
	}

	yearQueries := 0
	bareQueries := 0
	f.mu.Lock()
	for _, s := range f.seen {
		if strings.Contains(s, "/search/movie?") {
			if strings.Contains(s, "year=2024") {
				yearQueries++
			} else {
				bareQueries++
			}
		}
	}
	f.mu.Unlock()
	if yearQueries != 1 || bareQueries != 1 {
		t.Fatalf("带年份查空后应重试一次无年份：year=%d bare=%d seen=%v", yearQueries, bareQueries, f.seen)
	}
}

func TestMetaByIDMovieParsesAllFields(t *testing.T) {
	c, f := newMetaTestClient(t)
	d, err := c.MetaByID(context.Background(), KindMovie, 693134)
	if err != nil {
		t.Fatal(err)
	}
	if d.IMDbID != "tt15239678" || d.Year != "2024" || d.Rating != "8.2" || d.Runtime != "166 min" {
		t.Fatalf("字段不全: %+v", d)
	}
	if d.Title != "沙丘2" || d.Original != "Dune: Part Two" {
		t.Fatalf("中文名/原名错位: %+v", d)
	}
	if len(d.Genres) != 2 || d.Genres[0] != "科幻" || d.Overview != "保罗的旅程" {
		t.Fatalf("类型/简介不对: %+v", d)
	}
	if d.Poster == "" || d.Backdrop == "" {
		t.Fatal("海报/背景图缺失")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	joined := strings.Join(f.seen, "|")
	if !strings.Contains(joined, "append_to_response=external_ids") {
		t.Fatalf("详情没带 external_ids，会多打一次接口: %v", f.seen)
	}
	if !strings.Contains(joined, "language=zh-CN") {
		t.Fatalf("详情没要中文: %v", f.seen)
	}
}

func TestMetaByIDTVFiltersSpecialsSeason(t *testing.T) {
	c, _ := newMetaTestClient(t)
	d, err := c.MetaByID(context.Background(), KindTV, 1396)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Seasons) != 2 || d.Seasons[0] != 1 || d.Seasons[1] != 2 {
		t.Fatalf("第 0 季应被过滤: %+v", d.Seasons)
	}
	// 每季集数来自同一次 /tv/{id} 响应。详情抽屉的起播线滑杆靠它把
	// 「全剧第 N 集」换算成季/集；特别篇（第 0 季）的 5 集不能算进正片。
	if len(d.SeasonEpisodes) != 2 ||
		d.SeasonEpisodes[0] != (SeasonCount{Season: 1, Episodes: 7}) ||
		d.SeasonEpisodes[1] != (SeasonCount{Season: 2, Episodes: 13}) {
		t.Fatalf("分季集数不对: %+v", d.SeasonEpisodes)
	}
	if d.Runtime != "49 min" {
		t.Fatalf("剧集时长应取 episode_run_time: %q", d.Runtime)
	}
	if d.IMDbID != "tt0903747" {
		t.Fatalf("IMDb ID 丢失: %+v", d)
	}
}

func TestMetaByIMDbIDPrefersWantedKind(t *testing.T) {
	c, f := newMetaTestClient(t)
	// 文件名认成剧集 → 应该用 tv_results 的 1396，而不是 movie_results 的 693134。
	d, err := c.MetaByIMDbID(context.Background(), "tt0903747", "series")
	if err != nil {
		t.Fatal(err)
	}
	if d.Kind != KindTV || d.ID != 1396 || d.IMDbID != "tt0903747" {
		t.Fatalf("类型优先级不对: %+v", d)
	}
	d2, err := c.MetaByIMDbID(context.Background(), "tt15239678", KindMovie)
	if err != nil {
		t.Fatal(err)
	}
	if d2.Kind != KindMovie || d2.ID != 693134 {
		t.Fatalf("电影优先级不对: %+v", d2)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.seen) == 0 || !strings.HasPrefix(f.seen[0], "/find/") {
		t.Fatalf("没有先走 /find: %v", f.seen)
	}
}

func TestSeasonEpisodesMetaCollectsNeededSeasons(t *testing.T) {	c, f := newMetaTestClient(t)
	eps, err := c.SeasonEpisodesMeta(context.Background(), 1396, []int{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 4 {
		t.Fatalf("两季各 2 集应为 4 条，拿到 %d", len(eps))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	calls := 0
	for _, s := range f.seen {
		if strings.Contains(s, "/season/") {
			calls++
		}
	}
	if calls != 2 {
		t.Fatalf("第 0 季不该发请求，期望 2 次拿到 %d 次", calls)
	}
}

func TestSplitBilingual(t *testing.T) {
	for _, c := range []struct{ in, zh, en string }{
		{"极速：伪装者 Drive The Pretenders", "极速：伪装者", "Drive The Pretenders"},
		{"碟中谍8：最终清算", "碟中谍8：最终清算", ""},
		{"Dune Part Two", "", "Dune Part Two"},
		{"赛博朋克：边缘行者 Cyberpunk Edgerunners 2022", "赛博朋克：边缘行者", "Cyberpunk Edgerunners 2022"},
		{"2160p", "", ""},
	} {
		zh, en := splitBilingual(c.in)
		if zh != c.zh || en != c.en {
			t.Errorf("splitBilingual(%q) = (%q, %q)，期望 (%q, %q)", c.in, zh, en, c.zh, c.en)
		}
	}
}

// TestSearchMetaSplitsBilingualQuery 混排片名整串搜不到时，必须拆开再搜。
//
// 真实案例：《极速：伪装者 Drive The Pretenders》——TMDB 的中文名是
// 《假面恋人：换档追爱》，英文原名 Drive – The Pretenders。不拆开的话
// 整理前的校验永远判定「TMDB 里没找到」，片子就一直躺在暂存目录。
func TestSearchMetaSplitsBilingualQuery(t *testing.T) {
	var (
		mu      sync.Mutex
		queries []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("query")
		mu.Lock()
		queries = append(queries, q)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if q == "Drive The Pretenders" {
			_, _ = w.Write([]byte(`{"results":[{"id":1101412,"title":"假面恋人：换档追爱","original_title":"Drive – The Pretenders","release_date":"2026-10-27"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	c := NewClient("test-key")
	c.Base = srv.URL
	hits, err := c.SearchMetaLang(context.Background(), "极速：伪装者 Drive The Pretenders", "2026", KindMovie, DefaultLanguage)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != 1101412 || !strings.Contains(hits[0].Original, "Drive") {
		t.Fatalf("拆开后应搜到英文原名那条，实际 %+v", hits)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(queries) < 2 || queries[len(queries)-1] != "Drive The Pretenders" {
		t.Fatalf("应该在中英两段里搜到英文段，实际请求 %v", queries)
	}
}
