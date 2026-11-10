package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"strm115/internal/store"
)

// seedCatalogLib 建一个带真实目录的库，返回库 id、磁盘根、账号 id。
func seedCatalogLib(t *testing.T, st *store.Store) (int64, string, int64) {
	t.Helper()
	ctx := context.Background()
	acct, err := st.CreateAccount(ctx, "115", "t", "{}")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "电影", RemoteCID: "0", RemotePath: "电影",
		LocalPath: root, CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return libID, root, acct
}

// TestCatalogLocalFallback 没配 Emby 时用本地台账出墙，剧集按剧根合并成一张卡片。
func TestCatalogLocalFallback(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)

	seed := []store.Media{
		{LibraryID: libID, RelPath: "电影/华语/空枪.2026/空枪.2026.strm", Kind: "movie",
			Title: "空枪", MetaTitle: "空枪", Year: "2026", IMDbID: "tt38775487",
			Genres: "犯罪, 剧情", PosterFile: "电影/华语/空枪.2026/poster.jpg", Status: "ok"},
		{LibraryID: libID, RelPath: "电视剧/日番/边缘行者.2022/Season 1/E01.strm", Kind: "series",
			Title: "边缘行者", MetaTitle: "赛博朋克：边缘行者", Season: 1, Episode: 1, IMDbID: "tt12590266", Status: "ok"},
		{LibraryID: libID, RelPath: "电视剧/日番/边缘行者.2022/Season 1/E02.strm", Kind: "series",
			Title: "边缘行者", MetaTitle: "赛博朋克：边缘行者", Season: 1, Episode: 2, IMDbID: "tt12590266", Status: "ok"},
	}
	for _, m := range seed {
		if err := st.UpsertMedia(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog?source=emby&library_id="+itoa(libID), nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Source string        `json:"source"`
		Total  int           `json:"total"`
		Items  []catalogCard `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Source != "local" {
		t.Fatalf("没配服务器时应回落到 local，实际 %s", out.Source)
	}
	if out.Total != 2 || len(out.Items) != 2 {
		t.Fatalf("电影 1 张 + 剧集 1 张 = 2，实际 total=%d items=%d", out.Total, len(out.Items))
	}
	var series catalogCard
	for _, c := range out.Items {
		if c.Type == "series" {
			series = c
		}
	}
	if series.Episodes != 2 {
		t.Fatalf("剧集应合并 2 集，实际 %d", series.Episodes)
	}

	// 详情抽屉里两集都要能列出来
	rec, _ = doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog/item?key="+series.Key, nil, cookies)
	var detail struct {
		Episodes []map[string]any `json:"episodes"`
	}
	json.Unmarshal(rec.Body.Bytes(), &detail)
	if len(detail.Episodes) != 2 {
		t.Fatalf("详情应列出 2 集，实际 %d", len(detail.Episodes))
	}
}

// TestCatalogValidation 非法 key / 不存在的条目要给出明确状态码。
func TestCatalogValidation(t *testing.T) {
	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())
	h := s.Handler()

	for _, tc := range []struct {
		method, url string
		body        any
		want        int
	}{
		{http.MethodGet, "/api/catalog/item?key=", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/catalog/item?key=zzz:1", nil, http.StatusBadRequest},
		{http.MethodGet, "/api/catalog/item?key=local:99999", nil, http.StatusNotFound},
		{http.MethodDelete, "/api/catalog/item?key=bad", nil, http.StatusBadRequest},
		{http.MethodPost, "/api/catalog/item", map[string]any{"key": "local:99999"}, http.StatusNotFound},
	} {
		rec, _ := doJSON(t, h, tc.method, tc.url, tc.body, cookies)
		if rec.Code != tc.want {
			t.Fatalf("%s %s 应 %d，实际 %d: %s", tc.method, tc.url, tc.want, rec.Code, rec.Body.String())
		}
	}
}

// newCatalogEmby 造一个最小 Emby：列表/详情/图片/删除，并记录被删的 id。
func newCatalogEmby(t *testing.T, itemPath string) (*httptest.Server, *[]string) {
	t.Helper()
	deleted := &[]string{}
	mux := http.NewServeMux()
	item := map[string]any{
		"Id": "121", "Name": "空枪", "Type": "Movie", "Path": itemPath,
		"ProductionYear": 2026, "CommunityRating": 7.3,
		"Overview": "一段简介", "Genres": []string{"犯罪", "剧情"},
		"ProviderIds": map[string]string{"Imdb": "tt38775487", "Tmdb": "1567937"},
		"ImageTags":   map[string]string{"Primary": "tag123"},
	}
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{"Items": []any{item}, "TotalRecordCount": 1})
	})
	mux.HandleFunc("GET /Items/121/Images/Primary", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	})
	mux.HandleFunc("DELETE /Items/", func(w http.ResponseWriter, r *http.Request) {
		*deleted = append(*deleted, strings.TrimPrefix(r.URL.Path, "/Items/"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /Items/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, deleted
}

// TestCatalogEmbyCRUD 从 Emby 列墙 → 图片代理 → 编辑 → 删除（只移索引）。
func TestCatalogEmbyCRUD(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, root, _ := seedCatalogLib(t, st)

	rel := "电影/华语/空枪.2026/空枪.2026.strm"
	full := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{full, strings.TrimSuffix(full, ".strm") + ".nfo",
		filepath.Join(filepath.Dir(full), "poster.jpg")} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = st.UpsertFiles(ctx, 1, []store.File{{
		LibraryID: libID, FileID: "fid-1", PickCode: "pc-1", RelPath: rel,
		Name: "空枪.2026.mkv", Size: 1 << 30,
	}})
	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: rel, Kind: "movie", Title: "空枪",
		MetaTitle: "空枪", IMDbID: "tt38775487", Genres: "犯罪, 剧情",
		NFOFile:    strings.TrimSuffix(rel, ".strm") + ".nfo",
		PosterFile: "电影/华语/空枪.2026/poster.jpg", Status: "ok",
	})

	emby, deleted := newCatalogEmby(t, filepath.ToSlash(full))
	_ = st.SetSetting(ctx, "server_kind", "emby")
	_ = st.SetSetting(ctx, "server_url", emby.URL)
	_ = st.SetSetting(ctx, "server_key", "test-key")

	// 1) 列表：Emby 源 + 台账富集 + 海报代理地址
	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog?source=emby&library_id="+itoa(libID), nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Source string        `json:"source"`
		Items  []catalogCard `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if list.Source != "emby" {
		t.Fatalf("配好 Emby 后应走 emby，实际 %s / %s", list.Source, rec.Body.String())
	}
	if len(list.Items) != 1 {
		t.Fatalf("应列出 1 张卡片，实际 %d: %s", len(list.Items), rec.Body.String())
	}
	card := list.Items[0]
	if card.Key != "emby:121" || !card.InLocal || card.MediaID == 0 || !card.NFO {
		t.Fatalf("卡片没和本地台账对上: %+v", card)
	}
	if !strings.HasPrefix(card.Poster, "/api/catalog/image?") {
		t.Fatalf("海报应走同源代理，实际 %q", card.Poster)
	}

	// 2) 图片代理：浏览器不接触 Emby 地址与 Key
	rec, _ = doJSON(t, s.Handler(), http.MethodGet,
		card.Poster, nil, cookies)
	if rec.Code != http.StatusOK || rec.Body.Len() != 4 {
		t.Fatalf("图片代理失败: %d / %d bytes", rec.Code, rec.Body.Len())
	}

	// 3) 改：重写 nfo 并回写缓存
	_ = st.PutMetaCache(ctx, "imdb|tt38775487", "tt38775487",
		`{"imdb_id":"tt38775487","title":"Empty Gun","title_cn":"空枪","year":"2026"}`)
	rec, _ = doJSON(t, s.Handler(), http.MethodPost, "/api/catalog/item", map[string]any{
		"key": card.Key, "title": "空枪（导演剪辑版）", "genres": []string{"犯罪", "悬疑"},
	}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("编辑应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var edited struct {
		Local map[string]any `json:"local"`
	}
	json.Unmarshal(rec.Body.Bytes(), &edited)
	if edited.Local["title"] != "空枪（导演剪辑版）" {
		t.Fatalf("标题没写进台账: %+v", edited.Local)
	}
	nfo, _ := os.ReadFile(strings.TrimSuffix(full, ".strm") + ".nfo")
	if !strings.Contains(string(nfo), "导演剪辑版") || !strings.Contains(string(nfo), "悬疑") {
		t.Fatalf("nfo 没有按编辑结果重写: %s", string(nfo))
	}
	_, cached, _, _ := st.GetMetaCache(ctx, "imdb|tt38775487")
	if !strings.Contains(cached, "导演剪辑版") {
		t.Fatalf("编辑结果没回写缓存，下次刮削会还原: %s", cached)
	}

	// 4) 删：只移索引，本地 .strm/元数据/台账与 Emby 条目都要清掉
	rec, _ = doJSON(t, s.Handler(), http.MethodDelete,
		"/api/catalog/item?key="+card.Key, nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("删除应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var del struct {
		EmbyRemoved  bool `json:"emby_removed"`
		RemovedFiles int  `json:"removed_files"`
		Purge        bool `json:"purge"`
	}
	json.Unmarshal(rec.Body.Bytes(), &del)
	if !del.EmbyRemoved || del.RemovedFiles < 3 || del.Purge {
		t.Fatalf("默认删除结果不对: %+v", del)
	}
	if len(*deleted) != 1 || (*deleted)[0] != "121" {
		t.Fatalf("Emby 条目没删: %v", *deleted)
	}
	if _, err := os.Stat(full); !os.IsNotExist(err) {
		t.Fatal("本地 .strm 没删掉")
	}
	if rows, _ := st.ListMediaAll(ctx, libID); len(rows) != 0 {
		t.Fatalf("刮削台账没清干净: %d 条", len(rows))
	}
	if rows, _, _ := st.ListFiles(ctx, libID, 10, 0); len(rows) != 0 {
		t.Fatalf("文件台账没清干净: %d 条", len(rows))
	}
}

// TestCatalogRelDir 添加片目时必须和同步用同一把尺子算相对路径，
// 否则同一个视频会在两个目录各生成一份 .strm，Emby 里变成两部重复的片。
func TestCatalogRelDir(t *testing.T) {
	lib := store.Library{RemotePath: "电影"}
	for _, tc := range []struct {
		name string
		req  catalogPickReq
		want string
	}{
		{"剥掉库根前缀", catalogPickReq{RemotePath: "/电影/2026/空枪"}, "2026/空枪"},
		{"选中库根本身", catalogPickReq{RemotePath: "/电影"}, ""},
		{"显式子目录优先", catalogPickReq{RemotePath: "/电影/2026", RelDir: "自定/目录"}, "自定/目录"},
		{"选了库外目录只用末级", catalogPickReq{RemotePath: "/剧集/边缘行者"}, "边缘行者"},
		{"根目录不产生子目录", catalogPickReq{RemotePath: "115 根目录"}, ""},
		{"路径穿越要被清掉", catalogPickReq{RelDir: "../../etc/片"}, "etc/片"},
		{"反斜杠与空格", catalogPickReq{RemotePath: ` /电影\华语 /空枪 `}, "华语/空枪"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := catalogRelDir(lib, tc.req); got != tc.want {
				t.Fatalf("want %q, got %q", tc.want, got)
			}
		})
	}
}

// TestCatalogSeriesEnrichment Emby 剧集条目指向目录、台账按分集存，
// 列表与详情都必须按目录前缀把本地状态补回来。
func TestCatalogSeriesEnrichment(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, root, _ := seedCatalogLib(t, st)

	showDir := "电视剧/日番/边缘行者.2022"
	for i := 1; i <= 3; i++ {
		_ = st.UpsertMedia(ctx, store.Media{
			LibraryID: libID,
			RelPath:   fmt.Sprintf("%s/Season 1/E%02d.strm", showDir, i),
			Kind:      "series", Title: "边缘行者", MetaTitle: "赛博朋克：边缘行者",
			Season: 1, Episode: i,
			NFOFile:    fmt.Sprintf("%s/Season 1/E%02d.nfo", showDir, i),
			PosterFile: showDir + "/poster.jpg",
			SubState:   "ok", Status: "ok",
		})
	}

	embyPath := filepath.ToSlash(filepath.Join(root, filepath.FromSlash(showDir)))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /Items", func(w http.ResponseWriter, r *http.Request) {
		writeTestJSON(w, map[string]any{
			"Items": []any{map[string]any{
				"Id": "200", "Name": "赛博朋克：边缘行者", "Type": "Series",
				"Path": embyPath, "RecursiveItemCount": 3,
				"ProviderIds": map[string]string{"Imdb": "tt12590266"},
			}},
			"TotalRecordCount": 1,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	_ = st.SetSetting(ctx, "server_kind", "emby")
	_ = st.SetSetting(ctx, "server_url", srv.URL)
	_ = st.SetSetting(ctx, "server_key", "k")

	rec, _ := doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog?source=emby&library_id="+itoa(libID), nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("列表应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Items []catalogCard `json:"items"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Items) != 1 {
		t.Fatalf("应有 1 张剧集卡片，实际 %d: %s", len(list.Items), rec.Body.String())
	}
	c := list.Items[0]
	if !c.InLocal || !c.NFO || c.Episodes != 3 || c.SubState != "ok" {
		t.Fatalf("剧集卡片没按目录前缀补上台账: %+v", c)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodGet,
		"/api/catalog/item?key="+c.Key, nil, cookies)
	var det struct {
		Local map[string]any `json:"local"`
	}
	json.Unmarshal(rec.Body.Bytes(), &det)
	if det.Local["exists"] != true {
		t.Fatalf("剧集详情缺本地台账: %+v / %s", det.Local, rec.Body.String())
	}
}
