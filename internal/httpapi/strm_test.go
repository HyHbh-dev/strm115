package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"strm115/internal/store"
)

// setupOrphanLib 造一个库：盘上有 2 个孤儿 + 1 个正常 .strm，
// 台账里有 1 个正常 + 1 个「盘上已经没了」。
func setupOrphanLib(t *testing.T) (*Server, store.Library, string) {
	t.Helper()
	s, st := newServerWithStore(t)
	root := t.TempDir()

	acctID, err := st.CreateAccount(context.Background(), "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	libID, err := st.CreateLibrary(context.Background(), store.Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100",
		LocalPath: root, CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	write := func(rel string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("http://nas:6086/d/x?/x.mkv\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("电影/空枪/keep.strm")
	write("电影/空枪/gone.strm")
	write("孤儿.strm")

	err = st.UpsertFiles(context.Background(), acctID, []store.File{
		{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "电影/空枪/keep.strm", Name: "keep.mkv"},
		{LibraryID: libID, FileID: "f2", PickCode: "p2", RelPath: "电影/空枪/missing.strm", Name: "missing.mkv"},
	})
	if err != nil {
		t.Fatal(err)
	}

	lib, err := st.GetLibrary(context.Background(), libID)
	if err != nil {
		t.Fatal(err)
	}
	return s, lib, root
}

func TestScanOrphansBothDirections(t *testing.T) {
	s, lib, _ := setupOrphanLib(t)

	res := s.scanOrphans(context.Background(), lib, 0)
	if res.Err != "" {
		t.Fatalf("扫描出错: %s", res.Err)
	}
	if res.Scanned != 3 {
		t.Errorf("盘上应有 3 个 .strm，实际 %d", res.Scanned)
	}
	gotExtra := strings.Join(res.Extra, ",")
	if !strings.Contains(gotExtra, "孤儿.strm") || !strings.Contains(gotExtra, "电影/空枪/gone.strm") {
		t.Errorf("孤儿判定不对: %v", res.Extra)
	}
	if len(res.Extra) != 2 {
		t.Errorf("应该正好 2 个孤儿，实际 %v", res.Extra)
	}
	if len(res.Missing) != 1 || res.Missing[0] != "电影/空枪/missing.strm" {
		t.Errorf("失效判定不对: %v", res.Missing)
	}
}

func TestCleanOrphansAll(t *testing.T) {
	s, lib, root := setupOrphanLib(t)

	body := `{"library_id":` + itoa(lib.ID) + `,"all":true}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/strm/orphans/clean", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCleanOrphans(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("清理失败: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Removed int `json:"removed"`
		Rows    int `json:"rows"`
		Failed  int `json:"failed"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Removed != 2 || out.Rows != 1 || out.Failed != 0 {
		t.Errorf("清理统计不对: %+v", out)
	}

	if _, err := os.Stat(filepath.Join(root, "孤儿.strm")); !os.IsNotExist(err) {
		t.Error("孤儿 .strm 应该被删掉")
	}
	if _, err := os.Stat(filepath.Join(root, "电影", "空枪", "gone.strm")); !os.IsNotExist(err) {
		t.Error("gone.strm 应该被删掉")
	}
	if _, err := os.Stat(filepath.Join(root, "电影", "空枪", "keep.strm")); err != nil {
		t.Error("台账里的正常文件不能被误删")
	}

	n, err := s.st.CountFiles(context.Background(), lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("台账应只剩 1 行，实际 %d", n)
	}
}

// 前端传来的清单可能过期或者被人手动改过：台账里有的绝不删，路径穿越直接跳过。
func TestCleanOrphansRejectsStaleAndTraversal(t *testing.T) {
	s, lib, root := setupOrphanLib(t)

	outside := filepath.Join(filepath.Dir(root), "evil.strm")
	if err := os.WriteFile(outside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(outside) })

	body := `{"library_id":` + itoa(lib.ID) + `,"extra":["电影/空枪/keep.strm","../evil.strm"]}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/strm/orphans/clean", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	s.handleCleanOrphans(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("清理失败: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(filepath.Join(root, "电影", "空枪", "keep.strm")); err != nil {
		t.Error("台账里的文件被误删了")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Error("库外文件被删了（路径穿越没挡住）")
	}
}

func TestSetKeepExtHandler(t *testing.T) {
	s, lib, _ := setupOrphanLib(t)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/libraries/"+itoa(lib.ID)+"/keep-ext",
		strings.NewReader(`{"keep_ext":false}`))
	req.Header.Set("Content-Type", "application/json")
	req.SetPathValue("id", itoa(lib.ID))
	s.handleSetKeepExt(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("改开关失败: %d %s", rec.Code, rec.Body.String())
	}
	got, err := s.st.GetLibrary(context.Background(), lib.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeepExt {
		t.Error("keep_ext 应该被改成 false")
	}
}
