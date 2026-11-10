package store

import (
	"context"
	"errors"
	"testing"
)

// 老库（v0.2.6）每个库只有一个账号+目录；升级后必须能自动变成「一个来源」，
// 否则第一次同步会报「这个库还没挂网盘目录」。
func TestLibrarySourcesBackfillAndCRUD(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	a1, _ := s.CreateAccount(ctx, "115", "115", "{}")
	a2, _ := s.CreateAccount(ctx, "quarktv", "夸克", "{}")

	libID, err := s.CreateLibrary(ctx, Library{
		AccountID: a1, Name: "影视", RemoteCID: "r1", RemotePath: "影视",
		LocalPath: "/media", CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	srcs, err := s.ListSources(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	if len(srcs) != 1 || srcs[0].AccountID != a1 || srcs[0].RemoteCID != "r1" {
		t.Fatalf("建库应自动带一条主来源: %+v", srcs)
	}
	if srcs[0].AccountKind != "115" {
		t.Errorf("来源应带上账号类型（播放地址要用）: %+v", srcs[0])
	}

	// 挂第二个网盘目录
	if _, err := s.AddSource(ctx, LibrarySource{
		LibraryID: libID, AccountID: a2, RemoteCID: "rq", RemotePath: "夸克影视",
	}); err != nil {
		t.Fatal(err)
	}
	// 重复挂同一条要能被识别，而不是抛 SQLite 原始错误
	_, err = s.AddSource(ctx, LibrarySource{
		LibraryID: libID, AccountID: a2, RemoteCID: "rq", RemotePath: "夸克影视",
	})
	if !errors.Is(err, ErrDuplicate) {
		t.Fatalf("重复来源应返回 ErrDuplicate，实际 %v", err)
	}

	lib, err := s.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	if len(lib.Sources) != 2 {
		t.Fatalf("GetLibrary 应带上两个来源: %+v", lib.Sources)
	}

	// 摘掉一个仍然能用（库和本地文件都不受影响）
	if err := s.DeleteSource(ctx, libID, lib.Sources[1].ID); err != nil {
		t.Fatal(err)
	}
	if srcs, _ := s.ListSources(ctx, libID); len(srcs) != 1 {
		t.Fatalf("摘掉后应剩一个来源: %+v", srcs)
	}
	if err := s.DeleteSource(ctx, libID, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("删不存在的来源应返回 ErrNotFound，实际 %v", err)
	}
}

func TestAccountByKind(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a1, _ := s.CreateAccount(ctx, "115", "115", "{}")
	q, _ := s.CreateAccount(ctx, "quarktv", "夸克", "{}")

	if id, ok := s.AccountByKind(ctx, "115"); !ok || id != a1 {
		t.Errorf("按类型取 115 账号失败: %d %v", id, ok)
	}
	if id, ok := s.AccountByKind(ctx, "quarktv"); !ok || id != q {
		t.Errorf("按类型取夸克账号失败: %d %v", id, ok)
	}
	if _, ok := s.AccountByKind(ctx, "nope"); ok {
		t.Error("没有的类型不该命中")
	}
}

// 清理失效条目按账号过滤：一个库挂两家时，扫完 115 不能把夸克的行清掉。
func TestIterStaleByAccount(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	a1, _ := s.CreateAccount(ctx, "115", "115", "{}")
	a2, _ := s.CreateAccount(ctx, "quarktv", "夸克", "{}")
	libID, _ := s.CreateLibrary(ctx, Library{
		AccountID: a1, Name: "L", RemoteCID: "r1", LocalPath: "/m", CollectionType: "movies",
	})

	if err := s.UpsertFiles(ctx, a1, []File{
		{LibraryID: libID, AccountID: a1, FileID: "f1", PickCode: "p1", RelPath: "a.strm"},
		{LibraryID: libID, AccountID: a2, FileID: "f2", PickCode: "p2", RelPath: "b.strm"},
	}); err != nil {
		t.Fatal(err)
	}

	var got []string
	err := s.IterStaleByAccount(ctx, libID, a1, map[string]struct{}{}, func(f File) error {
		got = append(got, f.FileID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "f1" {
		t.Fatalf("只该扫出 115 账号名下的失效行，实际 %v", got)
	}
}
