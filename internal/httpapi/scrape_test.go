package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"strm115/internal/store"
)

// TestScrapeRequiresSyncedLibrary 没同步过的库不能刮削。
//
// 这不是「为难而难」：刮削读的是 files 台账，空台账会得到「0 条」，
// 用户只会看到任务瞬间完成、什么都没发生，然后来问是不是坏了。
func TestScrapeRequiresSyncedLibrary(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "空库", RemoteCID: "0",
		LocalPath: t.TempDir(), CollectionType: "movies", KeepExt: true,
	})

	rec, _ := doJSON(t, s.Handler(), http.MethodPost,
		"/api/libraries/"+itoa(libID)+"/scrape", map[string]any{}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

func TestListMediaEmpty(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "库", RemoteCID: "0",
		LocalPath: t.TempDir(), CollectionType: "movies", KeepExt: true,
	})

	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/libraries/"+itoa(libID)+"/media", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Items []store.Media  `json:"items"`
		Total int            `json:"total"`
		Stats map[string]int `json:"stats"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Total != 0 || len(out.Items) != 0 {
		t.Fatalf("空库应返回空列表，实际 %+v", out)
	}
}

// TestSubtitleNeedsIMDbID 没有 IMDb ID 的条目不能补字幕，且要给明确原因。
func TestSubtitleNeedsIMDbID(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "库", RemoteCID: "0",
		LocalPath: t.TempDir(), CollectionType: "movies", KeepExt: true,
	})
	if err := st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/x.mkv", Kind: "movie",
		Title: "x", Status: "unmatched",
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListMediaAll(ctx, libID)
	if len(all) != 1 {
		t.Fatal("准备数据失败")
	}

	rec, _ := doJSON(t, s.Handler(), http.MethodPost,
		"/api/media/"+itoa(all[0].ID)+"/subtitle", map[string]any{}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// TestMediaImagePathTraversal 不能通过 kind/路径读到媒体库以外的文件。
func TestMediaImagePathTraversal(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	root := t.TempDir()
	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "库", RemoteCID: "0",
		LocalPath: root, CollectionType: "movies", KeepExt: true,
	})
	// 库外放一个「机密」文件，试着越过边界去读它
	secret := filepath.Join(filepath.Dir(root), "secret.txt")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o644)
	t.Cleanup(func() { os.Remove(secret) })

	if err := st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "a.mkv", Kind: "movie", Title: "a", Status: "ok",
		IMDbID: "tt1", PosterFile: "../secret.txt",
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListMediaAll(ctx, libID)

	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/media/"+itoa(all[0].ID)+"/image", nil, cookies)
	if rec.Code == http.StatusOK {
		t.Fatalf("路径穿越居然读到了文件: %s", rec.Body.String())
	}
}

// TestSettingsExposeScrapeConfig 设置接口要把 JSON 配置解成对象给前端。
func TestSettingsExposeScrapeConfig(t *testing.T) {
	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodGet, "/api/settings", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("读取设置失败 %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"scrape", "subtitle"} {
		obj, ok := out[k].(map[string]any)
		if !ok {
			t.Fatalf("%s 应是对象，实际 %T", k, out[k])
		}
		if len(obj) == 0 {
			t.Fatalf("%s 是空对象", k)
		}
	}
	sc := out["scrape"].(map[string]any)
	if sc["rate_ms"] == nil {
		t.Error("scrape.rate_ms 缺失")
	}
}

// TestPutSettingsScrapeConfig 保存设置后要能原样读回来。
func TestPutSettingsScrapeConfig(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	body := map[string]any{
		"scrape_config":   `{"write_nfo":true,"fetch_images":false,"min_size_mb":80,"rate_ms":900,"auto":true}`,
		"subtitle_config": `{"auto":true,"translate":false,"bilingual":true,"api_base":"http://127.0.0.1:9999/v1","model":"m","workers":3}`,
	}
	rec, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/settings", body, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}

	sc := s.loadScrapeConfig(ctx)
	if !sc.Auto || sc.FetchImages || sc.MinSizeMB != 80 || sc.RateMS != 900 {
		t.Fatalf("刮削配置没存对: %+v", sc)
	}
	sb := s.loadSubtitleConfig(ctx)
	if !sb.Auto || sb.Translate || !sb.Bilingual || sb.Workers != 3 || sb.APIBase != "http://127.0.0.1:9999/v1" {
		t.Fatalf("字幕配置没存对: %+v", sb)
	}
	// 老配置里没有 refresh_server 这个键：不能因为反序列化把它变成 false，
	// 否则升级后字幕写盘就悄悄不再通知 Emby 了。
	if !sb.RefreshServer {
		t.Fatal("老字幕配置缺 refresh_server 时应默认开启刷新")
	}
	if err := st.SetSetting(ctx, settingSubtitleConfig, `{"auto":true,"refresh_server":false}`); err != nil {
		t.Fatal(err)
	}
	if s.loadSubtitleConfig(ctx).RefreshServer {
		t.Fatal("用户显式关掉刷新后不该被默认值覆盖")
	}

	// 坏 JSON 不能把服务带崩，应该退回默认值
	if err := st.SetSetting(ctx, settingScrapeConfig, "{不是 JSON"); err != nil {
		t.Fatal(err)
	}
	if got := s.loadScrapeConfig(ctx); got.RateMS != defaultScrapeConfig().RateMS {
		t.Fatalf("坏配置应回退默认值，实际 %+v", got)
	}
}

func itoa(v int64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// TestRescrapeMediaValidation 单条重刮的入参校验：没 ID 要明确拒绝，
// 记录不存在要 404 —— 不能让前端拿一个空 ID 去打上游。
func TestRescrapeMediaValidation(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())

	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "库", RemoteCID: "0",
		LocalPath: t.TempDir(), CollectionType: "movies", KeepExt: true,
	})
	if err := st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/x.mkv", Kind: "movie",
		Title: "x", Status: "unmatched",
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := st.ListMediaAll(ctx, libID)
	if len(all) != 1 {
		t.Fatal("准备数据失败")
	}

	// 没有 IMDb ID：400
	rec, _ := doJSON(t, s.Handler(), http.MethodPost,
		"/api/media/"+itoa(all[0].ID)+"/scrape", map[string]any{}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 ID 应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 记录的 ID 不存在：404
	rec, _ = doJSON(t, s.Handler(), http.MethodPost,
		"/api/media/999999/scrape", map[string]any{"imdb_id": "tt15239678"}, cookies)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的记录应返回 404，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 非法 ID（只有数字 / 太短）也要拒绝
	rec, _ = doJSON(t, s.Handler(), http.MethodPost,
		"/api/media/"+itoa(all[0].ID)+"/scrape", map[string]any{"imdb_id": "12345"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 ID 应返回 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}
