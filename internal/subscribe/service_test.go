package subscribe

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"strm115/internal/resource"
	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// ==================== 纯函数 ====================

func assertEps(t *testing.T, name string, got, want []tmdb.TVEpisode) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s：长度不匹配 got=%+v want=%+v", name, got, want)
	}
	for i := range want {
		if got[i].Season != want[i].Season || got[i].Episode != want[i].Episode {
			t.Fatalf("%s：第 %d 项是 S%02dE%02d，期望 S%02dE%02d",
				name, i, got[i].Season, got[i].Episode, want[i].Season, want[i].Episode)
		}
	}
}

// pendingEpisodes 决定「这一轮该补哪几集」，跨季和水位线为 0 是两个高危分支。
func TestPendingEpisodes(t *testing.T) {
	show := &tmdb.TVShow{
		Seasons: []tmdb.TVSeason{
			{Season: 2, EpisodeCount: 6},
			{Season: 1, EpisodeCount: 10},
		},
		LastAired: &tmdb.TVEpisode{Season: 2, Episode: 3},
	}

	// 1. 水位线 0：从 S01E01 开始，按季升序，不因为 seasons 数组乱序而错
	got := pendingEpisodes(0, 0, show, 3)
	want := []tmdb.TVEpisode{{Season: 1, Episode: 1}, {Season: 1, Episode: 2}, {Season: 1, Episode: 3}}
	assertEps(t, "首轮", got, want)

	// 2. 跨季：S01E10 之后应接 S02E01，而不是从 S02E01 重来或跳过整季
	got = pendingEpisodes(1, 9, show, 3)
	want = []tmdb.TVEpisode{{Season: 1, Episode: 10}, {Season: 2, Episode: 1}, {Season: 2, Episode: 2}}
	assertEps(t, "跨季", got, want)

	// 3. 追平：什么都不返回
	if got := pendingEpisodes(2, 3, show, 3); len(got) != 0 {
		t.Fatalf("已追平不该有待补集，实际 %+v", got)
	}

	// 4. 还没播的集不能出现在待补里（S02 只播到 E03）
	for _, ep := range pendingEpisodes(1, 10, show, 10) {
		if ep.Season == 2 && ep.Episode > 3 {
			t.Fatalf("没播的集 S02E%02d 不该待补", ep.Episode)
		}
	}

	// 5. 没有 seasons 明细时退化到最新那一季
	noSeasons := &tmdb.TVShow{LastAired: &tmdb.TVEpisode{Season: 1, Episode: 4}}
	got = pendingEpisodes(1, 2, noSeasons, 5)
	want = []tmdb.TVEpisode{{Season: 1, Episode: 3}, {Season: 1, Episode: 4}}
	assertEps(t, "无季明细", got, want)

	// 6. 未开播：Latest 为空，不该有待补
	if got := pendingEpisodes(0, 0, &tmdb.TVShow{}, 3); len(got) != 0 {
		t.Fatalf("未开播的剧不该有待补集")
	}
}

func TestBackoffDuration(t *testing.T) {
	cases := map[int]time.Duration{
		1:  time.Hour,
		2:  2 * time.Hour,
		3:  4 * time.Hour,
		4:  8 * time.Hour,
		5:  12 * time.Hour,
		9:  12 * time.Hour, // 封顶，不能无限涨
		99: 12 * time.Hour,
	}
	for n, want := range cases {
		if got := backoffDuration(n); got != want {
			t.Errorf("backoffDuration(%d) = %v，期望 %v", n, got, want)
		}
	}
}

func TestNextCheckAfterAir(t *testing.T) {
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)

	// 明天播：按播出日次日算（> 正常周期）
	got := nextCheckAfterAir("2026-11-09", 6*time.Hour, now)
	if want := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("播出次日的检查时间算错了: %v，期望 %v", got, want)
	}
	// 播出日期解析不出来：用正常周期
	if got := nextCheckAfterAir("", 6*time.Hour, now); !got.Equal(now.Add(6 * time.Hour)) {
		t.Fatalf("日期缺失时应退回正常周期，实际 %v", got)
	}
	// 播出日就在今天：按播出日次日 00:00 算（14 小时后），仍晚于 6 小时周期
	if got := nextCheckAfterAir("2026-11-08", 6*time.Hour, now); !got.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("今天播的集应该明天 0 点后再查，实际 %v", got)
	}
}

