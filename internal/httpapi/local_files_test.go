package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"strm115/internal/store"
)

// newLocalFixture 造一个库：根目录里有一个正常目录（台账里有 .strm）、
// 一个广告壳目录（本地多出来的）和一个散落文件。
func newLocalFixture(t *testing.T) (*Server, *store.Store, int64, string, []*http.Cookie) {
	t.Helper()
	s, st := newServerWithStore(t)
	ctx := context.Background()
	acctID, err := st.CreateAccount(ctx, "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100", LocalPath: root,
		CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	write := func(rel string, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("空枪 (2026)/空枪.strm", "http://nas:6086/d/x?/x.mkv\n")
	write("广告壳/readme.txt", "ad")
	write("散落.strm", "http://nas:6086/d/y?/y.mkv\n")

	if err := st.UpsertFiles(ctx, acctID, []store.File{
		{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "空枪 (2026)/空枪.strm", Name: "空枪.mkv"},
	}); err != nil {
		t.Fatal(err)
	}
	return s, st, libID, root, loginForTest(t, s.Handler())
}

func TestLocalBrowseMarksOrphans(t *testing.T) {
	s, _, libID, _, cookies := newLocalFixture(t)
	h := s.Handler()

	rec, _ := doJSON(t, h, http.MethodGet,
		"/api/local/browse?library_id="+strconv.FormatInt(libID, 10), nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("列目录失败 %d: %s", rec.Code, rec.Body.String())
	}
	var got localBrowseResp
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Dirs != 2 || got.Files != 1 {
		t.Fatalf("根目录应有 2 个目录 + 1 个文件，实际 dirs=%d files=%d", got.Dirs, got.Files)
	}
	if got.Orphans != 1 {
		t.Fatalf("根目录应有 1 个孤儿文件，实际 %d", got.Orphans)
	}
	// 目录在前：排序是这一页的可用性前提，翻两层之后不该混着文件
	if !got.Entries[0].IsDir || !got.Entries[1].IsDir {
		t.Fatalf("目录应该排在文件前面：%+v", got.Entries)
	}

	// 进到「空枪 (2026)」：这一条 .strm 在台账里，不该被标成孤儿
	rec, _ = doJSON(t, h, http.MethodGet,
		"/api/local/browse?library_id="+strconv.FormatInt(libID, 10)+"&path="+url.QueryEscape("空枪 (2026)"), nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("进子目录失败 %d: %s", rec.Code, rec.Body.String())
	}
	got = localBrowseResp{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Entries) != 1 || !got.Entries[0].Known {
		t.Fatalf("台账里有的 .strm 应标 known：%+v", got.Entries)
	}
	if got.Orphans != 0 {
		t.Fatalf("子目录不该有孤儿，实际 %d", got.Orphans)
	}
	if len(got.Crumbs) != 2 || got.Crumbs[1].Path != "空枪 (2026)" {
		t.Fatalf("面包屑不对：%+v", got.Crumbs)
	}
}

// 路径穿越必须挡在业务之前：这一页是唯一能删宿主文件的地方。
func TestLocalBrowseRejectsTraversal(t *testing.T) {
	s, _, libID, _, cookies := newLocalFixture(t)
	h := s.Handler()
	for _, p := range []string{"../etc", "..%2F..%2Fetc", "/etc/passwd"} {
		rec, _ := doJSON(t, h, http.MethodGet,
			"/api/local/browse?library_id="+strconv.FormatInt(libID, 10)+"&path="+p, nil, cookies)
		if rec.Code == http.StatusOK {
			t.Errorf("path=%q 不该通过，实际 200: %s", p, rec.Body.String())
		}
	}
}

func TestLocalRemoveGuards(t *testing.T) {
	s, _, libID, root, cookies := newLocalFixture(t)
	h := s.Handler()

	// 1) 不能删库根
	rec, _ := doJSON(t, h, http.MethodPost, "/api/local/remove",
		map[string]any{"library_id": libID, "path": ""}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("删根目录应被拒绝，实际 %d: %s", rec.Code, rec.Body.String())
	}

	// 2) 非空目录不带 recursive 不能删
	rec, _ = doJSON(t, h, http.MethodPost, "/api/local/remove",
		map[string]any{"library_id": libID, "path": "广告壳"}, cookies)
	if rec.Code != http.StatusConflict {
		t.Fatalf("非空目录应要二次确认，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "广告壳")); err != nil {
		t.Fatal("被拒绝的删除不该动到文件")
	}

	// 3) 普通文件直接删；台账里没有的，回执不该提「会被同步写回来」
	rec, _ = doJSON(t, h, http.MethodPost, "/api/local/remove",
		map[string]any{"library_id": libID, "path": "散落.strm"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("删文件失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "散落.strm")); !os.IsNotExist(err) {
		t.Fatal("文件应该已经删掉")
	}

	// 4) 台账里有的文件：允许删，但要明确告诉用户下一轮同步会写回来
	rec, _ = doJSON(t, h, http.MethodPost, "/api/local/remove",
		map[string]any{"library_id": libID, "path": "空枪 (2026)/空枪.strm"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("删台账内的文件失败 %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if hint, _ := out["hint"].(string); hint == "" || !strings.Contains(hint, "下一轮同步") {
		t.Fatalf("台账内的文件删除后应提示会被同步写回，实际 %q", hint)
	}

	// 5) 带 recursive 才能删掉整个目录，回执给出真实文件数
	rec, _ = doJSON(t, h, http.MethodPost, "/api/local/remove",
		map[string]any{"library_id": libID, "path": "广告壳", "recursive": true}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("递归删除失败 %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "广告壳")); !os.IsNotExist(err) {
		t.Fatal("目录应该已经删掉")
	}
}

func TestLocalStatCountsBytesAndOrphans(t *testing.T) {
	s, _, libID, _, cookies := newLocalFixture(t)
	h := s.Handler()
	rec, _ := doJSON(t, h, http.MethodPost, "/api/local/stat",
		map[string]any{"library_id": libID}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("统计失败 %d: %s", rec.Code, rec.Body.String())
	}
	var st localStat
	if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
		t.Fatal(err)
	}
	if st.Files != 3 || st.Dirs != 2 {
		t.Fatalf("应有 3 文件 / 2 目录，实际 %d / %d", st.Files, st.Dirs)
	}
	// 台账只有 1 条，另外 2 个文件（广告壳/readme.txt、散落.strm）算孤儿
	if st.Orphans != 2 {
		t.Fatalf("孤儿数应为 2，实际 %d", st.Orphans)
	}
	if st.Bytes <= 0 {
		t.Fatalf("体积应大于 0，实际 %d", st.Bytes)
	}
}
