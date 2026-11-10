package sync

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strm115/internal/pan"
	"strm115/internal/store"
)

// 一个媒体库同时挂 115 和夸克（v0.2.7）。
//
// 这一组测试守的是三条边界：
//  1. .strm 里的播放地址必须能区分网盘（夸克 q/ 前缀，115 保持原样）；
//  2. 台账行要记对归属账号，否则播放时代理会拿错凭证；
//  3. 清理失效条目必须按来源分开，不能把另一个网盘那半边删掉。
type multiFixture struct {
	svc            *Service
	st             *store.Store
	lib            store.Library
	acct115, acctQ int64
	pan115, panQ   *fakePan
	media          string
}

func setupMulti(t *testing.T) *multiFixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	media := filepath.Join(dir, "media")
	os.MkdirAll(media, 0o750)
	ctx := context.Background()

	p115, _ := st.CreateAccount(ctx, "115", "115 账号", "{}")
	pq, _ := st.CreateAccount(ctx, "quarktv", "夸克", "{}")

	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: p115, Name: "影视", RemoteCID: "r115",
		RemotePath: "影视", LocalPath: media, CollectionType: "movies", KeepExt: true,
	})
	if _, err := st.AddSource(ctx, store.LibrarySource{
		LibraryID: libID, AccountID: pq, RemoteCID: "rq", RemotePath: "夸克影视",
	}); err != nil {
		t.Fatal(err)
	}
	lib, _ := st.GetLibrary(ctx, libID)

	f115 := &fakePan{tree: map[string][]pan.Entry{
		"r115": {{ID: "m1", Name: "电影", IsDir: true}},
		"m1":   {mkfile("f115", "沙丘2.mkv", 100), mkfile("f115b", "奥本海默.mkv", 200)},
	}}
	fq := &fakePan{tree: map[string][]pan.Entry{
		"rq":  {{ID: "qm1", Name: "电影", IsDir: true}},
		"qm1": {mkfile("fq1", "沙丘2.mkv", 900), mkfile("fq2", "可怜的东西.mkv", 300)},
	}}

	mgrs := map[int64]*pan.Manager{
		p115: pan.NewManager(f115, pan.Account{ID: p115, Kind: "115"},
			pan.Options{MinGap: time.Millisecond, MaxAttempts: 1, Logf: func(string, ...any) {}}),
		pq: pan.NewManager(fq, pan.Account{ID: pq, Kind: "quarktv"},
			pan.Options{MinGap: time.Millisecond, MaxAttempts: 1, Logf: func(string, ...any) {}}),
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(st, func(_ context.Context, accountID int64) (*pan.Manager, error) {
		return mgrs[accountID], nil
	}, nil, quiet)

	return &multiFixture{
		svc: svc, st: st, lib: lib, media: media,
		acct115: p115, acctQ: pq, pan115: f115, panQ: fq,
	}
}

func readStrm(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return strings.TrimSpace(string(b))
}

func TestMultiSourceStrmCarriesPanKind(t *testing.T) {
	f := setupMulti(t)
	ctx := context.Background()

	job, err := f.svc.Start(f.lib, Options{Domain: "https://emby.example.com", KeepExt: true, DeleteStale: true})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job)
	if p := job.Snapshot(); p.Phase != "done" {
		t.Fatalf("同步未成功: %+v", p)
	}

	// 115 的地址保持原样（历史 .strm 一个都不用改）
	got115 := readStrm(t, filepath.Join(f.media, "电影", "奥本海默.strm"))
	if want := "https://emby.example.com/d/pc-f115b.mkv?/奥本海默.mkv"; got115 != want {
		t.Errorf("115 地址不对:\n got %q\nwant %q", got115, want)
	}

	// 夸克的地址必须带 q/ 前缀：fid 和 115 的 pickcode 长一个样，
	// 不带前缀播放代理就只能猜账号。
	gotQ := readStrm(t, filepath.Join(f.media, "电影", "可怜的东西.strm"))
	if want := "https://emby.example.com/d/q/pc-fq2.mkv?/可怜的东西.mkv"; gotQ != want {
		t.Errorf("夸克地址不对:\n got %q\nwant %q", gotQ, want)
	}

	// 台账里的归属账号决定了播放时用谁的凭证
	files, _, err := f.st.ListFiles(ctx, f.lib.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]int64{}
	for _, file := range files {
		owner[file.Name] = file.AccountID
	}
	if owner["奥本海默.mkv"] != f.acct115 {
		t.Errorf("115 的文件应挂在 115 账号下: %+v", owner)
	}
	if owner["可怜的东西.mkv"] != f.acctQ {
		t.Errorf("夸克的文件应挂在夸克账号下: %+v", owner)
	}
}

func TestMultiSourceSameFileKeepsBigger(t *testing.T) {
	f := setupMulti(t)
	ctx := context.Background()
	job, _ := f.svc.Start(f.lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	waitJob(t, job)

	// 两个网盘都有「电影/沙丘2.mkv」，夸克那份更大（900 > 100）：
	// 一个 .strm 只能指向一家，留画质更好的那份，台账里也只该剩一行。
	got := readStrm(t, filepath.Join(f.media, "电影", "沙丘2.strm"))
	if !strings.Contains(got, "/d/q/pc-fq1.mkv") {
		t.Errorf("应保留夸克那份更大的: %s", got)
	}
	files, _, err := f.st.ListFiles(ctx, f.lib.ID, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, file := range files {
		if file.RelPath == "电影/沙丘2.strm" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("同一个 .strm 在台账里只该有一行，实际 %d", n)
	}
}

func TestMultiSourceStaleCleanupIsScoped(t *testing.T) {
	f := setupMulti(t)
	job, _ := f.svc.Start(f.lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	waitJob(t, job)

	// 只在夸克侧删掉一部片：115 那边必须毫发无损。
	// 若清理没有按来源隔离，这里会把 115 的两条一起删掉再写回来。
	f.panQ.tree["qm1"] = []pan.Entry{mkfile("fq1", "沙丘2.mkv", 900)}

	job2, _ := f.svc.Start(f.lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	waitJob(t, job2)

	if _, err := os.Stat(filepath.Join(f.media, "电影", "可怜的东西.strm")); !os.IsNotExist(err) {
		t.Error("夸克侧删除后 .strm 应被清理")
	}
	if _, err := os.Stat(filepath.Join(f.media, "电影", "奥本海默.strm")); err != nil {
		t.Error("115 侧的文件不该被另一来源的清理波及")
	}
	if _, err := os.Stat(filepath.Join(f.media, "电影", "沙丘2.strm")); err != nil {
		t.Error("两边都有的文件应保留")
	}
}
