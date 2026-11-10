package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// withFakeTVSeasons 给「专有日历」测试用的假 TMDB：两季各两集，
// 一集已播（2020）、一集未播（2099），外加季清单与 external_ids。
func withFakeTVSeasons(t *testing.T, s *Server, st *store.Store) {
	t.Helper()
	day := func(n int) string { return time.Now().AddDate(0, 0, n).Format("2006-01-02") }
	past1, past2 := day(-10), day(-3)
	future1, future2 := day(20), day(27)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/94605":
			_, _ = w.Write([]byte(`{"id":94605,"name":"测试剧","original_name":"Test Show",
				"first_air_date":"` + past1 + `","status":"Ended","number_of_episodes":4,
				"seasons":[{"season_number":1,"episode_count":2},{"season_number":2,"episode_count":2}],
				"last_episode_to_air":{"season_number":2,"episode_number":2}}`))
		case "/tv/94605/season/1":
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":1,"name":"第一集","air_date":"` + past1 + `"},
				{"episode_number":2,"name":"第二集","air_date":"` + past2 + `"}]}`))
		case "/tv/94605/season/2":
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":1,"name":"第三集","air_date":"` + future1 + `"},
				{"episode_number":2,"name":"第四集","air_date":"` + future2 + `"}]}`))
		case "/tv/94605/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt12590266"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if err := st.SetSetting(context.Background(), "tmdb_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	s.applyTMDBUpstream(context.Background())
	t.Cleanup(func() { tmdb.SetUpstream("", "") })
}

