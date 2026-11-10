package httpapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// fakeCatalogTMDB 是海报墙用的假 TMDB 上游：详情 / 反查 / 按片名搜。
// 所有断言都走真 HTTP，顺带把「上游根改了下一次请求就生效」验了。
//
// 上游是进程级配置，Server.New() 会按 settings 重置成官方地址；
// 所以必须像真实设置页那样先写 tmdb_base 再 applyTMDBUpstream，
// 否则请求会打到真 TMDB，测试变成依赖外网。
func fakeCatalogTMDB(t *testing.T, s *Server, st *store.Store, base string) *httptest.Server {
	t.Helper()
	oldAPI, oldImage := tmdb.CurrentUpstream()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		p := r.URL.Path
		switch {
		case p == "/movie/693134":
			_, _ = io.WriteString(w, `{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","vote_average":8.2,"runtime":166,"overview":"保罗的旅程","poster_path":"/dune.jpg","backdrop_path":"/dune-bg.jpg","genres":[{"name":"科幻"}],"external_ids":{"imdb_id":"tt15239678"}}`)
		case p == "/tv/1396":
			_, _ = io.WriteString(w, `{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","vote_average":9.5,"episode_run_time":[49],"overview":"化学老师的转变","poster_path":"/bb.jpg","genres":[{"name":"剧情"}],"seasons":[{"season_number":1}],"external_ids":{"imdb_id":"tt0903747"}}`)
		case p == "/tv/1396/season/1":
			// 订阅弹窗点某一集时的单集详情也走这个假上游。
			_, _ = io.WriteString(w, `{"episodes":[{"episode_number":1,"name":"试播集","air_date":"2008-01-20","still_path":"/bb-still.jpg","overview":"一个化学老师走上了不归路。","runtime":47,"vote_average":8.4}]}`)
		case p == "/find/tt15239678":
			_, _ = io.WriteString(w, `{"movie_results":[{"id":693134}],"tv_results":[]}`)
		case p == "/search/tv":
			_, _ = io.WriteString(w, `{"results":[{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","vote_average":9.5,"poster_path":"/bb.jpg"}]}`)
		case p == "/search/movie":
			_, _ = io.WriteString(w, `{"results":[{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","vote_average":8.2,"poster_path":"/dune.jpg"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	ctx := context.Background()
	root := base
	if root == "" {
		root = srv.URL
	}
	if err := st.SetSetting(ctx, "tmdb_base", root); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "tmdb_image_base", root); err != nil {
		t.Fatal(err)
	}
	s.applyTMDBUpstream(ctx)
	t.Cleanup(func() {
		tmdb.SetUpstream(oldAPI, oldImage)
		srv.Close()
	})
	return srv
}

type catalogListResp struct {
	Source       string        `json:"source"`
	Total        int           `json:"total"`
	Items        []catalogCard `json:"items"`
	TMDbMissing  int           `json:"tmdb_missing"`
	EmbyChecked  bool          `json:"emby_checked"`
	Hint         string        `json:"hint"`
	ProviderNote string        `json:"provider_note"`
}

func getCatalog(t *testing.T, h http.Handler, cookies []*http.Cookie, query string) catalogListResp {
	t.Helper()
	rec, _ := doJSON(t, h, http.MethodGet, "/api/catalog"+query, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out catalogListResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestCatalogDefaultsToTMDB 不传 source 时默认走 TMDB，且按台账的 tmdb_id 直查详情。
func TestCatalogDefaultsToTMDB(t *testing.T) {
	s, st := newServerWithStore(t)
	srv := fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)

	rows := []store.Media{
		{LibraryID: libID, RelPath: "电影/沙丘2/Dune.mkv", Kind: "movie",
			Title: "Dune Part Two", MetaTitle: "本地旧名字", Year: "2024",
			IMDbID: "tt15239678", TMDbID: 693134, Rating: "1.0",
			NFOFile: "电影/沙丘2/movie.nfo", Status: "ok"},
		{LibraryID: libID, RelPath: "剧集/绝命毒师/Season 1/E01.mkv", Kind: "series",
			Title: "Breaking Bad", MetaTitle: "本地剧名", Year: "2008",
			IMDbID: "tt0903747", TMDbID: 1396, Season: 1, Episode: 1, Status: "ok"},
	}
	for _, m := range rows {
		if err := st.UpsertMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	out := getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID))
	if out.Source != "tmdb" {
		t.Fatalf("默认数据源应是 tmdb，实际 %s", out.Source)
	}
	if out.TMDbMissing != 0 || out.EmbyChecked {
		t.Fatalf("TMDB 全部命中且不该查 Emby: missing=%d emby_checked=%v hint=%s",
			out.TMDbMissing, out.EmbyChecked, out.Hint)
	}
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("应有 2 张卡，实际 total=%d items=%d", out.Total, len(out.Items))
	}
	byType := map[string]catalogCard{}
	for _, c := range out.Items {
		byType[c.Type] = c
	}
	movie, series := byType["movie"], byType["series"]
	if movie.Name != "沙丘2" || movie.Original != "Dune: Part Two" || movie.Rating != 8.2 {
		t.Errorf("电影卡片没换成 TMDB 元数据: %+v", movie)
	}
	if series.Name != "绝命毒师" || series.Rating != 9.5 {
		t.Errorf("剧集卡片没换成 TMDB 元数据: %+v", series)
	}
	for _, c := range out.Items {
		if c.TMDBID == "" || !strings.HasPrefix(c.Poster, srv.URL) {
			t.Errorf("TMDB ID / 海报地址不对: %+v", c)
		}
		if c.MediaID == 0 || !strings.HasPrefix(c.Key, "local:") {
			t.Errorf("本地台账状态被覆盖: %+v", c)
		}
	}
	if !movie.NFO {
		t.Error("本地 NFO 标记被 TMDB 富集抹掉了")
	}
	if !strings.Contains(out.ProviderNote, "TMDB") {
		t.Errorf("来源说明没跟上: %s", out.ProviderNote)
	}

	// 详情抽屉也要是同一份 TMDB 数据。
	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog/item?source=tmdb&key="+movie.Key, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var det struct {
		Item catalogCard `json:"item"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &det); err != nil {
		t.Fatal(err)
	}
	if det.Item.Name != "沙丘2" || det.Item.TMDBID != "693134" {
		t.Fatalf("详情没按 TMDB 富集: %+v", det.Item)
	}
}

// TestCatalogTMDBResolvesLegacyRows 老台账没有 tmdb_id 时，走 IMDb /find 与片名搜索。
func TestCatalogTMDBResolvesLegacyRows(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)

	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/沙丘2/Dune.mkv", Kind: "movie",
		Title: "Dune Part Two", MetaTitle: "沙丘2", Year: "2024",
		IMDbID: "tt15239678", Status: "ok",
	})
	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "剧集/绝命毒师/Season 1/E01.mkv", Kind: "series",
		Title: "绝命毒师", MetaTitle: "绝命毒师", Year: "2008", Season: 1, Episode: 1,
		Status: "ok",
	})

	out := getCatalog(t, s.Handler(), cookies, "?source=tmdb&library_id="+itoa(libID))
	if out.TMDbMissing != 0 || len(out.Items) != 2 {
		t.Fatalf("两条都应被解析: missing=%d items=%d hint=%s", out.TMDbMissing, len(out.Items), out.Hint)
	}
	got := map[string]string{}
	for _, c := range out.Items {
		got[c.Type] = c.TMDBID
	}
	if got["movie"] != "693134" || got["series"] != "1396" {
		t.Fatalf("反查结果不对: %v", got)
	}
}

