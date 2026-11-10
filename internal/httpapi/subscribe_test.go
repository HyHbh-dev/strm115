package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// withFakeTMDB 把 tmdb_base 指到一个假服务器。
//
// 必须做这一步：不拦的话测试会真的去打 api.themoviedb.org —— 慢、依赖外网、
// 还会因为限流时红时绿。源码不内置 Key 也一样要把上游指到假服务器。
func withFakeTMDB(t *testing.T, s *Server, st *store.Store) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/94605":
			// name 故意和前端传的不一样，用来验证「以 TMDB 为准」
			_, _ = w.Write([]byte(`{"id":94605,"name":"赛博朋克：边缘行者",
				"original_name":"Cyberpunk: Edgerunners","first_air_date":"2022-09-13",
				"status":"Ended","number_of_episodes":10,
				"seasons":[{"season_number":1,"episode_count":10}],
				"last_episode_to_air":{"season_number":1,"episode_number":10}}`))
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
	// 只写 settings 是不够的：上游是进程级配置，设置页保存后走的正是
	// 这一步 applyTMDBUpstream。少了它，请求会打到真 TMDB（94605 其实是
	// 《英雄联盟：双城之战》），测试就变成依赖外网且时红时绿。
	s.applyTMDBUpstream(context.Background())
	t.Cleanup(func() { tmdb.SetUpstream("", "") })
}

