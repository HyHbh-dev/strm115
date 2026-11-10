package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"strm115/internal/store"
)

// 详情接口是「所有页面点片名」的共同出口，所以它必须能把三处数据拼齐：
// TMDB 的展示字段、本地台账的入库状态、订阅表的水位线。少任何一块，
// 抽屉里就会出现「已入库却说没入库」或者「订了却给个订阅按钮」这种骗人的界面。
func TestTitleDetailJoinsLocalAndSubscription(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)
	ctx := context.Background()

	// ① 台账：这部剧已经入库、字幕是下载来的中文（不是机翻）。
	//    media_meta 和 libraries 都有外键，得先把账号和库建出来。
	acct, err := st.CreateAccount(ctx, "115", "测试账号", "cookie")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "剧集", RemoteCID: "0", LocalPath: "/media/tv",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertMedia(ctx, store.Media{
		LibraryID: 1,
		RelPath:   "Cyberpunk/Edge.S01E01.strm",
		Kind:      "series",
		Title:     "边缘行者",
		MetaTitle: "赛博朋克：边缘行者",
		Year:      "2022",
		MetaYear:  "2022",
		TMDbID:    94605,
		Runtime:   "25 min",
		SubState:  "ok",
		SubOrigin: "zh",
	}); err != nil {
		t.Fatal(err)
	}

	// ② 订阅：用 POST 建（服务端会拿 TMDB 校正标题/集数）并把起播线设在
	//    S01E05，再把它拨到 S02E08 —— 一条线可以被反复重设（前进或后退）。
	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "前端乱填的名字", "quality": "1080p",
		"watermark_season": 1, "watermark_episode": 5,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 2, "watermark_episode": 8,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("改水位线失败 %d: %s", rec.Code, rec.Body.String())
	}

	// ③ 按 tmdb_id 查详情：订阅页、播出日历点片名走的就是这条。
	rec, _ = doJSON(t, h, http.MethodGet, "/api/title?type=tv&tmdb_id=94605", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Type     string `json:"type"`
		TMDBID   int    `json:"tmdb_id"`
		Title    string `json:"title"`
		Original string `json:"original"`
		Local    *struct {
			Exists      bool   `json:"exists"`
			Episodes    int    `json:"episodes"`
			SubState    string `json:"sub_state"`
			SubOrigin   string `json:"sub_origin"`
			RelPath     string `json:"rel_path"`
			RemoteCID   string `json:"remote_cid"`
			LocalRoot   string `json:"local_root"`
			LocalDir    string `json:"local_dir"`
			LocalRelDir string `json:"local_rel_dir"`
		} `json:"local"`
		Sub *struct {
			ID               int64  `json:"id"`
			State            string `json:"state"`
			WatermarkSeason  int    `json:"watermark_season"`
			WatermarkEpisode int    `json:"watermark_episode"`
			TotalEpisodes    int    `json:"total_episodes"`
		} `json:"sub"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "tv" || d.TMDBID != 94605 {
		t.Fatalf("类型/ID 不对: %+v", d)
	}
	if d.Title != "赛博朋克：边缘行者" || d.Original != "Cyberpunk: Edgerunners" {
		t.Fatalf("展示字段应以 TMDB 为准: title=%q original=%q", d.Title, d.Original)
	}
	if d.Local == nil || !d.Local.Exists {
		t.Fatalf("已入库的行没被认出来，local=%+v", d.Local)
	}
	if d.Local.Episodes != 1 || d.Local.SubState != "ok" || d.Local.SubOrigin != "zh" {
		t.Fatalf("本地状态漏了: %+v", d.Local)
	}
	// 目录跳转：库信息从 library_id 关联出来，路径要按 ResolveRoot 修正。
	if d.Local.RemoteCID != "0" || d.Local.LocalRoot == "" ||
		d.Local.LocalRelDir != "Cyberpunk" || d.Local.LocalDir == "" {
		t.Fatalf("详情没把网盘/本地目录信息带出来: %+v", d.Local)
	}
	if d.Sub == nil || d.Sub.WatermarkSeason != 2 || d.Sub.WatermarkEpisode != 8 {
		t.Fatalf("订阅状态/水位线没带出来: %+v", d.Sub)
	}
	if d.Sub.TotalEpisodes != 10 {
		t.Fatalf("总集数应来自 TMDB: %+v", d.Sub)
	}

	// ④ 按 media_id 查：首页「最近入库」的卡片只有台账 id，没有 tmdb_id。
	rec, _ = doJSON(t, h, http.MethodGet, "/api/title?media_id=1", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("media_id 详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var byMedia struct {
		TMDBID int `json:"tmdb_id"`
		Local  *struct {
			Exists bool `json:"exists"`
		} `json:"local"`
	}
	json.Unmarshal(rec.Body.Bytes(), &byMedia)
	if byMedia.TMDBID != 94605 || byMedia.Local == nil || !byMedia.Local.Exists {
		t.Fatalf("media_id 应能反推出 tmdb_id 与本地状态: %s", rec.Body.String())
	}
}

// 什么标识都没给时不能「猜一部片出来」返回 200 —— 那会让前端以为点开了详情。
func TestTitleDetailNeedsIdentifier(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	for _, path := range []string{
		"/api/title",
		"/api/title?type=tv&q=绝不可能存在的片名ABCDEFG&year=1999",
	} {
		rec, _ := doJSON(t, h, http.MethodGet, path, nil, cookies)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s 应回 400，实际 %d: %s", path, rec.Code, rec.Body.String())
		}
	}
}

// 水位线是「用户手动拨进度」的入口，语义是强制设置而不是只许前进：
// 补不到的老集必须能跳过去。但两个字段要成对给，负数要挡住。
func TestSubscriptionWatermarkPUT(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "边缘行者",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}

	read := func() (int, int) {
		sub, err := st.GetSubscription(context.Background(), 1)
		if err != nil || sub == nil {
			t.Fatalf("读订阅失败: %v", err)
		}
		return sub.WatermarkSeason, sub.WatermarkEpisode
	}

	// 前进
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 2, "watermark_episode": 8,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("前进失败 %d: %s", rec.Code, rec.Body.String())
	}
	if season, ep := read(); season != 2 || ep != 8 {
		t.Fatalf("前进没生效: S%dE%d", season, ep)
	}

	// 后退：跳过 S01E03 那种补不到的老集，靠的就是这一步。
	// 老的 SetSubscriptionWatermark 带单调条件，会把这次静默丢掉。
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 1, "watermark_episode": 3,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("后退失败 %d: %s", rec.Code, rec.Body.String())
	}
	if season, ep := read(); season != 1 || ep != 3 {
		t.Fatalf("后退没生效（被单调条件吃掉了？）: S%dE%d", season, ep)
	}

	// 非法入参
	for _, body := range []map[string]any{
		{"watermark_season": 2},
		{"watermark_episode": 8},
		{"watermark_season": -1, "watermark_episode": 8},
		{"watermark_season": 2, "watermark_episode": -1},
	} {
		rec, _ := doJSON(t, h, http.MethodPut, "/api/subscriptions/1", body, cookies)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v 应回 400，实际 %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	if season, ep := read(); season != 1 || ep != 3 {
		t.Fatalf("非法请求不该改动水位线: S%dE%d", season, ep)
	}
}

// 起播线（原水位线）是「这一集及之前不下载」的那条线：可前进可后退、可清零。
// 两条线合并后，界面上的滑杆改的就是它。
func TestSubscriptionStartLinePUT(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTMDB(t, s, st)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodPost, "/api/subscriptions", map[string]any{
		"tmdb_id": 94605, "media_type": "tv", "title": "边缘行者",
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("建订阅失败 %d: %s", rec.Code, rec.Body.String())
	}
	read := func() (int, int) {
		sub, err := st.GetSubscription(context.Background(), 1)
		if err != nil || sub == nil {
			t.Fatalf("读订阅失败: %v", err)
		}
		return sub.WatermarkSeason, sub.WatermarkEpisode
	}
	if ws, we := read(); ws != 0 || we != 0 {
		t.Fatalf("新订阅默认应从头开始: S%dE%d", ws, we)
	}

	// 设到 S02E03：之后的集才下载
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 2, "watermark_episode": 3,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if ws, we := read(); ws != 2 || we != 3 {
		t.Fatalf("起播线没生效: S%dE%d", ws, we)
	}

	// 往回拨：用户要补前面几集
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 1, "watermark_episode": 1,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("回拨失败 %d: %s", rec.Code, rec.Body.String())
	}
	if ws, we := read(); ws != 1 || we != 1 {
		t.Fatalf("回拨没生效: S%dE%d", ws, we)
	}

	// 非法：只给一半 / 负数 / 越界，且不能改坏已有值
	for _, body := range []map[string]any{
		{"watermark_season": 2},
		{"watermark_episode": 8},
		{"watermark_season": -1, "watermark_episode": 8},
		{"watermark_season": 2, "watermark_episode": 1000},
		{"watermark_season": 100, "watermark_episode": 1},
	} {
		rec, _ := doJSON(t, h, http.MethodPut, "/api/subscriptions/1", body, cookies)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%v 应回 400，实际 %d: %s", body, rec.Code, rec.Body.String())
		}
	}
	if ws, we := read(); ws != 1 || we != 1 {
		t.Fatalf("非法请求不该改动起播线: S%dE%d", ws, we)
	}

	// 改一条不存在的订阅不能回 ok
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/999999", map[string]any{
		"watermark_season": 1, "watermark_episode": 1,
	}, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的订阅应回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 清零 = 从头开始
	rec, _ = doJSON(t, h, http.MethodPut, "/api/subscriptions/1", map[string]any{
		"watermark_season": 0, "watermark_episode": 0,
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("清零失败 %d: %s", rec.Code, rec.Body.String())
	}
	if ws, we := read(); ws != 0 || we != 0 {
		t.Fatalf("清零没生效: S%dE%d", ws, we)
	}
}