// TestCatalogTMDBFailureKeepsLocalCards TMDB 挂掉时整面墙仍要用本地数据出齐。
func TestCatalogTMDBFailureKeepsLocalCards(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "http://127.0.0.1:9")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)
	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/空枪/空枪.mkv", Kind: "movie",
		Title: "空枪", MetaTitle: "空枪", Year: "2026", TMDbID: 999999, Status: "ok",
	})

	out := getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID))
	if out.Source != "tmdb" || out.Total != 1 || len(out.Items) != 1 {
		t.Fatalf("墙不该变空: source=%s total=%d items=%d", out.Source, out.Total, len(out.Items))
	}
	if out.Items[0].Name != "空枪" || out.Items[0].TMDBID != "999999" {
		t.Errorf("本地字段被清空: %+v", out.Items[0])
	}
	if out.TMDbMissing != 1 || !strings.Contains(out.Hint, "保留本地台账") {
		t.Errorf("失败要如实提示: missing=%d hint=%s", out.TMDbMissing, out.Hint)
	}
}

// TestCatalogSourcesAreSwitchable Emby / 本地台账 / 非法值回默认，三条路径都要在。
func TestCatalogSourcesAreSwitchable(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)
	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/沙丘2/Dune.mkv", Kind: "movie",
		Title: "沙丘2", MetaTitle: "沙丘2", TMDbID: 693134, Status: "ok",
	})

	for _, tc := range []struct{ query, want string }{
		{"?source=local&library_id=" + itoa(libID), "local"},
		{"?source=TMDB&library_id=" + itoa(libID), "tmdb"},
		{"?source=bogus&library_id=" + itoa(libID), "tmdb"},
		{"?library_id=" + itoa(libID), "tmdb"},
	} {
		out := getCatalog(t, s.Handler(), cookies, tc.query)
		if out.Source != tc.want {
			t.Errorf("%s 应返回 %s，实际 %s", tc.query, tc.want, out.Source)
		}
		if len(out.Items) != 1 {
			t.Errorf("%s 应保留 1 张卡，实际 %d", tc.query, len(out.Items))
		}
	}
}

