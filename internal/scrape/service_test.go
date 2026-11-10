package scrape

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// fakeTMDB 是一台假的 TMDB 上游：搜索 / 反查 / 详情 / 演员 / 分季 / 图片
// 全在同一个 httptest 服务里，并按端点计数。
//
// 纯 TMDB 之后刮削链路只有这一个上游，测试不再 mock 函数变量，
// 而是真打 HTTP —— 顺带把 URL 拼法、append_to_response、图片根一起验了。
type fakeTMDB struct {
	mu                                                 sync.Mutex
	searches, finds, details, credits, seasons, images int32
}

func (f *fakeTMDB) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	tmbdOldAPI, tmbdOldImage := tmdb.CurrentUpstream()
	tmdb.SetUpstream(srv.URL, srv.URL)
	t.Cleanup(func() {
		tmdb.SetUpstream(tmbdOldAPI, tmbdOldImage)
		srv.Close()
	})
	return srv.URL
}

func (f *fakeTMDB) counts() (searches, finds, details, credits, seasons int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.searches, f.finds, f.details, f.credits, f.seasons
}

func (f *fakeTMDB) serve(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	if os.Getenv("FAKE_TMDB_DEBUG") != "" {
		println("FAKE_TMDB_PATH", p)
	}
	// 图片：海报 / 背景 / 演员头像都从图片根走，给一张够大的假 JPEG。
	for _, size := range []string{"/w342/", "/w1280/", "/w185/"} {
		if strings.HasPrefix(p, size) {
			atomic.AddInt32(&f.images, 1)
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, make([]byte, 2048)...))
			return
		}
	}

	q := r.URL.Query().Get("query")
	switch {
	case strings.HasPrefix(p, "/search/movie"):
		atomic.AddInt32(&f.searches, 1)
		if strings.Contains(strings.ToLower(q), "dune") {
			writeJSONFake(w, `{"results":[{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","vote_average":8.2,"poster_path":"/dune.jpg","backdrop_path":"/dune-bg.jpg","overview":"保罗的旅程"}]}`)
		} else {
			writeJSONFake(w, `{"results":[]}`)
		}
		return
	case strings.HasPrefix(p, "/search/tv"):
		atomic.AddInt32(&f.searches, 1)
		switch {
		case strings.Contains(strings.ToLower(q), "breaking"):
			writeJSONFake(w, `{"results":[{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","vote_average":9.5,"poster_path":"/bb.jpg"}]}`)
		case strings.Contains(strings.ToLower(q), "mortal"):
			// 只有英文发布名的国创：zh-CN 下 TMDB 只回中文名，en-US 下才是英文名。
			// 用来验证刮削的「第二语言兜底」。
			if r.URL.Query().Get("language") == "en-US" {
				writeJSONFake(w, `{"results":[{"id":106449,"name":"A Record of a Mortal's Journey to Immortality","original_name":"凡人修仙传","first_air_date":"2020-07-25","vote_average":8.4,"poster_path":"/mr.jpg"}]}`)
			} else {
				writeJSONFake(w, `{"results":[{"id":106449,"name":"凡人修仙传","original_name":"凡人修仙传","first_air_date":"2020-07-25","vote_average":8.4,"poster_path":"/mr.jpg"}]}`)
			}
		default:
			writeJSONFake(w, `{"results":[]}`)
		}
		return
	case strings.HasPrefix(p, "/find/"):
		atomic.AddInt32(&f.finds, 1)
		switch p {
		case "/find/tt15239678":
			writeJSONFake(w, `{"movie_results":[{"id":693134}],"tv_results":[]}`)
		case "/find/tt0903747":
			writeJSONFake(w, `{"movie_results":[],"tv_results":[{"id":1396}]}`)
		default:
			writeJSONFake(w, `{"movie_results":[],"tv_results":[]}`)
		}
		return
	// 电影走 /movie/<id>/credits，剧集走 /tv/<id>/aggregate_credits；
	// 用 "credits" 而不是 "/credits" 才能同时认下这两种路径。
	case strings.Contains(p, "credits"):
		atomic.AddInt32(&f.credits, 1)
		if strings.HasPrefix(p, "/tv/") {
			writeJSONFake(w, `{"cast":[{"id":2,"name":"布莱恩·科兰斯顿","roles":[{"character":"Walter White"}],"profile_path":"/bc.jpg"}],"crew":[{"name":"文斯·吉里根","jobs":[{"job":"Creator"}]}]}`)
		} else {
			writeJSONFake(w, `{"cast":[{"id":1,"name":"赞达亚","character":"Chani","profile_path":"/zen.jpg"}],"crew":[{"name":"丹尼斯·维伦纽瓦","job":"Director"}]}`)
		}
		return
	case strings.Contains(p, "/season/"):
		atomic.AddInt32(&f.seasons, 1)
		writeJSONFake(w, `{"episodes":[
			{"episode_number":1,"name":"Pilot","air_date":"2008-01-20","overview":"首集简介"},
			{"episode_number":2,"name":"Cat's in the Bag...","air_date":"2008-01-27","overview":"第二集简介"},
			{"episode_number":3,"name":"...And the Bag's in the River","air_date":"2008-02-10"}]}`)
		return
	case strings.HasPrefix(p, "/movie/"):
		atomic.AddInt32(&f.details, 1)
		writeJSONFake(w, `{"id":693134,"title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27","vote_average":8.2,"runtime":166,"overview":"保罗的旅程","poster_path":"/dune.jpg","backdrop_path":"/dune-bg.jpg","genres":[{"name":"科幻"}],"external_ids":{"imdb_id":"tt15239678"}}`)
		return
	case p == "/tv/106449":
		atomic.AddInt32(&f.details, 1)
		writeJSONFake(w, `{"id":106449,"name":"凡人修仙传","original_name":"凡人修仙传","first_air_date":"2020-07-25","vote_average":8.4,"episode_run_time":[20],"overview":"修仙","poster_path":"/mr.jpg","genres":[{"name":"动画"}],"seasons":[{"season_number":1}],"external_ids":{"imdb_id":"tt12879782"}}`)
		return
	case strings.HasPrefix(p, "/tv/"):
		atomic.AddInt32(&f.details, 1)
		writeJSONFake(w, `{"id":1396,"name":"绝命毒师","original_name":"Breaking Bad","first_air_date":"2008-01-20","vote_average":9.5,"episode_run_time":[49],"overview":"化学老师的转变","poster_path":"/bb.jpg","backdrop_path":"/bb-bg.jpg","genres":[{"name":"剧情"}],"seasons":[{"season_number":0},{"season_number":1}],"external_ids":{"imdb_id":"tt0903747"}}`)
		return
	default:
		http.NotFound(w, r)
	}
}