// 订阅接口的最小闭环：建、列、改、删。重复订阅必须 409 而不是静默多一行 ——
// 两条订阅抢同一个水位线，结果是同一集被提交两次。
func TestSubscriptionsCRUD(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	// 空库
	rec, _ := doJSON(t, h, http.MethodGet, "/api/subscriptions", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表失败 %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items   []map[string]any `json:"items"`
		Counts  map[string]int   `json:"counts"`
		Builtin bool             `json:"builtin_tmdb"`
		Every   int              `json:"interval_minutes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("空库不该有订阅，实际 %d 条", len(list.Items))
	}
	if list.Every != defaultSubInterval {
		t.Fatalf("默认周期应是 %d 分钟，实际 %d", defaultSubInterval, list.Every)
	}
	// 测试库没填自己的 Key，用的就是内置那把公共配额
	if !list.Builtin {
		t.Fatalf("没填 Key 时 builtin_tmdb 应为 true")
	}

	// 建一条：故意传错名字，看服务端会不会用 TMDB 的元数据纠正
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv",
		"title": "前端传错的名字", "quality": "1080p",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &created)
	if created.ID <= 0 {
		t.Fatalf("没返回 id: %s", rec.Body.String())
	}

	// 同一部剧再来一次：409
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "重复",
	}, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("重复订阅应回 409，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 列表里字段名要稳：前端按这些键渲染表格
	rec, _ = doJSON(t, h, http.MethodGet, "/api/subscriptions", nil, cookies)
	var raw struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &raw)
	if len(raw.Items) != 1 {
		t.Fatalf("应有 1 条，实际 %d", len(raw.Items))
	}
	for _, key := range []string{
		"id", "tmdb_id", "media_type", "title", "year", "quality", "state",
		"watermark_season", "watermark_episode", "total_episodes",
		"next_check_at", "last_result", "fail_count", "library_id",
	} {
		if _, ok := raw.Items[0][key]; !ok {
			t.Fatalf("订阅 JSON 缺字段 %q，前端会渲染成空白", key)
		}
	}
	// 标题、IMDb、年份、总集数必须来自 TMDB，不是前端传的那个
	var title, imdb string
	var total, year int
	json.Unmarshal(raw.Items[0]["title"], &title)
	json.Unmarshal(raw.Items[0]["imdb_id"], &imdb)
	json.Unmarshal(raw.Items[0]["total_episodes"], &total)
	json.Unmarshal(raw.Items[0]["year"], &year)
	if title != "赛博朋克：边缘行者" {
		t.Fatalf("标题应被 TMDB 纠正，实际 %q", title)
	}
	if imdb != "tt12590266" {
		t.Fatalf("IMDb 应从 TMDB 补齐，实际 %q", imdb)
	}
	if total != 10 || year != 2022 {
		t.Fatalf("总集数/年份不对: total=%d year=%d", total, year)
	}

	// 暂停
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1",
		map[string]any{"state": "paused"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("更新失败 %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/subscriptions?state=paused", nil, cookies)
	var paused struct {
		Items []struct {
			State string `json:"state"`
		} `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &paused)
	if len(paused.Items) != 1 || paused.Items[0].State != "paused" {
		t.Fatalf("按状态筛选没生效: %s", rec.Body.String())
	}

	// 非法状态要被挡住，错误信息得说清允许哪些值
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1",
		map[string]any{"state": "flying"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法状态应回 400，实际 %d", rec.Code)
	}

	// 删
	rec, _ = doJSON(t, h, http.MethodDelete, "/api/subscriptions/1", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除失败 %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = doJSON(t, h, http.MethodDelete, "/api/subscriptions/1", nil, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("重复删除应回 404，实际 %d", rec.Code)
	}
}

// 参数校验：缺 tmdb_id、缺片名、TMDB 查不到这部剧，各自给出能照做的提示。
func TestSubCreateValidation(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions",
		map[string]any{"media_type": "tv", "title": "x"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 tmdb_id 应回 400，实际 %d", rec.Code)
	}
	if got := errText(t, rec); got == "" {
		t.Fatalf("错误信息不能为空")
	}

	// 电影不需要 TMDB，缺片名必须挡住 —— 建一条没名字的订阅毫无意义
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions",
		map[string]any{"tmdb_id": 5, "media_type": "movie"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("电影缺片名应回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 剧集查不到集数：不让建。建一条判断不了集数的订阅只会永远停在 waiting。
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions",
		map[string]any{"tmdb_id": 999999, "media_type": "tv", "title": "不存在的剧"}, cookies)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("TMDB 查不到时应回 502，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 电影正常建
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions",
		map[string]any{"tmdb_id": 693134, "media_type": "movie", "title": "沙丘 2", "year": 2024}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("电影订阅应能建，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 周期设置的边界：太密对 TMDB 和 115 都不友好，必须有下限。
func TestSubConfigBounds(t *testing.T) {
	s, _ := newServerWithStore(t)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/subscriptions/config", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读配置失败 %d", rec.Code)
	}

	for _, bad := range []int{0, 5, 14, 8 * 24 * 60} {
		rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/config",
			map[string]any{"interval_minutes": bad}, cookies)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%d 分钟应被拒绝，实际 %d", bad, rec.Code)
		}
	}
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/config",
		map[string]any{"interval_minutes": 120}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("120 分钟应可保存，实际 %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = doJSON(t, h, http.MethodGet, "/api/subscriptions/config", nil, cookies)
	var cfg struct {
		Interval int `json:"interval_minutes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &cfg)
	if cfg.Interval != 120 {
		t.Fatalf("周期没落库，实际 %d", cfg.Interval)
	}
}

// 「立即检查」的两个前置错误：订阅不存在、TMDB 查不到剧。
// 都要快速失败，而不是挂住 3 分钟。
func TestSubRunErrors(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions/999/run", nil, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的订阅应回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 直接往库里插一条指向不存在剧集的订阅（绕过创建时的校验）
	if _, err := st.CreateSubscription(context.Background(), store.Subscription{
		TMDbID: 888888, MediaType: "tv", Title: "查不到的剧",
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ = doJSON(t, h, http.MethodPost, "/api/subscriptions/1/run", nil, cookies)
	if rec.Code == http.StatusOK {
		t.Fatalf("TMDB 查不到时不该成功: %s", rec.Body.String())
	}
	if got := errText(t, rec); got == "" {
		t.Fatalf("要给出原因")
	}
}

// errText 取错误响应里的那句话。接口的所有失败都走 writeErr，
// 结构固定是 {"error": "..."}；这里统一解析，省得每处都写一遍。
func errText(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("错误响应不是合法 JSON: %s", rec.Body.String())
	}
	return body.Error
}
