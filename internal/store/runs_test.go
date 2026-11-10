package store

import (
	"context"
	"testing"
	"time"
)

// 同步流水账是 Strm 管理页「最近轮次」的唯一数据源，字段与统计都不能丢。
func TestSyncRuns(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	acctID, err := s.CreateAccount(ctx, "115", "主号", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	libID, err := s.CreateLibrary(ctx, Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100",
		LocalPath: "/media/电影", CollectionType: "movies", KeepExt: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	id, err := s.StartSyncRun(ctx, libID, "电影", "full")
	if err != nil {
		t.Fatalf("开始记账失败: %v", err)
	}
	if err := s.FinishSyncRun(ctx, id, "done", 23, 20, 3, 1, 2, "同步完成"); err != nil {
		t.Fatalf("补账失败: %v", err)
	}

	runs, err := s.ListSyncRuns(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("应有 1 条记录，实际 %d", len(runs))
	}
	r := runs[0]
	if r.LibraryName != "电影" || r.Trigger != "full" || r.Phase != "done" {
		t.Errorf("基本字段不对: %+v", r)
	}
	if r.Files != 23 || r.Written != 20 || r.Removed != 3 || r.Failed != 1 || r.Skipped != 2 {
		t.Errorf("统计不对: %+v", r)
	}
	if r.FinishedAt.IsZero() || r.StartedAt.IsZero() {
		t.Errorf("时间没记上: %+v", r)
	}

	if got := s.LastFullSyncAt(ctx); got.IsZero() {
		t.Error("成功全量后 LastFullSyncAt 不该是零值")
	}

	// 增量不该影响「上次全量」
	incID, _ := s.StartSyncRun(ctx, libID, "电影", "incremental")
	if err := s.FinishSyncRun(ctx, incID, "done", 23, 0, 0, 0, 0, "同步完成"); err != nil {
		t.Fatal(err)
	}
	first := s.LastFullSyncAt(ctx)
	if first.IsZero() {
		t.Fatal("增量把上次全量覆盖掉了")
	}

	// 失败的全量也不算
	failID, _ := s.StartSyncRun(ctx, libID, "电影", "full")
	if err := s.FinishSyncRun(ctx, failID, "error", 0, 0, 0, 0, 0, "同步失败"); err != nil {
		t.Fatal(err)
	}
	if !s.LastFullSyncAt(ctx).Equal(first) {
		t.Error("失败的全量不该成为「上次全量」")
	}
}

func TestFilesByFileIDs(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	acctID, _ := s.CreateAccount(ctx, "115", "主号", `{}`)
	libID, _ := s.CreateLibrary(ctx, Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100",
		LocalPath: "/media/电影", CollectionType: "movies", KeepExt: true,
	})
	err := s.UpsertFiles(ctx, acctID, []File{
		{LibraryID: libID, FileID: "f1", PickCode: "p1", RelPath: "电影/空枪/x.strm", Name: "x.mkv", Size: 10},
		{LibraryID: libID, FileID: "f2", PickCode: "p2", RelPath: "电影/沙丘/y.strm", Name: "y.mkv", Size: 20},
	})
	if err != nil {
		t.Fatal(err)
	}

	refs, err := s.FilesByFileIDs(ctx, acctID, []string{"f1", "missing"})
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 {
		t.Fatalf("应只查回 1 条，实际 %d", len(refs))
	}
	if refs["f1"].RelPath != "电影/空枪/x.strm" || refs["f1"].LibraryID != libID {
		t.Errorf("台账反查结果不对: %+v", refs["f1"])
	}

	// 换账号查不到——这条能防住「同一台机器上两个 115 账号串台账」
	otherID, _ := s.CreateAccount(ctx, "115", "小号", `{}`)
	refs, _ = s.FilesByFileIDs(ctx, otherID, []string{"f1"})
	if len(refs) != 0 {
		t.Errorf("别的账号不该查到这条: %+v", refs)
	}
}

func TestSetLibraryKeepExt(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	acctID, _ := s.CreateAccount(ctx, "115", "主号", `{}`)
	libID, _ := s.CreateLibrary(ctx, Library{
		AccountID: acctID, Name: "电影", RemoteCID: "100",
		LocalPath: "/media/电影", CollectionType: "movies", KeepExt: true,
	})
	if err := s.SetLibraryKeepExt(ctx, libID, false); err != nil {
		t.Fatal(err)
	}
	lib, err := s.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	if lib.KeepExt {
		t.Error("keep_ext 应该被关掉")
	}
	if lib.LastSyncAt.After(time.Now()) {
		t.Error("改开关不该动同步时间")
	}
}