func writeJSONFake(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func newTestService(t *testing.T) (*Service, *store.Store, store.Library, string) {
	t.Helper()

	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	ctx := context.Background()
	acct, err := st.CreateAccount(ctx, "115", "test", "{}")
	if err != nil {
		t.Fatal(err)
	}
	mediaRoot := filepath.Join(dir, "media")
	if err := os.MkdirAll(mediaRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "测试库", RemoteCID: "0",
		LocalPath: mediaRoot, CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := st.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}

	// 给一把假 Key：源码里不再内置 Key，端到端测试也不该依赖部署环境变量。
	// fakeTMDB 是本地假上游，不校验 api_key，用占位串就够。
	svc := New(st, func(context.Context) string { return "test-key" },
		func() string { return mediaRoot },
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	return svc, st, lib, mediaRoot
}

// TestScrapeEndToEnd 一条龙：解析 → 查 TMDB（每部只查一次）→ 写 nfo + 图片 + 库。
func TestScrapeEndToEnd(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, mediaRoot := newTestService(t)
	ctx := context.Background()

	// 一季 3 集 + 一部电影
	files := []store.File{
		{LibraryID: lib.ID, FileID: "1", PickCode: "a", Name: "Breaking.Bad.S01E01.mkv",
			RelPath: "剧集/Breaking Bad/Season 01/Breaking.Bad.S01E01.mkv", Size: 1 << 30},
		{LibraryID: lib.ID, FileID: "2", PickCode: "b", Name: "Breaking.Bad.S01E02.mkv",
			RelPath: "剧集/Breaking Bad/Season 01/Breaking.Bad.S01E02.mkv", Size: 1 << 30},
		{LibraryID: lib.ID, FileID: "3", PickCode: "c", Name: "Breaking.Bad.S01E03.mkv",
			RelPath: "剧集/Breaking Bad/Season 01/Breaking.Bad.S01E03.mkv", Size: 1 << 30},
		{LibraryID: lib.ID, FileID: "4", PickCode: "d", Name: "Dune.Part.Two.2024.mkv",
			RelPath: "电影/Dune.Part.Two.2024.2160p.mkv", Size: 20 << 30},
		// 太小的文件应当被 MinSize 过滤掉
		{LibraryID: lib.ID, FileID: "5", PickCode: "e", Name: "sample.mkv",
			RelPath: "电影/sample.mkv", Size: 1 << 20},
	}
	if err := st.UpsertFiles(ctx, lib.AccountID, files); err != nil {
		t.Fatal(err)
	}

	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	job, err := svc.Start(lib, opt)
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()

	p := job.Snapshot()
	if p.Phase != "done" {
		t.Fatalf("任务没跑完: phase=%s err=%s", p.Phase, p.Err)
	}
	if p.Total != 4 {
		t.Fatalf("应处理 4 个视频（sample 被体积过滤），实际 %d", p.Total)
	}
	if p.Matched != 4 || p.Unmatched != 0 {
		t.Fatalf("匹配数不对: matched=%d unmatched=%d", p.Matched, p.Unmatched)
	}

	// 关键：3 集共用一次查询，加上电影共 2 次搜索 / 2 次详情 / 1 次分季。
	searches, finds, details, _, seasons := fake.counts()
	if searches != 2 {
		t.Fatalf("TMDB 搜索被请求 %d 次，期望 2 次（同剧集必须共用一次查询）", searches)
	}
	if finds != 0 {
		t.Fatalf("按片名搜时不该走 /find，实际 %d 次", finds)
	}
	if details != 2 || seasons != 1 {
		t.Fatalf("详情/分季请求数不对: details=%d seasons=%d", details, seasons)
	}

	for _, rel := range []string{
		"剧集/Breaking Bad/Season 01/Breaking.Bad.S01E01.nfo",
		"剧集/Breaking Bad/Season 01/Breaking.Bad.S01E02.nfo",
		"电影/Dune.Part.Two.2024.2160p.nfo",
		"剧集/Breaking Bad/poster.jpg", // 剧集海报写在剧集根目录，只写一份
		"电影/Dune.Part.Two.2024.2160p-poster.jpg",
	} {
		if _, err := os.Stat(filepath.Join(mediaRoot, filepath.FromSlash(rel))); err != nil {
			t.Errorf("缺少文件 %s: %v", rel, err)
		}
	}
	if _, err := os.Stat(filepath.Join(mediaRoot, "剧集/Breaking Bad/Season 01/poster.jpg")); err == nil {
		t.Error("剧集海报被逐集复制了，应只写在剧集根目录")
	}

	// 每集的 nfo 必须写「这一集」的名字，不是整季共用一个剧名。
	for ep, want := range map[string]string{
		"S01E01": "<title>Pilot</title>",
		"S01E02": "<title>Cat&#39;s in the Bag...</title>",
	} {
		raw, err := os.ReadFile(filepath.Join(mediaRoot, "剧集/Breaking Bad/Season 01", "Breaking.Bad."+ep+".nfo"))
		if err != nil {
			t.Fatalf("读 %s 的 nfo 失败: %v", ep, err)
		}
		if !strings.Contains(string(raw), want) {
			t.Errorf("%s 的 nfo 没写成单集标题 %s，实际:\n%s", ep, want, raw)
		}
	}

	// 库里的记录：TMDB ID 必须落库，海报墙要靠它直查详情。
	all, err := st.ListMediaAll(ctx, lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Fatalf("库里有 %d 条，期望 4", len(all))
	}
	for _, m := range all {
		if m.Status != "ok" {
			t.Errorf("%s 状态 %s（%s）", m.RelPath, m.Status, m.Message)
		}
		if m.IMDbID == "" {
			t.Errorf("%s 没有 imdb_id（TMDB external_ids 应带回来）", m.RelPath)
		}
		if m.TMDbID <= 0 {
			t.Errorf("%s 没有 tmdb_id，海报墙没法按 TMDB 直查", m.RelPath)
		}
	}

	// 第二次刮削必须全部命中缓存，一次网络都不发。
	beforeSearches, beforeFinds, beforeDetails, _, beforeSeasons := fake.counts()
	opt2 := DefaultOptions()
	opt2.Rate = 0
	opt2.Translate = false
	job2, err := svc.Start(lib, opt2)
	if err != nil {
		t.Fatal(err)
	}
	<-job2.Done()
	if job2.Snapshot().Cached != 2 {
		t.Errorf("第二次刮削应命中 2 条缓存，实际 %d", job2.Snapshot().Cached)
	}
	afterSearches, afterFinds, afterDetails, _, afterSeasons := fake.counts()
	if afterSearches != beforeSearches || afterFinds != beforeFinds ||
		afterDetails != beforeDetails || afterSeasons != beforeSeasons {
		t.Errorf("第二次刮削又请求了 TMDB：search %d→%d find %d→%d detail %d→%d season %d→%d",
			beforeSearches, afterSearches, beforeFinds, afterFinds,
			beforeDetails, afterDetails, beforeSeasons, afterSeasons)
	}
}

// TestScrapeUnmatchedIsExplicit 认不出来的文件名要明确落成 unmatched，不能瞎猜。
func TestScrapeUnmatchedIsExplicit(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, _ := newTestService(t)
	ctx := context.Background()
	if err := st.UpsertFiles(ctx, lib.AccountID, []store.File{
		{LibraryID: lib.ID, FileID: "1", PickCode: "a", Name: "movie.mkv",
			RelPath: "电影/movie.mkv", Size: 2 << 30},
	}); err != nil {
		t.Fatal(err)
	}

	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	job, err := svc.Start(lib, opt)
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	p := job.Snapshot()
	if p.Unmatched != 1 {
		t.Fatalf("应记为未匹配，实际 unmatched=%d matched=%d", p.Unmatched, p.Matched)
	}
	all, _ := st.ListMediaAll(ctx, lib.ID)
	if len(all) != 1 || all[0].Status != "unmatched" {
		t.Fatalf("库里应有一条 unmatched，实际 %+v", all)
	}
	if all[0].IMDbID != "" || all[0].TMDbID != 0 {
		t.Error("未匹配的条目不该有 ID")
	}
}

// TestScrapeMetaCacheSurvivesAcrossLibraries 缓存按片名维度，不绑库。
func TestScrapeMetaCacheSurvivesAcrossLibraries(t *testing.T) {
	svc, st, lib, _ := newTestService(t)
	ctx := context.Background()
	// 带 TMDbID 才是新链路写的缓存；老 IMDb 时代的缓存必须被判失效。
	payload, _ := json.Marshal(Meta{
		IMDbID: "tt15239678", TMDbID: 693134, Title: "Dune: Part Two",
		TitleCN: "沙丘2", Year: "2024", Source: "tmdb",
		CastDetail: []tmdb.CastMember{{Name: "赞达亚"}},
	})
	key := store.MetaCacheKey(KindMovie, "Dune Part Two", "2024")
	if err := st.PutMetaCache(ctx, key, "tt15239678", string(payload)); err != nil {
		t.Fatal(err)
	}

	job := &Job{}
	m, cached, err := svc.resolveOne(ctx, &pacer{}, groupKey{
		kind: KindMovie, title: "Dune Part Two", year: "2024",
	}, "Dune Part Two", nil, Options{FetchMeta: true}, job)
	if err != nil {
		t.Fatal(err)
	}
	if !cached || m.IMDbID != "tt15239678" || m.TMDbID != 693134 {
		t.Fatalf("应命中缓存，实际 cached=%v meta=%+v", cached, m)
	}
	_ = lib
}

// TestScrapeRejectsLegacyCache 老缓存没有 TMDbID，不能冒充 TMDB 结果。
func TestScrapeRejectsLegacyCache(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)
	svc, st, _, _ := newTestService(t)
	ctx := context.Background()
	payload, _ := json.Marshal(Meta{IMDbID: "tt15239678", Title: "Dune: Part Two", Year: "2024", Source: "imdb"})
	key := store.MetaCacheKey(KindMovie, "Dune Part Two", "2024")
	if err := st.PutMetaCache(ctx, key, "tt15239678", string(payload)); err != nil {
		t.Fatal(err)
	}
	m, cached, err := svc.resolveOne(ctx, &pacer{}, groupKey{
		kind: KindMovie, title: "Dune Part Two", year: "2024",
	}, "Dune Part Two", nil, Options{FetchMeta: true, Translate: false}, &Job{})
	if err != nil {
		t.Fatal(err)
	}
	if cached || m.TMDbID != 693134 || m.Source != "tmdb" {
		t.Fatalf("老缓存应被重查：cached=%v meta=%+v", cached, m)
	}
}

// TestScrapeUsesIMDbIDFromName 文件名里写了 IMDb ID 时不该再走搜索接口。
//
// ID 此时只作 TMDB /find 的反查键，不是去 IMDb 取数据。
func TestScrapeUsesIMDbIDFromName(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, mediaRoot := newTestService(t)
	ctx := context.Background()
	if err := st.UpsertFiles(ctx, lib.AccountID, []store.File{
		{LibraryID: lib.ID, FileID: "1", PickCode: "a",
			Name:    "Dune.Part.Two.2024.mkv",
			RelPath: "电影/沙丘2 (2024) {imdb-tt15239678}/Dune.Part.Two.2024.mkv",
			Size:    20 << 30},
	}); err != nil {
		t.Fatal(err)
	}

	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	job, err := svc.Start(lib, opt)
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()
	if p := job.Snapshot(); p.Matched != 1 {
		t.Fatalf("应匹配 1 条，实际 matched=%d unmatched=%d err=%s", p.Matched, p.Unmatched, p.Err)
	}
	searches, finds, details, _, _ := fake.counts()
	if searches != 0 {
		t.Errorf("有 IMDb ID 时不该再搜索（搜索接口被打了 %d 次）", searches)
	}
	if finds != 1 || details != 1 {
		t.Errorf("应各请求 1 次 /find 与详情，实际 find=%d detail=%d", finds, details)
	}

	raw, err := os.ReadFile(filepath.Join(mediaRoot, "电影/沙丘2 (2024) {imdb-tt15239678}/Dune.Part.Two.2024.nfo"))
	if err != nil {
		t.Fatalf("读 nfo 失败: %v", err)
	}
	if !strings.Contains(string(raw), "tt15239678") || !strings.Contains(string(raw), `type="tmdb"`) {
		t.Errorf("nfo 里应同时有 imdb 与 tmdb uniqueid:\n%s", raw)
	}
}

// TestRescrapeOneByIMDbID 单条重刮只碰这一条，且按 ID 直查 TMDB。
func TestRescrapeOneByIMDbID(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, mediaRoot := newTestService(t)
	ctx := context.Background()
	rel := "电影/空枪.2026.2160p.mkv"
	if err := st.UpsertFiles(ctx, lib.AccountID, []store.File{
		{LibraryID: lib.ID, FileID: "1", PickCode: "a", Name: "空枪.2026.2160p.mkv",
			RelPath: rel, Size: 20 << 30},
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mediaRoot, "电影"), 0o755); err != nil {
		t.Fatal(err)
	}
	m := store.Media{
		LibraryID: lib.ID, RelPath: rel, Kind: KindMovie, Title: "空枪", Year: "2026",
		Status: "unmatched", Message: "TMDB 里没找到足够接近的结果",
		// 故意留一份旧数据，重刮后必须被清掉
		Director: "错误导演", Genres: "错误类型",
	}
	if err := st.UpsertMedia(ctx, m); err != nil {
		t.Fatal(err)
	}

	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	opt.Overwrite = true

	got, err := svc.RescrapeOne(ctx, lib, m, "https://www.imdb.com/title/tt15239678/", opt)
	if err != nil {
		t.Fatalf("重刮失败: %v", err)
	}
	if got.IMDbID != "tt15239678" || got.TMDbID != 693134 || got.Status != "ok" {
		t.Fatalf("结果不对: %+v", got)
	}
	if got.Director != "丹尼斯·维伦纽瓦" || strings.Contains(got.Genres, "错误") {
		t.Errorf("旧字段没被替换: director=%q genres=%q", got.Director, got.Genres)
	}
	searches, finds, _, _, _ := fake.counts()
	if searches != 0 {
		t.Error("按 ID 重刮不该走搜索接口")
	}
	if finds != 1 {
		t.Errorf("应只请求 1 次 /find，实际 %d 次", finds)
	}

	raw, err := os.ReadFile(filepath.Join(mediaRoot, "电影/空枪.2026.2160p.nfo"))
	if err != nil {
		t.Fatalf("nfo 没写到 .strm 旁边: %v", err)
	}
	if !strings.Contains(string(raw), "<uniqueid") || !strings.Contains(string(raw), "tt15239678") {
		t.Errorf("nfo 内容不对:\n%s", raw)
	}

	// 再点一次：命中本地缓存，0 请求。
	if _, err := svc.RescrapeOne(ctx, lib, got, "tt15239678", opt); err != nil {
		t.Fatal(err)
	}
	_, finds2, _, _, _ := fake.counts()
	if finds2 != 1 {
		t.Errorf("第二次重刮又请求了上游（find=%d）", finds2)
	}
}

// TestNeededSeasonsAndCoverage 缓存命中要按「需要的季是否都覆盖」判断。
func TestNeededSeasonsAndCoverage(t *testing.T) {
	got := neededSeasons(map[int]struct{}{2: {}, 1: {}, 3: {}})
	if len(got) != 3 || got[0] != 1 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("季号没排序: %v", got)
	}
	m := Meta{EpisodesFetched: true, EpisodesSeasons: []int{1}}
	if !seasonsCovered(m, []int{1}) {
		t.Error("已有 S01 应命中缓存")
	}
	if seasonsCovered(m, []int{1, 2}) {
		t.Error("缺 S02 时不该命中缓存，否则新一季全退回剧名")
	}
	if seasonsCovered(Meta{}, nil) {
		t.Error("没拉过分集时不能算覆盖")
	}
}