// TestCatalogUpdateByTMDbID 海报墙编辑弹窗改 TMDB ID 时，后端要按新 ID 重刮
// 并回写台账；这是「认错片」唯一的自助修正入口。
func TestCatalogUpdateByTMDbID(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, root, _ := seedCatalogLib(t, st)

	rel := "电影/沙丘2/Dune.mkv"
	if err := st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: rel, Kind: "movie", Title: "认错的片",
		MetaTitle: "认错的片", TMDbID: 111, Status: "unmatched",
	}); err != nil {
		t.Fatal(err)
	}
	m, ok, err := st.GetMedia(ctx, libID, rel)
	if err != nil || !ok {
		t.Fatalf("读台账失败: ok=%v err=%v", ok, err)
	}
	if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash("电影/沙丘2")), 0o755); err != nil {
		t.Fatal(err)
	}

	rec, _ := doJSON(t, s.Handler(), http.MethodPost, "/api/catalog/item", map[string]any{
		"key": localKey(m.ID), "tmdb_id": 693134,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("按 TMDB ID 重刮应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Item  catalogCard    `json:"item"`
		Local map[string]any `json:"local"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Local["tmdb_id"] != "693134" || out.Item.TMDBID != "693134" {
		t.Fatalf("新 TMDB ID 没落库: local=%v item=%+v", out.Local, out.Item)
	}
	if out.Item.Name != "沙丘2" || out.Item.IMDbID != "tt15239678" {
		t.Fatalf("没换成 TMDB 的元数据: %+v", out.Item)
	}
	got, _, _ := st.GetMedia(ctx, libID, rel)
	if got.TMDbID != 693134 || got.IMDbID != "tt15239678" {
		t.Fatalf("台账没写回: %+v", got)
	}
}

// TestCatalogLocalPosterSkipsTMDB 有图的卡片不联网。
//
// 这条不是「省流量」，是首屏时间：TMDB 走用户自己的反代，单次 1.6–2.6 秒，
// 一页六张卡（4 并发）就是两三秒。刮削早就把海报落库了，列表页再问一遍没有意义。
func TestCatalogLocalPosterSkipsTMDB(t *testing.T) {
	s, st := newServerWithStore(t)
	oldAPI, oldImage := tmdb.CurrentUpstream()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1) // 只数次数，不求内容
		http.NotFound(w, r)
	}))
	ctx := context.Background()
	defer func() {
		tmdb.SetUpstream(oldAPI, oldImage)
		srv.Close()
	}()
	if err := st.SetSetting(ctx, "tmdb_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting(ctx, "tmdb_image_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	s.applyTMDBUpstream(ctx)

	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)
	rows := []store.Media{
		{LibraryID: libID, RelPath: "电影/沙丘2/Dune.mkv", Kind: "movie",
			Title: "Dune Part Two", MetaTitle: "沙丘2", Year: "2024",
			TMDbID: 693134, Rating: "8.2", Overview: "保罗的旅程",
			PosterFile: "电影/沙丘2/poster.jpg", BackdropFile: "电影/沙丘2/fanart.jpg",
			Status: "ok"},
		{LibraryID: libID, RelPath: "电影/星际穿越/Interstellar.mkv", Kind: "movie",
			Title: "Interstellar", MetaTitle: "星际穿越", Year: "2014",
			TMDbID: 157336, Status: "ok"},
		// 第二部没有磁盘海报，只有刮削时存下的 URL —— 也不该联网
		{LibraryID: libID, RelPath: "电影/奥本海默/Oppenheimer.mkv", Kind: "movie",
			Title: "Oppenheimer", MetaTitle: "奥本海默", Year: "2023",
			TMDbID: 872585, PosterURL: "https://img.example/p.jpg", Status: "ok"},
	}
	// 上面第二部改一下：给它一条 poster_url，验证「有 URL 也不算缺图」
	rows[1].PosterURL = "https://img.example/interstellar.jpg"
	for _, m := range rows {
		if err := st.UpsertMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	out := getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID))
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("卡片都有图时不该问 TMDB，实际问了 %d 次", n)
	}
	if out.TMDbMissing != 0 {
		t.Fatalf("不该有「缺元数据」的卡片: %d", out.TMDbMissing)
	}
	if out.Total != 3 {
		t.Fatalf("应有 3 张卡，实际 %d", out.Total)
	}
	byName := map[string]catalogCard{}
	for _, c := range out.Items {
		byName[c.Name] = c
	}
	// 有磁盘海报：走本地图片接口，标题保持台账里的中文名（没被 TMDB 覆盖）
	dune := byName["沙丘2"]
	if !strings.HasPrefix(dune.Poster, "/api/media/") {
		t.Errorf("磁盘海报应走本地图片接口，实际 %q", dune.Poster)
	}
	if dune.Rating != 8.2 || dune.Overview != "保罗的旅程" {
		t.Errorf("台账里的元数据丢了: %+v", dune)
	}
	// 只有 URL 的：直接用存下来的地址，不联网也不丢图
	if got := byName["星际穿越"].Poster; got != "https://img.example/interstellar.jpg" {
		t.Errorf("有 poster_url 时应直接用它，实际 %q", got)
	}
	if got := byName["奥本海默"].Poster; got != "https://img.example/p.jpg" {
		t.Errorf("有 poster_url 时应直接用它，实际 %q", got)
	}

	// refresh=1 是唯一的强制联网入口（设置页改了反代之后用）
	before := atomic.LoadInt32(&hits)
	getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID)+"&refresh=1")
	if atomic.LoadInt32(&hits) <= before {
		t.Fatalf("refresh=1 应强制重查 TMDB，实际没发请求")
	}
}
