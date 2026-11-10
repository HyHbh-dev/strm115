package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"strm115/internal/store"
)

// 总览面板的数字必须都对得上：台账条数、体积、刮削成功数、最新入库的剧集合并。
// 这里用真实 SQLite 造一份「1 部电影 + 1 部 3 集（跨两季）的剧 + 1 条没匹配上」的库。
func TestDashboardAggregates(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	root := t.TempDir()

	acctID, err := st.CreateAccount(ctx, "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100",
		LocalPath: root, CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	err = st.UpsertFiles(ctx, acctID, []store.File{
		{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "电影/空枪/movie.strm", Name: "空枪.mkv", Size: 2 << 30},
		{LibraryID: libID, FileID: "f2", PickCode: "p2", RelPath: "剧/漫长的季节/Season 1/ep1.strm", Name: "ep1.mkv", Size: 1 << 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.MarkLibrarySynced(ctx, libID, time.Now(), 2); err != nil {
		t.Fatal(err)
	}

	// 海报是刮削真写到磁盘上的文件：总览只在文件真的存在时才给图片地址。
	writeArt := func(rel string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("jpeg"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeArt("电影/空枪/poster.jpg")
	writeArt("剧/漫长的季节/poster.jpg")

	now := time.Now()
	put := func(m store.Media) {
		t.Helper()
		m.LibraryID = libID
		m.UpdatedAt = now
		if err := st.UpsertMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	put(store.Media{RelPath: "电影/空枪/movie.strm", Kind: "movie", Title: "空枪", MetaTitle: "空枪", Year: "2026",
		MetaYear: "2026", Status: "ok", PosterFile: "电影/空枪/poster.jpg"})
	episodes := []string{
		"剧/漫长的季节/Season 1/ep1.strm",
		"剧/漫长的季节/Season 1/ep2.strm",
		"剧/漫长的季节/Season 2/ep3.strm",
	}
	for i, rel := range episodes {
		// 真实数据里单集行的 poster_file 是空的：图写在剧根，一季只下一次。
		put(store.Media{RelPath: rel, Kind: "series", Title: "漫长的季节", MetaTitle: "漫长的季节",
			Year: "2023", MetaYear: "2023", Status: "ok", Season: i/2 + 1, Episode: i%2 + 1})
	}
	put(store.Media{RelPath: "电影/没对上.strm", Kind: "movie", Title: "没对上", Status: "unmatched"})

	runID, err := st.StartSyncRun(ctx, libID, "电影", "full")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.FinishSyncRun(ctx, runID, "done", 2, 2, 0, 0, 0, "ok"); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	s.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口失败: %d %s", rec.Code, rec.Body.String())
	}

	var out struct {
		Totals struct {
			Libraries int   `json:"libraries"`
			Files     int   `json:"files"`
			Bytes     int64 `json:"bytes"`
			Scraped   int   `json:"scraped"`
			Unmatched int   `json:"unmatched"`
			Running   int   `json:"running"`
		} `json:"totals"`
		Libraries []dashLibrary `json:"libraries"`
		Recent    []dashRecentItem
		Jobs      []dashJob `json:"jobs"`
		Runs      []struct {
			Trigger string `json:"trigger"`
			Phase   string `json:"phase"`
		} `json:"runs"`
		LastFullSync string `json:"last_full_sync"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	if out.Totals.Libraries != 1 || out.Totals.Files != 2 {
		t.Errorf("库/台账数不对: %+v", out.Totals)
	}
	if want := int64(3 << 30); out.Totals.Bytes != want {
		t.Errorf("体积合计应为 %d，实际 %d", want, out.Totals.Bytes)
	}
	if out.Totals.Scraped != 4 || out.Totals.Unmatched != 1 {
		t.Errorf("刮削统计不对: %+v", out.Totals)
	}
	if out.Totals.Running != 0 || len(out.Jobs) != 0 {
		t.Errorf("没有任务在跑时不该报运行中: %+v", out.Jobs)
	}
	if out.Libraries[0].Status != "ok" {
		t.Errorf("同步过且有上次时间，状态应为 ok，实际 %q", out.Libraries[0].Status)
	}
	if out.LastFullSync == "" {
		t.Error("全量流水账的时间应该被带出来")
	}
	if len(out.Runs) != 1 || out.Runs[0].Trigger != "full" {
		t.Errorf("最近流水账不对: %+v", out.Runs)
	}

	// 3 集跨两季的同一部剧要合并成一张卡；电影单独一张。
	if len(out.Recent) != 2 {
		t.Fatalf("最新入库应为 2 张卡（1 电影 + 1 剧），实际 %d: %+v", len(out.Recent), out.Recent)
	}
	var series, movie *dashRecentItem
	for i := range out.Recent {
		switch out.Recent[i].Type {
		case "series":
			series = &out.Recent[i]
		case "movie":
			movie = &out.Recent[i]
		}
	}
	if series == nil || movie == nil {
		t.Fatalf("电影/剧集卡片缺一: %+v", out.Recent)
	}
	if series.Episodes != 3 {
		t.Errorf("跨两季的同一部剧应合并成 3 集，实际 %d", series.Episodes)
	}
	if series.Name != "漫长的季节" || series.Year != "2023" {
		t.Errorf("剧集卡片标题不对: %+v", series)
	}
	// 单集行没有 poster_file，但剧根有 poster.jpg —— 要能兜底成图片地址。
	if series.Poster == "" {
		t.Error("剧集海报应该按剧根兜底到 poster.jpg")
	}
	if movie.Name != "空枪" || movie.Poster == "" {
		t.Errorf("电影卡片应该有中文名和海报地址: %+v", movie)
	}
}

// 没配 Emby / 没配 Key 时状态徽章要如实说「没配」，而不是假装在线。
func TestDashboardServicesHonest(t *testing.T) {
	s, _ := newServerWithStore(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard", nil)
	s.handleDashboard(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("总览接口失败: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Services []dashService  `json:"services"`
		Emby     map[string]any `json:"emby"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Services) != 3 {
		t.Fatalf("应有 115 / IMDb / TMDB 三个徽章，实际 %d", len(out.Services))
	}
	if out.Services[0].Key != "115" || out.Services[0].State != "off" {
		t.Errorf("没有账号时 115 应为 off: %+v", out.Services[0])
	}
	if out.Services[1].State != "warn" || out.Services[2].State != "warn" {
		t.Errorf("没配 Key 时 IMDb/TMDB 应为 warn: %+v", out.Services[1:])
	}
	if out.Emby["configured"] != false {
		t.Errorf("没配 Emby 时 configured 应为 false: %+v", out.Emby)
	}
}