// TestRescrapeOneByTMDbID 直接按 TMDB ID 重刮：不搜、不走 /find，
// 一次详情就够；IMDb ID 由 external_ids 带回来而不是照抄用户输入。
func TestRescrapeOneByTMDbID(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, mediaRoot := newTestService(t)
	ctx := context.Background()
	rel := "电影/沙丘2.2024.2160p.mkv"
	m := store.Media{
		LibraryID: lib.ID, RelPath: rel, Kind: KindMovie, Title: "沙丘2", Year: "2024",
		Status: "unmatched", Message: "TMDB 里没找到足够接近的结果",
		Director: "错误导演", Genres: "错误类型",
	}
	if err := st.UpsertMedia(ctx, m); err != nil {
		t.Fatal(err)
	}
	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	opt.Overwrite = true

	got, err := svc.RescrapeOneByTMDbID(ctx, lib, m, 693134, opt)
	if err != nil {
		t.Fatalf("按 TMDB ID 重刮失败: %v", err)
	}
	if got.TMDbID != 693134 || got.IMDbID != "tt15239678" || got.Status != "ok" {
		t.Fatalf("结果不对: %+v", got)
	}
	if got.Director != "丹尼斯·维伦纽瓦" || strings.Contains(got.Genres, "错误") {
		t.Errorf("旧字段没被替换: director=%q genres=%q", got.Director, got.Genres)
	}
	searches, finds, details, _, _ := fake.counts()
	if searches != 0 || finds != 0 || details != 1 {
		t.Errorf("应只请求 1 次详情：search=%d find=%d detail=%d", searches, finds, details)
	}
	raw, err := os.ReadFile(filepath.Join(mediaRoot, "电影/沙丘2.2024.2160p.nfo"))
	if err != nil {
		t.Fatalf("nfo 没写出来: %v", err)
	}
	if !strings.Contains(string(raw), `type="tmdb"`) || !strings.Contains(string(raw), "tt15239678") {
		t.Errorf("nfo 缺 tmdb / imdb uniqueid:/n%s", raw)
	}
}

