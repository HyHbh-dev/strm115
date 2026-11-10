package calendar

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strm115/internal/douban"
	"strm115/internal/store"
	"strm115/internal/tmdb"
)

var fixedNow = time.Date(2026, 11, 8, 12, 0, 0, 0, time.Local)
var weekStart = time.Date(2026, 11, 2, 0, 0, 0, 0, time.Local)

type harness struct {
	svc         *Service
	tmdbCalls   atomic.Int32
	tvdbCalls   atomic.Int32
	tvmazeCalls atomic.Int32
	doubanCalls atomic.Int32
	now         time.Time
	creds       string
	source      string
	tvmazeBase  string
	// empty 为 true 时假 TMDB 的所有列表都返回空，用来造「一轮排期都没排出来」
	empty atomic.Bool
	// doubanOn 默认关：老用例只关心 TMDB/TVDB/TVMaze，豆瓣开着会让 Total 变多。
	// 要验证豆瓣那半边的用例自己打开。
	doubanOn atomic.Bool
	// doubanFails 让假豆瓣一律 418，用来验「豆瓣挂了日历主体还在」。
	doubanFails atomic.Bool
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	// 老用例显式用 tvdb，免得默认源切换后被假服务器漏掉请求。
	h := &harness{now: fixedNow, creds: "test-key", source: "tvdb"}

	tmdbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.tmdbCalls.Add(1)
		p := r.URL.Path
		page := r.URL.Query().Get("page")
		if h.empty.Load() && (strings.HasPrefix(p, "/tv/on_the_air") ||
			p == "/tv/airing_today" || p == "/discover/movie") {
			_, _ = w.Write([]byte(`{"results":[]}`))
			return
		}
		switch {
		case p == "/tv/on_the_air" && page == "1":
			_, _ = w.Write([]byte(`{"results":[
				{"id":100,"name":"在播剧","original_name":"On Air Show","poster_path":"/a.jpg","popularity":900,
				 "first_air_date":"2020-01-01","origin_country":["US"]},
				{"id":200,"name":"新剧","original_name":"New Show","poster_path":"/b.jpg","popularity":100}]}`))
		case p == "/tv/on_the_air" && page == "2":
			_, _ = w.Write([]byte(`{"results":[]}`))
		case p == "/tv/airing_today":
			_, _ = w.Write([]byte(`{"results":[{"id":100,"name":"在播剧","popularity":900}]}`))
		case p == "/tv/100":
			_, _ = w.Write([]byte(`{"id":100,"name":"在播剧","original_name":"On Air Show","status":"Returning Series",
				"first_air_date":"2020-01-01","last_air_date":"2026-12-31","poster_path":"/a.jpg",
				"networks":[{"name":"NBC"}],"episode_run_time":[42],
				"seasons":[{"season_number":2,"episode_count":8,"air_date":"2026-09-01"}],
				"last_episode_to_air":{"season_number":2,"episode_number":5,"name":"第五集","air_date":"2026-11-05"},
				"next_episode_to_air":{"season_number":2,"episode_number":6,"name":"第六集","air_date":"2026-11-12"}}`))
		case p == "/tv/100/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt100","tvdb_id":371028}`))
		case p == "/tv/100/season/2":
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":4,"name":"第四集","air_date":"2026-10-29"},
				{"episode_number":5,"name":"第五集","air_date":"2026-11-05","overview":"本周这集","runtime":42},
				{"episode_number":6,"name":"第六集","air_date":"2026-11-12"}]}`))
		case p == "/tv/200":
			_, _ = w.Write([]byte(`{"id":200,"name":"新剧","original_name":"New Show","status":"Returning Series",
				"first_air_date":"2026-11-03","last_air_date":"2026-12-31",
				"seasons":[{"season_number":1,"episode_count":4,"air_date":"2026-11-03"}]}`))
		case p == "/tv/200/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt200"}`))
		case p == "/tv/200/season/1":
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":1,"name":"试播集","air_date":"2026-11-03"},
				{"episode_number":2,"name":"第二集","air_date":"2026-11-10"}]}`))
		case p == "/discover/movie":
			_, _ = w.Write([]byte(`{"results":[
				{"id":300,"title":"本周电影","release_date":"2026-11-06","poster_path":"/m.jpg"},
				{"id":301,"title":"上周电影","release_date":"2026-10-01"}]}`))
		default:
			t.Errorf("意外的 TMDB 路径 %s", p)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(tmdbSrv.Close)

	tvdbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.tvdbCalls.Add(1)
		switch r.URL.Path {
		case "/login":
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":"T"}}`))
		case "/series/371028/extended":
			_, _ = w.Write([]byte(`{"status":"success","data":{
				"id":371028,"name":"On Air Show","airsTime":"20:00","airsDays":{"thursday":true},
				"originalNetwork":{"name":"NBC"},"image":"https://artworks.thetvdb.com/p.jpg",
				"episodes":[
					{"id":1,"name":"Ep5","aired":"2026-11-05","seasonNumber":2,"episodeNumber":5,"runtime":45},
					{"id":2,"name":"第七集","aired":"2026-11-07","seasonNumber":2,"episodeNumber":7,"runtime":44}]}}`))
		default:
			t.Errorf("意外的 TVDB 路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(tvdbSrv.Close)

	// TVMaze 假排期：只有 11-05 的 S02E05 有时刻，用来验证三级对齐与「只补字段」。
	tvmazeSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.tvmazeCalls.Add(1)
		switch r.URL.Path {
		case "/schedule":
			if r.URL.Query().Get("date") == "2026-11-05" {
				_, _ = w.Write([]byte(`[{"id":1,"name":"Ep5 EN","airdate":"2026-11-05","airtime":"20:30",
				  "season":2,"number":5,"runtime":46,
				  "show":{"id":9,"name":"On Air Show","network":{"name":"NBC"},
				    "externals":{"thetvdb":371028,"imdb":"tt100"}}}]`))
				return
			}
			_, _ = w.Write([]byte(`[]`))
		case "/schedule/web":
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("意外的 TVMaze 路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(tvmazeSrv.Close)
	h.tvmazeBase = tvmazeSrv.URL

	// 假豆瓣：榜单接口只有 4 个会被日历用到。日期一律写完整的 YYYY-MM-DD，
	// 因为 parseRelease 对「11.10」这种写法要按 time.Now() 补年份，测试里会飘。
	doubanSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.doubanCalls.Add(1)
		if h.doubanFails.Load() {
			w.WriteHeader(418)
			_, _ = w.Write([]byte(`{"msg":"rate limited"}`))
			return
		}
		const head = `/rexxar/api/v2/subject_collection/`
		key := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, head), "/items")
		switch key {
		case "movie_soon":
			_, _ = w.Write([]byte(`{"total":2,"subject_collection_items":[
				{"id":"4001","title":"国产电影甲","type":"movie","year":"2026",
				 "rating":{"value":8.9},"most_recent_release_date":"2026-11-06",
				 "cover":{"url":"http://img.example/db1.jpg"}},
				{"id":"4002","title":"下月才上的片","type":"movie","year":"2026",
				 "most_recent_release_date":"2026-12-25"}]}`))
		case "movie_showing":
			_, _ = w.Write([]byte(`{"total":1,"subject_collection_items":[
				{"id":"4003","title":"已经在映的老片","type":"movie","year":"2026",
				 "most_recent_release_date":"2026-10-01"}]}`))
		case "tv_domestic":
			_, _ = w.Write([]byte(`{"total":1,"subject_collection_items":[
				{"id":"5001","title":"国产剧甲","type":"tv","year":"2026",
				 "episodes_info":"更新至12集","card_subtitle":"2026 / 中国大陆 / 剧情",
				 "rating":{"value":7.8},"cover":{"url":"http://img.example/db5.jpg"}}]}`))
		case "tv_hot":
			_, _ = w.Write([]byte(`{"total":3,"subject_collection_items":[
				{"id":"5002","title":"国产热播乙","type":"tv","year":"2026",
				 "episodes_info":"更新至6集","card_subtitle":"2026 / 中国大陆 / 都市"},
				{"id":"5003","title":"美剧不该进来","type":"tv","year":"2026",
				 "episodes_info":"更新至3集","card_subtitle":"2026 / 美国 / 犯罪"},
				{"id":"5004","title":"电影混在剧榜里","type":"movie","year":"2026",
				 "episodes_info":"更新至3集","card_subtitle":"2026 / 中国大陆"}]}`))
		default:
			t.Errorf("意外的豆瓣路径 %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(doubanSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "cal.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	client := tmdb.NewClient("test-key")
	client.Base = tmdbSrv.URL
	// 一个进程一个客户端：豆瓣客户端内部有串行节流与 URL 缓存，
	// 每轮重建会把节流状态丢掉，测试也会被 200ms 间隔拖成好几秒。
	dbClient := douban.NewWithBases("", doubanSrv.URL, doubanSrv.URL)
	h.svc = New(Deps{
		Store: st,
		TMDb:  func() *tmdb.Client { return client },
		Credentials: func() (string, string) {
			return h.creds, "1234"
		},
		TVDBBase:       tvdbSrv.URL,
		SourceFunc:     func() string { return h.source },
		TVMazeBaseFunc: func() string { return h.tvmazeBase },
		Douban: func() *douban.Client {
			if !h.doubanOn.Load() {
				return nil // 没启用：日历一个豆瓣请求都不该发
			}
			return dbClient
		},
		Now: func() time.Time { return h.now },
	})
	return h
}

func findEntry(w *Week, date, code string) *Entry {
	for i := range w.Days {
		if w.Days[i].Date != date {
			continue
		}
		for j := range w.Days[i].Items {
			if w.Days[i].Items[j].Code == code {
				return &w.Days[i].Items[j]
			}
		}
	}
	return nil
}

func findMovie(w *Week, date string) *Entry {
	for i := range w.Days {
		if w.Days[i].Date == date {
			for j := range w.Days[i].Items {
				if w.Days[i].Items[j].Kind == "movie" {
					return &w.Days[i].Items[j]
				}
			}
		}
	}
	return nil
}

func TestWeekBuildsFromBothSources(t *testing.T) {
	h := newHarness(t)
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Start != "2026-11-02" || w.End != "2026-11-08" || len(w.Days) != 7 {
		t.Errorf("周边界错: %+v", w)
	}
	if w.Total != 4 {
		t.Errorf("应有 4 张卡（在播剧 S02E05 + TVDB 独有 S02E07 + 新剧 S01E01 + 本周电影），实际 %d", w.Total)
	}
	e5 := findEntry(w, "2026-11-05", "S02E05")
	if e5 == nil {
		t.Fatal("缺 11-05 的 S02E05")
	}
	if e5.AirTime != "20:00" || e5.Network != "NBC" || e5.EpisodeTitle != "第五集" {
		t.Errorf("TMDB 主条目 / TVDB 补字段不对: %+v", e5)
	}
	e7 := findEntry(w, "2026-11-07", "S02E07")
	if e7 == nil || e7.Source != "tvdb" || e7.AirTime != "20:00" {
		t.Errorf("TVDB 独有的集没补进来: %+v", e7)
	}
	e1 := findEntry(w, "2026-11-03", "S01E01")
	if e1 == nil || !e1.NewShow || !e1.Premiere {
		t.Errorf("新剧首播标记不对: %+v", e1)
	}
	if m := findMovie(w, "2026-11-06"); m == nil || m.Title != "本周电影" {
		t.Errorf("电影没进日历: %+v", m)
	}
	if !w.Sources.TMDB || !w.Sources.TVDB || !w.Sources.TVDBConfigured || w.Sources.Shows != 2 {
		t.Errorf("来源标记不对: %+v", w.Sources)
	}
	if w.Notice != "" {
		t.Errorf("数据源齐全时不该有提示: %s", w.Notice)
	}
}

func TestWeekCacheAndRefresh(t *testing.T) {
	h := newHarness(t)
	if _, err := h.svc.Week(context.Background(), weekStart, false); err != nil {
		t.Fatal(err)
	}
	afterFirst := h.tmdbCalls.Load()
	if afterFirst == 0 {
		t.Fatal("第一轮没打 TMDB")
	}

	w2, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if !w2.Cached || h.tmdbCalls.Load() != afterFirst {
		t.Errorf("第二轮该整周命中缓存，实际 cached=%v 请求 %d→%d",
			w2.Cached, afterFirst, h.tmdbCalls.Load())
	}

	w3, err := h.svc.Week(context.Background(), weekStart, true)
	if err != nil {
		t.Fatal(err)
	}
	if w3.Cached || h.tmdbCalls.Load() <= afterFirst {
		t.Errorf("refresh=1 应该穿透缓存，实际 cached=%v 请求 %d→%d",
			w3.Cached, afterFirst, h.tmdbCalls.Load())
	}

	// 本周缓存 30 分钟：推过 TTL 后应重新抓，而不是永远拿旧数据。
	afterRefresh := h.tmdbCalls.Load()
	h.now = fixedNow.Add(31 * time.Minute)
	if _, err := h.svc.Week(context.Background(), weekStart, false); err != nil {
		t.Fatal(err)
	}
	if h.tmdbCalls.Load() <= afterRefresh {
		t.Error("超过 30 分钟缓存没失效")
	}
}

func TestWeekWithoutTVDBDegradesGracefully(t *testing.T) {
	h := newHarness(t)
	h.creds = ""
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Sources.TVDBConfigured || w.Sources.TVDB || !w.Sources.TMDB {
		t.Errorf("降级状态不对: %+v", w.Sources)
	}
	if !strings.Contains(w.Notice, "未配置 TVDB") {
		t.Errorf("应给出可读的降级提示，实际 %q", w.Notice)
	}
	if e5 := findEntry(w, "2026-11-05", "S02E05"); e5 == nil || e5.AirTime != "" || e5.Network != "NBC" {
		t.Errorf("没有 TVDB 时 TMDB 的电视网仍应显示: %+v", e5)
	}
	if findEntry(w, "2026-11-07", "S02E07") != nil {
		t.Error("没有 TVDB 时不该出现 TVDB 独有的集")
	}
}

func TestWeekUsesTVMazeByDefault(t *testing.T) {
	h := newHarness(t)
	h.source = "tvmaze"
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Sources.Source != "tvmaze" || !w.Sources.TVMaze || w.Sources.TVDB {
		t.Errorf("应走 TVMaze 而不是 TVDB：%+v", w.Sources)
	}
	if w.Sources.Attribution == "" || w.Sources.AttributionURL != "https://www.tvmaze.com" {
		t.Errorf("CC BY-SA 署名缺失：%+v", w.Sources)
	}
	if h.tvmazeCalls.Load() == 0 {
		t.Error("没打 TVMaze 排期")
	}
	// 只补字段、不新增卡片：TVDB 那条独有集不该出现。
	if w.Total != 3 {
		t.Errorf("TVMaze 不该新增条目，实际 %d 张卡", w.Total)
	}
	e5 := findEntry(w, "2026-11-05", "S02E05")
	if e5 == nil || e5.AirTime != "20:30" {
		t.Errorf("tvdb_id 对齐没补上时刻：%+v", e5)
	}
	if e5.Network != "NBC" || e5.EpisodeTitle != "第五集" || e5.Runtime != 42 {
		t.Errorf("TMDB 已有字段不该被覆盖：%+v", e5)
	}
	if w.Notice != "" {
		t.Errorf("增强成功时不该有提示：%s", w.Notice)
	}
}

func TestTVMazeFailureStillReturnsCalendar(t *testing.T) {
	h := newHarness(t)
	h.source = "tvmaze"
	h.tvmazeBase = "http://127.0.0.1:1"
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatalf("TVMaze 挂了不该让整张日历失败：%v", err)
	}
	if !w.Sources.TMDB || w.Sources.TVMaze || w.Total != 3 {
		t.Errorf("降级状态不对：%+v / total=%d", w.Sources, w.Total)
	}
	if !strings.Contains(w.Notice, "TVMaze 增强未完成") {
		t.Errorf("应给出可读的降级原因：%q", w.Notice)
	}
}

func TestCalendarSourceOff(t *testing.T) {
	h := newHarness(t)
	h.source = "off"
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Sources.Source != "off" || w.Sources.TVMaze || w.Sources.TVDB {
		t.Errorf("关闭增强后不该有任何增强源：%+v", w.Sources)
	}
	if !strings.Contains(w.Notice, "已关闭时刻增强") {
		t.Errorf("应说明时刻被关掉了：%q", w.Notice)
	}
	if e5 := findEntry(w, "2026-11-05", "S02E05"); e5 == nil || e5.AirTime != "" {
		t.Errorf("关闭增强后不该有时刻：%+v", e5)
	}
}

func TestParseStart(t *testing.T) {
	got, err := ParseStart("", fixedNow)
	if err != nil || got.Format("2006-01-02") != "2026-11-02" {
		t.Errorf("默认应是本周一，实际 %v / %v", got, err)
	}
	got, err = ParseStart("2026-03-04", fixedNow)
	if err != nil || got.Format("2006-01-02") != "2026-03-04" {
		t.Errorf("应允许任意起点（截图是周三起），实际 %v / %v", got, err)
	}
	if _, err := ParseStart("03/04/2026", fixedNow); err == nil {
		t.Error("非法日期应报错")
	}
	if _, err := ParseStart("2030-01-01", fixedNow); err == nil {
		t.Error("超出前后两年应报错")
	}
}

func TestCandidateSeasonsAndWithin(t *testing.T) {
	d := &tmdb.ShowDetail{
		LastAired:   &tmdb.TVEpisode{Season: 2, Episode: 5, AirDate: "2026-11-05"},
		NextAiring:  &tmdb.TVEpisode{Season: 2, Episode: 6, AirDate: "2026-11-12"},
		LastAirDate: "2026-12-31",
		Seasons:     []tmdb.TVSeason{{Season: 2, AirDate: "2026-09-01"}},
	}
	got := candidateSeasons(d, "2026-11-02", "2026-11-08")
	if len(got) != 1 || got[0] != 2 {
		t.Errorf("应只查第 2 季: %v", got)
	}
	if within("2026-11-12", "2026-11-02", "2026-11-08") {
		t.Error("下周的集不该算本周")
	}
	if !within("2026-11-02", "2026-11-02", "2026-11-08") || !within("2026-11-08", "2026-11-02", "2026-11-08") {
		t.Error("边界日期要包含")
	}
	if within("", "2026-11-02", "2026-11-08") {
		t.Error("空日期不该命中")
	}
	// last/next 都在周外、但季首播在周内：新季仍要查。
	d2 := &tmdb.ShowDetail{Seasons: []tmdb.TVSeason{{Season: 3, AirDate: "2026-11-03"}}}
	if got := candidateSeasons(d2, "2026-11-02", "2026-11-08"); len(got) != 1 || got[0] != 3 {
		t.Errorf("新季首播没被挑中: %v", got)
	}
}

// 设置页改 tvdb_base 后，下一轮日历必须用新地址重建 Client：
// Bearer Token 是那个域签发的，沿用旧 Client 会一直 401。
func TestTVDBClientFollowsBaseFunc(t *testing.T) {
	base := "https://tvdb-a.example/v4"
	s := New(Deps{
		Credentials:  func() (string, string) { return "k", "p" },
		TVDBBaseFunc: func() string { return base },
	})
	c1 := s.tvdbClient()
	if c1 == nil || c1.Base != "https://tvdb-a.example/v4" {
		t.Fatalf("没用上动态接口根: %+v", c1)
	}
	if c2 := s.tvdbClient(); c2 != c1 {
		t.Error("配置没变时不该重建 Client（Token 会被丢掉）")
	}
	base = "https://tvdb-b.example/v4/"
	c3 := s.tvdbClient()
	if c3 == c1 || c3.Base != "https://tvdb-b.example/v4" {
		t.Errorf("换地址后没重建 / 没去尾斜杠: %+v", c3)
	}

	// 动态函数留空时退回静态值（单测里的假服务器就是这么接的）
	st := New(Deps{
		Credentials:  func() (string, string) { return "k", "p" },
		TVDBBase:     "http://127.0.0.1:1/v4",
		TVDBBaseFunc: func() string { return "" },
	})
	if got := st.tvdbClient(); got == nil || got.Base != "http://127.0.0.1:1/v4" {
		t.Errorf("动态为空时该退回静态值: %+v", got)
	}
}

// TestEmptyWeekIsNotCached 复现「关页面把日历弄空白」：
// 客户端提前断开 / 上游临时失败时，build 不报错地返回一份空周历。
// 这份东西绝不能进缓存 —— 内存缓存优先于库，一旦写进去，接下来 30 分钟
// 所有人看到的都是空白日历，「我的订阅」筛选也跟着恒空，只能等 TTL 或手动重抓。
func TestEmptyWeekIsNotCached(t *testing.T) {
	h := newHarness(t)
	h.source = "off" // 不走时刻增强，专注缓存行为
	h.empty.Store(true)

	w1, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w1.Total != 0 {
		t.Fatalf("这一轮本该是空的，实际 %d 条", w1.Total)
	}
	callsAfterEmpty := h.tmdbCalls.Load()

	// 上游恢复：下一轮必须真的重抓，而不是命中那份空白缓存。
	h.empty.Store(false)
	w2, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w2.Cached {
		t.Fatal("空白周历被缓存了：Cached=true")
	}
	if w2.Total == 0 {
		t.Fatal("上游恢复后仍是空日历")
	}
	if h.tmdbCalls.Load() <= callsAfterEmpty {
		t.Fatal("没重新抓 TMDB，说明命中了空白缓存")
	}
}

// findByKey 按 Key 前缀找：同一天可能既有一部 TMDB 电影又有一部豆瓣电影，
// 只按日期挑会挑到谁全看排序。豆瓣拆出去后 DoubanWeek 直接返回 []Entry，
// 这个 helper 跟着改成在 Entry 切片里找。
func findByKey(entries []Entry, prefix string) *Entry {
	for i := range entries {
		if strings.HasPrefix(entries[i].Key, prefix) {
			return &entries[i]
		}
	}
	return nil
}

// TestWeekIncludesDoubanMoviesAndTV 验豆瓣那一半（现由公开方法 DoubanWeek 提供）：
// 有明确上映日的电影进 movies，档期不在本周的不进，国产剧整块放 TV，
// 美剧和电影不混进剧列表。覆盖拆分前 TestWeekIncludesDoubanMoviesAndTV 的
// 全部断言，只是数据来源从 Week() 改成 DoubanWeek()。
func TestWeekIncludesDoubanMoviesAndTV(t *testing.T) {
	h := newHarness(t)
	h.doubanOn.Store(true)
	block, entries := h.svc.DoubanWeek(context.Background(), weekStart)

	if !block.Enabled {
		t.Error("开着豆瓣却没标 Enabled，前端分不清「关了」和「没匹配上」")
	}
	if !block.OK || block.Error != "" {
		t.Fatalf("豆瓣该成功: %+v", block)
	}

	m := findByKey(entries, "dbmovie:4001:")
	if m == nil {
		t.Fatal("豆瓣本周上映的电影没进 movies")
	}
	if m.Date != "2026-11-06" || m.Kind != "movie" || m.Source != "douban" || m.DoubanID != 4001 {
		t.Errorf("豆瓣电影字段不对: %+v", m)
	}
	if m.Title != "国产电影甲" || m.Rating != 8.9 {
		t.Errorf("片名/评分没透传: %+v", m)
	}
	if m.Poster != "https://img.example/db1.jpg" {
		t.Errorf("海报没升到 https: %q", m.Poster)
	}
	if m.PageURL == "" || !strings.Contains(m.PageURL, "4001") {
		t.Errorf("豆瓣条目要给豆瓣详情页: %q", m.PageURL)
	}
	// 电影不是「新剧」——以前 TMDB 电影也被标成新剧，导致两个筛选看起来一样。
	if m.NewShow || m.Premiere {
		t.Errorf("电影不该标新剧/首播: NewShow=%v Premiere=%v", m.NewShow, m.Premiere)
	}

	// 档期不在本周的两部（下月上映 / 已经过档）都不该出现。
	if findByKey(entries, "dbmovie:4002:") != nil {
		t.Error("下月上映的电影混进了本周")
	}
	if findByKey(entries, "dbmovie:4003:") != nil {
		t.Error("早已上映的电影混进了本周")
	}

	if len(block.TV) != 2 {
		t.Fatalf("国产剧该有 2 部（美剧与电影不应混进来），实际 %d: %+v", len(block.TV), block.TV)
	}
	if block.TV[0].Title != "国产剧甲" || block.TV[0].EpisodesInfo != "更新至12集" {
		t.Errorf("国产剧进度没透传: %+v", block.TV[0])
	}
	if block.TV[1].Title != "国产热播乙" {
		t.Errorf("热播榜里的国产剧没并进来: %+v", block.TV[1])
	}
	for _, it := range block.TV {
		if it.Kind != "tv" {
			t.Errorf("TV 里混进了非剧集: %+v", it)
		}
		if !strings.Contains(it.CardSubtitle, "中国") {
			t.Errorf("TV 里混进了非国产剧: %+v", it)
		}
	}

	// 拆分后主响应 /api/calendar 只回 TMDB 主体：即使豆瓣开着也不再等它、
	// 不把豆瓣电影并进 days。总数仍是原来的 4 条，Douban 只是占位。
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if w.Total != 4 {
		t.Errorf("主响应该只剩 TMDB 的 4 条（豆瓣已拆走），实际 %d", w.Total)
	}
	if w.Douban.Enabled {
		t.Error("主响应的 Douban 应只是 enabled=false 占位，不能假装问过豆瓣")
	}
	if findEntry(w, "2026-11-05", "S02E05") == nil {
		t.Error("TMDB 条目丢了")
	}
}

// TestWeekDoubanDisabledSkipsRequests 验设置页关掉豆瓣后一个请求都不发，
// 也不该在界面上留下一块「豆瓣加载失败」的提示。现直接测公开方法 DoubanWeek。
func TestWeekDoubanDisabledSkipsRequests(t *testing.T) {
	h := newHarness(t)
	h.source = "off"
	block, entries := h.svc.DoubanWeek(context.Background(), weekStart)
	if n := h.doubanCalls.Load(); n != 0 {
		t.Errorf("豆瓣没启用却发了 %d 个请求", n)
	}
	if block.Enabled || block.OK {
		t.Errorf("没启用时不该说自己在问豆瓣: %+v", block)
	}
	if len(entries) != 0 || len(block.TV) != 0 {
		t.Errorf("没启用时不该有任何条目: %+v / %+v", entries, block.TV)
	}
}

// TestWeekDoesNotWaitForDouban 钉住拆分本身（Bug 1 后端核心）：即使豆瓣开着，
// 主响应 Week() 也不再同步拉豆瓣（0 请求），更不把它并进 days —— 豆瓣改由
// /api/calendar/douban 异步提供，主响应只回 TMDB 主体。
func TestWeekDoesNotWaitForDouban(t *testing.T) {
	h := newHarness(t)
	h.doubanOn.Store(true)
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	if n := h.doubanCalls.Load(); n != 0 {
		t.Errorf("主响应不该再打豆瓣，实际 %d 个请求", n)
	}
	if w.Douban.Enabled {
		t.Error("主响应的 Douban 只能是 enabled=false 占位")
	}
	if w.Total != 4 {
		t.Errorf("主响应只回 TMDB 主体 4 条，实际 %d", w.Total)
	}
}

// TestWeekDoubanFailureKeepsTMDB 验降级：豆瓣 418 时只写 Error，TMDB 主体不受影响。
// 拆分后豆瓣由 DoubanWeek 独立承担，主响应 Week() 根本不再碰豆瓣，天然不被拖垮。
func TestWeekDoubanFailureKeepsTMDB(t *testing.T) {
	h := newHarness(t)
	h.doubanOn.Store(true)
	h.doubanFails.Store(true)

	block, entries := h.svc.DoubanWeek(context.Background(), weekStart)
	if block.OK {
		t.Error("豆瓣全挂了 OK 不该是 true")
	}
	if !strings.Contains(block.Error, "豆瓣") {
		t.Errorf("错误原因要能看懂: %q", block.Error)
	}
	if len(block.TV) != 0 {
		t.Errorf("豆瓣挂了还有剧列表: %+v", block.TV)
	}
	if len(entries) != 0 {
		t.Errorf("豆瓣挂了还给出电影条目: %+v", entries)
	}

	// 主响应：TMDB 那一半照常返回，不因为豆瓣失败而减少或报错。
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatalf("豆瓣挂了不该让整张日历报错: %v", err)
	}
	if w.Total != 4 || findMovie(w, "2026-11-06") == nil {
		t.Errorf("TMDB 条目被豆瓣拖垮了: total=%d", w.Total)
	}
}

// TestTMDbMovieIsNotNewShow 单独钉住「日历里新剧和电影显示一样」这个 bug。
func TestTMDbMovieIsNotNewShow(t *testing.T) {
	h := newHarness(t)
	w, err := h.svc.Week(context.Background(), weekStart, false)
	if err != nil {
		t.Fatal(err)
	}
	m := findMovie(w, "2026-11-06")
	if m == nil {
		t.Fatal("缺 11-06 的电影")
	}
	if m.NewShow {
		t.Error("电影被标成了新剧，导致「新剧」筛选里全是电影")
	}
	if ep := findEntry(w, "2026-11-03", "S01E01"); ep == nil || !ep.NewShow {
		t.Errorf("真·新剧的首集该保留新剧标记: %+v", ep)
	}
}

// TestDoubanMoviesAreNotNewShow 钉住 Bug 2 的豆瓣侧：豆瓣电影 Entry 绝不能被
// 标成 NewShow / Premiere（两者只给「第一季第一集」的剧集）。否则「新剧」筛选
// 会混进电影，看起来和「电影」筛选一模一样。检查整条 doubanWeek 路径产出的
// 每一个电影条目，顺便确认这里只回电影、不混进别的种类。
func TestDoubanMoviesAreNotNewShow(t *testing.T) {
	h := newHarness(t)
	h.doubanOn.Store(true)
	_, entries := h.svc.DoubanWeek(context.Background(), weekStart)
	if len(entries) == 0 {
		t.Fatal("该有一部本周上映的豆瓣电影")
	}
	for _, e := range entries {
		if e.Kind != "movie" {
			t.Errorf("DoubanWeek 只该回电影 Entry，混进了 %s: %+v", e.Kind, e)
		}
		if e.NewShow || e.Premiere {
			t.Errorf("电影条目被标成新剧/首播: key=%s NewShow=%v Premiere=%v",
				e.Key, e.NewShow, e.Premiere)
		}
	}
}
