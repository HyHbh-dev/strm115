package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestAccountAndLibrary(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.CreateAccount(ctx, "115", "主号", `{"UID":"1","CID":"2","SEID":"3"}`)
	if err != nil {
		t.Fatal(err)
	}
	acct, err := s.GetAccount(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if acct.Name != "主号" || acct.Credential == "" {
		t.Fatalf("账号读回不对: %+v", acct)
	}

	libID, err := s.CreateLibrary(ctx, Library{
		AccountID: id, Name: "电影", RemoteCID: "100", LocalPath: "/media/电影",
		CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	lib, err := s.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	if !lib.KeepExt || lib.Name != "电影" {
		t.Fatalf("库读回不对: %+v", lib)
	}

	if err := s.SetLibraryServer(ctx, libID, "emby", "lib-9"); err != nil {
		t.Fatal(err)
	}
	lib, _ = s.GetLibrary(ctx, libID)
	if lib.ServerLibID != "lib-9" {
		t.Fatalf("媒体服务器映射没存上: %+v", lib)
	}
}

func TestUpsertAndStale(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	acctID, _ := s.CreateAccount(ctx, "115", "a", "{}")
	libID, _ := s.CreateLibrary(ctx, Library{AccountID: acctID, Name: "L", RemoteCID: "1", LocalPath: "/m"})

	files := []File{
		{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "a/x.strm", Name: "x.mkv", Size: 10},
		{LibraryID: libID, FileID: "f2", PickCode: "p2", RelPath: "a/y.strm", Name: "y.mkv", Size: 20},
	}
	if err := s.UpsertFiles(ctx, acctID, files); err != nil {
		t.Fatal(err)
	}
	// 重复 upsert 不应产生第二行
	if err := s.UpsertFiles(ctx, acctID, files); err != nil {
		t.Fatal(err)
	}
	n, _ := s.CountFiles(ctx, libID)
	if n != 2 {
		t.Fatalf("应为 2 行，实际 %d", n)
	}

	// f2 改名：rel_path 应被更新
	if err := s.UpsertFiles(ctx, acctID, []File{
		{LibraryID: libID, FileID: "f2", PickCode: "p2", RelPath: "a/z.strm", Name: "y.mkv", Size: 20},
	}); err != nil {
		t.Fatal(err)
	}
	paths, _ := s.FileRelPaths(ctx, libID)
	if paths["f2"] != "a/z.strm" {
		t.Fatalf("改名没更新: %+v", paths)
	}

	// f1 这次没见到 → stale
	seen := map[string]struct{}{"f2": {}}
	var stale []string
	if err := s.IterStale(ctx, libID, seen, func(f File) error {
		stale = append(stale, f.FileID)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 || stale[0] != "f1" {
		t.Fatalf("stale 判定不对: %v", stale)
	}
}

func TestAccountByPickcode(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	acctID, _ := s.CreateAccount(ctx, "115", "a", "{}")
	libID, _ := s.CreateLibrary(ctx, Library{AccountID: acctID, Name: "L", RemoteCID: "1", LocalPath: "/m"})
	if err := s.UpsertFiles(ctx, acctID, []File{{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "x.strm"}}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.AccountByPickcode(ctx, "p1")
	if !ok || got != acctID {
		t.Fatalf("反查账号失败: %d %v", got, ok)
	}
	if _, ok := s.AccountByPickcode(ctx, "nope"); ok {
		t.Fatal("不存在的 pickcode 不该命中")
	}
}

func TestSettings(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	if _, ok := s.GetSetting(ctx, "play_domain"); ok {
		t.Fatal("初始不该有值")
	}
	if err := s.SetSetting(ctx, "play_domain", "http://a:6086"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSetting(ctx, "play_domain", "http://b:6086"); err != nil {
		t.Fatal(err)
	}
	v, ok := s.GetSetting(ctx, "play_domain")
	if !ok || v != "http://b:6086" {
		t.Fatalf("覆盖写失败: %q", v)
	}
}

func TestCascadeDelete(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	acctID, _ := s.CreateAccount(ctx, "115", "a", "{}")
	libID, _ := s.CreateLibrary(ctx, Library{AccountID: acctID, Name: "L", RemoteCID: "1", LocalPath: "/m"})
	s.UpsertFiles(ctx, acctID, []File{{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "x.strm"}})

	if err := s.DeleteAccount(ctx, acctID); err != nil {
		t.Fatal(err)
	}
	libs, _ := s.ListLibraries(ctx)
	if len(libs) != 0 {
		t.Fatalf("账号删除后库应级联删除，实际 %d", len(libs))
	}
	time.Sleep(10 * time.Millisecond)
	if n, _ := s.CountFiles(ctx, libID); n != 0 {
		t.Fatalf("台账应级联删除，实际 %d", n)
	}
}