// TestSubscriptionCalendarMaterialized 验证「建订阅 → 按 TMDB 集数集合物化专有日历
// → 起播线之前的集跳过、起播线本身保留 → 播出日决定状态」这条主干。
func TestSubscriptionCalendarMaterialized(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTVSeasons(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "测试剧",
		"watermark_season": 1, "watermark_episode": 2,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID            int64 `json:"id"`
		CalendarTotal int   `json:"calendar_total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.CalendarTotal != 4 {
		t.Fatalf("应按 TMDB 生成 4 集，实际 %d（%s）", created.CalendarTotal, rec.Body.String())
	}

	eps, err := st.ListSubscriptionEpisodes(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) != 4 {
		t.Fatalf("日历应有 4 行，实际 %d", len(eps))
	}
	want := map[string]string{
		"S01E01": store.SubEpSkipped, // 起播线之前：不搜
		"S01E02": store.SubEpWaiting, // 起播线本身（含），已播、待搜
		"S02E01": store.SubEpPending, // 2099 年才播
		"S02E02": store.SubEpPending,
	}
	for _, e := range eps {
		if got, ok := want[e.Code()]; !ok {
			t.Errorf("多出来的集 %s", e.Code())
		} else if e.Status != got {
			t.Errorf("%s 状态 = %s，期望 %s", e.Code(), e.Status, got)
		}
	}
	// 播出日必须真的来自 TMDB 的单集接口，不是季的 air_date。
	if eps[0].AirDate == "" || eps[2].AirDate == "" {
		t.Fatalf("单集 air_date 没写对：%+v", eps)
	}

	// 专有日历接口：范围内只回有日期的集，且不含 skipped。
	start := time.Now().AddDate(0, 0, -30).Format("2006-01-02")
	rec, _ = doJSON(t, h, http.MethodGet,
		"/api/subscriptions/calendar?start="+start+"&days=62", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("专有日历失败 %d: %s", rec.Code, rec.Body.String())
	}
	var cal struct {
		Items   []store.SubEpisode `json:"items"`
		Undated []store.SubEpisode `json:"undated"`
		Total   int                `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &cal); err != nil {
		t.Fatal(err)
	}
	if cal.Total != 3 {
		t.Fatalf("应回 3 集（4 集里只跳过 S01E01），实际 %d", cal.Total)
	}
	for _, e := range cal.Items {
		if e.Title != "测试剧" {
			t.Errorf("日历条目没带片名：%+v", e)
		}
		if e.Status == store.SubEpSkipped {
			t.Errorf("跳过的集不该进日历：%s", e.Code())
		}
	}

	// 选起始集的接口：季清单 + 指定季的逐集（含 air_date）。
	rec, _ = doJSON(t, h, http.MethodGet,
		"/api/subscriptions/episodes?tmdb_id=94605&season=1", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("集数集合失败 %d: %s", rec.Code, rec.Body.String())
	}
	var epResp struct {
		Seasons  []tmdb.TVSeason      `json:"seasons"`
		Episodes []tmdb.SeasonEpisode `json:"episodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &epResp); err != nil {
		t.Fatal(err)
	}
	if len(epResp.Seasons) != 2 || len(epResp.Episodes) != 2 {
		t.Fatalf("季/集集合不对：%d 季 %d 集", len(epResp.Seasons), len(epResp.Episodes))
	}
	if epResp.Episodes[0].AirDate == "" {
		t.Fatalf("逐集没带播出日：%+v", epResp.Episodes)
	}
}

// TestDueCalendarEpisodesDailyRetry 验证「只搜到期的集」和「没搜到每天重试一次」。
func TestDueCalendarEpisodesDailyRetry(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	subID, err := st.CreateSubscription(ctx, store.Subscription{
		TMDbID: 94605, MediaType: "tv", Title: "测试剧", State: store.SubWatching,
	})
	if err != nil {
		t.Fatal(err)
	}
	today := time.Now().Format("2006-01-02")
	eps := []store.SubEpisode{
		{Season: 1, Episode: 1, AirDate: "2020-01-01", Status: store.SubEpWaiting}, // 到期
		{Season: 1, Episode: 2, AirDate: "2099-01-01", Status: store.SubEpPending}, // 未播
		{Season: 1, Episode: 3, AirDate: "", Status: store.SubEpWaiting},           // 未定档也算到期
		{Season: 1, Episode: 4, AirDate: today, Status: store.SubEpDone},           // 已下载
		{Season: 1, Episode: 5, AirDate: "2020-01-02", Status: store.SubEpSkipped}, // 起播线之前
	}
	if err := st.UpsertSubscriptionEpisodes(ctx, subID, eps); err != nil {
		t.Fatal(err)
	}

	due, err := st.DueCalendarEpisodes(ctx, time.Now(), 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("应该只有 2 集到期（已播待搜 + 未定档），实际 %d：%+v", len(due), due)
	}
	if due[0].Sub.ID != subID || due[0].Sub.Title != "测试剧" {
		t.Fatalf("到期集没带上订阅信息：%+v", due[0])
	}

	// 没搜到 → 排到 24 小时后；这一轮不该再被捞出来。
	next := time.Now().Add(24 * time.Hour)
	if err := st.MarkSubEpisodeMissed(ctx, due[0].Ep.ID, next, "没搜到 S01E01 的资源"); err != nil {
		t.Fatal(err)
	}
	soon, err := st.DueCalendarEpisodes(ctx, time.Now().Add(time.Hour), 20)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range soon {
		if d.Ep.ID == due[0].Ep.ID {
			t.Fatal("没搜到的集在 24 小时内又被捞出来了，每天重试一次没生效")
		}
	}
	tomorrow, err := st.DueCalendarEpisodes(ctx, next.Add(time.Minute), 20)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range tomorrow {
		if d.Ep.ID == due[0].Ep.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("24 小时后这一集应该重新到期")
	}

	// 搜到并提交 → done，且刷新 TMDB 集数时不能被重置回待搜。
	if err := st.MarkSubEpisodeDone(ctx, due[0].Ep.ID, "已提交"); err != nil {
		t.Fatal(err)
	}
	refresh := []store.SubEpisode{
		{Season: 1, Episode: 1, AirDate: "2020-01-01", Status: store.SubEpWaiting},
		{Season: 1, Episode: 6, AirDate: "2099-02-01", Status: store.SubEpPending},
	}
	if err := st.UpsertSubscriptionEpisodes(ctx, subID, refresh); err != nil {
		t.Fatal(err)
	}
	all, err := st.ListSubscriptionEpisodes(ctx, subID)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 6 {
		t.Fatalf("刷新后应是 6 集（5 + 新增 1），实际 %d", len(all))
	}
	var refreshed *store.SubEpisode
	for i := range all {
		if all[i].ID == due[0].Ep.ID {
			refreshed = &all[i]
		}
	}
	if refreshed == nil || refreshed.Status != store.SubEpDone || refreshed.FoundAt == "" {
		t.Fatalf("刷新把已下载的集（id=%d）重置了：%+v", due[0].Ep.ID, all)
	}
	after, err := st.HasSubscriptionCalendar(ctx, subID)
	if err != nil || !after {
		t.Fatalf("专有日历应存在，has=%v err=%v", after, err)
	}
	_ = s // 保留 newServerWithStore 建出来的 Server，路由注册由其它用例覆盖
}

// TestCalendarRefreshUpdatesTotal 盯住「TMDB 后来加集，总集数要跟着涨」。
//
// 只补日历行、不更新 total_episodes 的话，下载是对的，但影视详情里的
// 共 N 集和起播线滑杆会一直停在建订阅时的数字，用户会以为订阅没跟上。
func TestCalendarRefreshUpdatesTotal(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTVSeasons(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)
	ctx := context.Background()

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "测试剧",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}

	// 模拟「建订阅时 TMDB 只有 2 集」的历史数据，再让它刷新一次。
	if err := st.SetSubscriptionTotal(ctx, created.ID, 2); err != nil {
		t.Fatal(err)
	}
	rec, _ = doJSON(t, h, http.MethodPost,
		"/api/subscriptions/"+strconv.FormatInt(created.ID, 10)+"/calendar/refresh", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("刷新日历失败 %d: %s", rec.Code, rec.Body.String())
	}
	sub, err := st.GetSubscription(ctx, created.ID)
	if err != nil || sub == nil {
		t.Fatalf("读订阅失败：%v", err)
	}
	if sub.TotalEpisodes != 4 {
		t.Fatalf("刷新后总集数应为 4，实际 %d", sub.TotalEpisodes)
	}
}
