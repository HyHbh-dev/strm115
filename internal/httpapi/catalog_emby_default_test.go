package httpapi

// 「影视墙用 Emby 的数据」这条默认策略的回归测试：
// 接了媒体服务器就默认读 Emby，没接才退回 TMDB 富集。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/store"
)

// fakeEmbyForCatalog 起一个只实现「库列表 + 条目列表」的假 Emby。
func fakeEmbyForCatalog(t *testing.T, root string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/emby/Library/VirtualFolders", "/Library/VirtualFolders":
			_, _ = io.WriteString(w, fmt.Sprintf(
				`[{"Name":"电影","CollectionType":"movies","ItemId":"241","Locations":[%q]}]`, root))
		case "/emby/Items", "/Items":
			// 只要带了 ParentId 就认定「库已绑定」，返回一条该库里的电影。
			if r.URL.Query().Get("ParentId") != "241" {
				t.Errorf("没绑定 Emby 库时不该拿空 ParentId 查全库: %s", r.URL.RawQuery)
			}
			_, _ = io.WriteString(w, fmt.Sprintf(
				`{"TotalRecordCount":1,"Items":[{"Id":"437","Name":"F1：狂飙飞车","Type":"Movie",`+
					`"ProductionYear":2025,"Path":%q,"ImageTags":{"Primary":"tag1"}}]}`,
				root+"/F1：狂飙飞车 (2025)/F1.strm"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// 接了 Emby：不带 source 参数时应该直接用 Emby 出墙，並把库自动绑上。
func TestCatalogDefaultsToEmbyWhenConnected(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, root, _ := seedCatalogLib(t, st)

	emby := fakeEmbyForCatalog(t, root)
	for k, v := range map[string]string{
		"server_kind": "emby",
		"server_url":  emby.URL,
		"server_key":  "test-key",
	} {
		if err := st.SetSetting(ctx, k, v); err != nil {
			t.Fatal(err)
		}
	}

	out := getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID))
	if out.Source != "emby" {
		t.Fatalf("接了 Emby 时默认源应是 emby，实际 %s（hint=%s）", out.Source, out.Hint)
	}
	if len(out.Items) != 1 || out.Items[0].Name != "F1：狂飙飞车" {
		t.Fatalf("应有一张 Emby 卡片，实际 %+v", out.Items)
	}

	// 自动绑定要落库：下次查询直接用 ParentId，不再靠路径匹配。
	lib, err := st.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	if lib.ServerLibID != "241" {
		t.Fatalf("库应自动绑到 Emby 库 241，实际 %q", lib.ServerLibID)
	}
}

// 没接 Emby：默认仍然是 TMDB 富集，不能退化成裸台账（那样海报会全空）。
func TestCatalogFallsBackToTMDBWithoutEmby(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	ctx := context.Background()
	cookies := loginForTest(t, s.Handler())
	libID, _, _ := seedCatalogLib(t, st)
	_ = st.UpsertMedia(ctx, store.Media{
		LibraryID: libID, RelPath: "电影/沙丘2/Dune.mkv", Kind: "movie",
		Title: "沙丘2", MetaTitle: "沙丘2", Year: "2024", TMDbID: 693134, Status: "ok",
	})

	out := getCatalog(t, s.Handler(), cookies, "?library_id="+itoa(libID))
	if out.Source != "tmdb" {
		t.Fatalf("没接 Emby 时应退回 tmdb，实际 %s", out.Source)
	}
	if len(out.Items) != 1 || !strings.Contains(out.Items[0].Name, "沙丘") {
		t.Fatalf("卡片异常: %+v", out.Items)
	}
}