// TestScrapeEnglishOnlyReleaseName 盯住「发布名只有英文、TMDB 中文名对不上」。
//
// 《凡人修仙传》的发布名是 `A Record of a Mortal’s Journey to Immortality`，
// 而 TMDB 的 zh-CN 名字只有中文。只搜一次 zh-CN 的话片名对不上，整部剧的
// 集数都会变成「没匹配」，库里就是一堆灰卡片。这里要求退一步用 en-US 再搜。
func TestScrapeEnglishOnlyReleaseName(t *testing.T) {
	fake := &fakeTMDB{}
	fake.start(t)

	svc, st, lib, _ := newTestService(t)
	ctx := context.Background()

	name := "A Record of a Mortal\u2019s Journey to Immortality Episode 199 1080p.mkv"
	files := []store.File{{
		LibraryID: lib.ID, FileID: "1", PickCode: "a", Name: name,
		RelPath: "剧集/A Record of a Mortal\u2019s Journey to Immortality/Season 1/" + name,
		Size:    1 << 30,
	}}
	if err := st.UpsertFiles(ctx, lib.AccountID, files); err != nil {
		t.Fatal(err)
	}

	opt := DefaultOptions()
	opt.Rate = 0
	opt.Translate = false
	job, err := svc.Start(lib, opt)
	if err != nil {
		t.Fatal(err)
	}
	<-job.Done()

	p := job.Snapshot()
	if p.Phase != "done" {
		t.Fatalf("任务没跑完: phase=%s err=%s", p.Phase, p.Err)
	}
	if p.Matched != 1 || p.Unmatched != 0 {
		t.Fatalf("英文发布名应能在 en-US 下匹配上，实际 matched=%d unmatched=%d", p.Matched, p.Unmatched)
	}
}