func TestEpisodeSpan(t *testing.T) {
	e := func(s, ep int) tmdb.TVEpisode { return tmdb.TVEpisode{Season: s, Episode: ep} }
	cases := []struct {
		eps  []tmdb.TVEpisode
		want string
	}{
		{nil, ""},
		{[]tmdb.TVEpisode{e(1, 5)}, "S01E05"},
		{[]tmdb.TVEpisode{e(1, 5), e(1, 6), e(1, 7)}, "S01E05-E07"},
		// 跨季不写区间：S01E10-S02E01 会被读成 3 集，其实是 2 集
		{[]tmdb.TVEpisode{e(1, 10), e(2, 1)}, "S01E10,S02E01"},
	}
	for _, c := range cases {
		if got := episodeSpan(c.eps); got != c.want {
			t.Errorf("episodeSpan(%v) = %q，期望 %q", c.eps, got, c.want)
		}
	}
}

// ==================== 端到端（真 store + 假上游） ====================

type submitCall struct {
	uris  []string
	cid   string
	label string
}

// 这条测试盯的是整个闭环唯一的硬承诺：**同一集不会被提交两次**。
// 它同时验证水位线推进、追平后不再提交、以及「没资源时不推进水位线」。
func TestCheckTVEndToEnd(t *testing.T) {
	st := openSubStore(t)
	ctx := context.Background()

	acctID, err := st.CreateAccount(ctx, "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "剧集", RemoteCID: "777",
		LocalPath: "/media/剧集", CollectionType: "tvshows",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 假 TMDB：一季 3 集，最新播到 E02（E03 还没播）
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/42":
			_, _ = w.Write([]byte(`{"id":42,"name":"某剧","first_air_date":"2026-10-01",
				"status":"Returning Series","number_of_episodes":3,
				"seasons":[{"season_number":1,"episode_count":3}],
				"last_episode_to_air":{"season_number":1,"episode_number":2}}`))
		case "/tv/42/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt42"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	var mu sync.Mutex
	var calls []submitCall
	svc := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{
		TMDb: func(context.Context) *tmdb.Client {
			c := tmdb.NewClient("k")
			c.Base = srv.URL
			return c
		},
		Search: func(context.Context, string, string, string, int) (*resource.Result, error) {
			// 三条单集资源 + 一条季包（季包不该被选中）
			return &resource.Result{Items: []resource.Item{
				{Title: "某剧 S01E01 1080p WEB-DL", Magnet: "magnet:?e1", Seeders: 5},
				{Title: "某剧 S01E02 1080p WEB-DL", Magnet: "magnet:?e2", Seeders: 9},
				{Title: "某剧 S01E03 1080p WEB-DL", Magnet: "magnet:?e3", Seeders: 9},
				{Title: "某剧 S01 1080p Complete", Magnet: "magnet:?season", Seeders: 99},
			}}, nil
		},
		Submit: func(_ context.Context, _ int64, uris []string, cid, label string) ([]string, error) {
			mu.Lock()
			calls = append(calls, submitCall{uris: uris, cid: cid, label: label})
			mu.Unlock()
			return []string{"hash1"}, nil
		},
		Interval: func(context.Context) time.Duration { return 6 * time.Hour },
	})

	subID, err := st.CreateSubscription(ctx, store.Subscription{
		TMDbID: 42, IMDbID: "tt42", MediaType: "tv",
		Title: "某剧", OriginalTitle: "Some Show", Year: 2026,
		LibraryID: libID, Quality: "1080p",
	})
	if err != nil {
		t.Fatal(err)
	}
	sub, err := st.GetSubscription(ctx, subID)
	if err != nil || sub == nil {
		t.Fatal(err)
	}

	// 第一轮：应提交 E01+E02（E03 还没播），不能碰季包
	msg, err := svc.CheckOne(ctx, *sub)
	if err != nil {
		t.Fatalf("第一轮检查失败: %v", err)
	}
	if msg != "已提交 S01E01-E02" {
		t.Fatalf("第一轮结论不对: %q", msg)
	}
	mu.Lock()
	if len(calls) != 1 {
		mu.Unlock()
		t.Fatalf("应提交 1 次，实际 %d 次", len(calls))
	}
	c := calls[0]
	mu.Unlock()
	if len(c.uris) != 2 || c.uris[0] != "magnet:?e1" || c.uris[1] != "magnet:?e2" {
		t.Fatalf("提交的链接不对: %+v", c.uris)
	}
	// 目录必须来自订阅指定的库，否则自动整理那条链对不上
	if c.cid != "777" {
		t.Fatalf("提交目录应是库的 remote_cid(777)，实际 %q", c.cid)
	}
	if c.label != "某剧 S01E01-E02" {
		t.Fatalf("任务名不对: %q", c.label)
	}

	sub, _ = st.GetSubscription(ctx, subID)
	if sub.WatermarkSeason != 1 || sub.WatermarkEpisode != 2 {
		t.Fatalf("水位线应推进到 S01E02，实际 S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}
	if sub.State != store.SubWaiting {
		t.Fatalf("追平后状态应为 waiting，实际 %q", sub.State)
	}

	// 第二轮：已经追平（最新只到 E02），一条都不该再提交
	if _, err := svc.CheckOne(ctx, *sub); err != nil {
		t.Fatalf("第二轮检查失败: %v", err)
	}
	mu.Lock()
	n := len(calls)
	mu.Unlock()
	if n != 1 {
		t.Fatalf("追平后不该再提交，实际累计 %d 次", n)
	}
	if sub, _ = st.GetSubscription(ctx, subID); sub.LastResult == "" {
		t.Fatalf("追平也要写 last_result，否则页面上看不出它跑过")
	}
}

// 查到有新集但搜不到资源：这是最常见的情况，必须「不动水位线 + 短间隔重试 + 不算失败」。
func TestCheckTVNoSource(t *testing.T) {
	st := openSubStore(t)
	ctx := context.Background()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/7":
			_, _ = w.Write([]byte(`{"id":7,"name":"新剧","status":"Returning Series",
				"seasons":[{"season_number":1,"episode_count":5}],
				"last_episode_to_air":{"season_number":1,"episode_number":3}}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	submitted := 0
	svc := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{
		TMDb: func(context.Context) *tmdb.Client {
			c := tmdb.NewClient("k")
			c.Base = srv.URL
			return c
		},
		// 只有 720p 的资源，而订阅要 2160p
		Search: func(context.Context, string, string, string, int) (*resource.Result, error) {
			return &resource.Result{Items: []resource.Item{
				{Title: "新剧 S01E01 720p", Magnet: "magnet:?a"},
			}}, nil
		},
		Submit: func(context.Context, int64, []string, string, string) ([]string, error) {
			submitted++
			return nil, nil
		},
		Interval: func(context.Context) time.Duration { return 6 * time.Hour },
	})

	subID, _ := st.CreateSubscription(ctx, store.Subscription{
		TMDbID: 7, MediaType: "tv", Title: "新剧", Quality: "2160p",
	})
	sub, _ := st.GetSubscription(ctx, subID)

	msg, err := svc.CheckOne(ctx, *sub)
	if err != nil {
		t.Fatalf("没资源不该算错误: %v", err)
	}
	if submitted != 0 {
		t.Fatalf("画质不达标不该提交")
	}
	if msg == "" {
		t.Fatalf("要给出结论（搜到几条、缺什么）")
	}
	sub, _ = st.GetSubscription(ctx, subID)
	if sub.WatermarkEpisode != 0 {
		t.Fatalf("没提交成功就不能推进水位线，实际 S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}
	if sub.FailCount != 0 {
		t.Fatalf("「没资源」不是失败，fail_count 应保持 0，实际 %d", sub.FailCount)
	}
	// 重试间隔要比正常周期短得多（资源刚上线那几小时最值钱）
	if d := time.Until(sub.NextCheckAt); d > 15*time.Minute {
		t.Fatalf("没资源时应 10 分钟后重试，实际 %v 后", d)
	}
}

// 上游出错：要落 last_result + fail_count，并且按退避推到后面。
func TestCheckFailureBacksOff(t *testing.T) {
	st := openSubStore(t)
	ctx := context.Background()

	svc := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{
		TMDb: func(context.Context) *tmdb.Client { return nil }, // 没 Key
		Search: func(context.Context, string, string, string, int) (*resource.Result, error) {
			return nil, nil
		},
		Submit: func(context.Context, int64, []string, string, string) ([]string, error) {
			return nil, nil
		},
	})
	subID, _ := st.CreateSubscription(ctx, store.Subscription{TMDbID: 1, MediaType: "tv", Title: "x"})
	sub, _ := st.GetSubscription(ctx, subID)

	if _, err := svc.CheckOne(ctx, *sub); err == nil {
		t.Fatalf("没有 TMDB 客户端应该报错")
	}
	sub, _ = st.GetSubscription(ctx, subID)
	if sub.FailCount != 1 {
		t.Fatalf("失败应计数，实际 %d", sub.FailCount)
	}
	if sub.LastResult == "" {
		t.Fatalf("失败也要留痕，否则页面显示的还是上一次的成功结果")
	}
	if d := time.Until(sub.NextCheckAt); d < 50*time.Minute {
		t.Fatalf("首次失败应退避约 1 小时，实际 %v", d)
	}

	// 手动点「立即检查」正好撞上轮询时，第二次必须被挡住，
	// 而不是两个 goroutine 同时给同一集提交离线。
	if !svc.acquire(subID) {
		t.Fatalf("第一次 acquire 应成功")
	}
	if !svc.Busy(subID) {
		t.Fatalf("acquire 之后 Busy 应为 true")
	}
	if _, err := svc.CheckOne(ctx, *sub); !errors.Is(err, ErrBusy) {
		t.Fatalf("重复检查应返回 ErrBusy，实际 %v", err)
	}
	svc.release(subID)
	if svc.Busy(subID) {
		t.Fatalf("release 之后不该还是 busy")
	}
}

// target 的兜底链：订阅自己的目录 > 库的 remote_cid > 全局 res_save_cid > "0"。
// 少了全局那一档，没指定库的订阅会下到 115 根目录，自动整理根本看不见。
func TestTargetFallback(t *testing.T) {
	ctx := context.Background()
	st := openSubStore(t)
	svc := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)), Deps{})

	if _, cid := svc.target(ctx, store.Subscription{SaveCID: "12345"}); cid != "12345" {
		t.Fatalf("订阅上写了目录就该用它，实际 %q", cid)
	}

	if err := st.SetSetting(ctx, "res_save_cid", "9988"); err != nil {
		t.Fatal(err)
	}
	if _, cid := svc.target(ctx, store.Subscription{}); cid != "9988" {
		t.Fatalf("没指定库应兜到全局 res_save_cid（自动整理盯的目录），实际 %q", cid)
	}
	// 库被删了也不能悄悄下到根目录，还是走全局设置。
	if _, cid := svc.target(ctx, store.Subscription{LibraryID: 999}); cid != "9988" {
		t.Fatalf("库不存在时应退回全局设置，实际 %q", cid)
	}

	if err := st.SetSetting(ctx, "res_save_cid", ""); err != nil {
		t.Fatal(err)
	}
	if _, cid := svc.target(ctx, store.Subscription{}); cid != "0" {
		t.Fatalf("全局也没配时和网页链路一样落根目录，实际 %q", cid)
	}
}

func openSubStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "sub.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
