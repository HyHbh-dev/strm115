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

// fakePan 是一棵可控的假网盘：cid → 子条目。
type fakePan struct {
	tree map[string][]pan.Entry
	// 每次 ListPage 的调用次数，用来验证「翻页受节流控制」
	pages int
}

func (f *fakePan) Kind() string { return "fake" }
func (f *fakePan) ListPage(ctx context.Context, dirID string, offset, limit int) ([]pan.Entry, int, error) {
	f.pages++
	all := f.tree[dirID]
	if offset >= len(all) {
		return nil, len(all), nil
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	return all[offset:end], len(all), nil
}
func (f *fakePan) Stat(ctx context.Context, id string) (pan.Entry, error) {
	return pan.Entry{ID: id}, nil
}
func (f *fakePan) DownloadURL(ctx context.Context, pc, ua string) (string, error) {
	return "https://cdn/" + pc, nil
}
func (f *fakePan) Mkdir(ctx context.Context, parent, name string) (string, error) { return "new", nil }
func (f *fakePan) Move(ctx context.Context, to string, ids ...string) error       { return nil }
func (f *fakePan) Rename(ctx context.Context, id, name string) error              { return nil }
func (f *fakePan) Remove(ctx context.Context, ids ...string) error                { return nil }
func (f *fakePan) Ping(ctx context.Context) error                                 { return nil }
func (f *fakePan) Close() error                                                   { return nil }

func mkfile(id, name string, size int64) pan.Entry {
	return pan.Entry{ID: id, Name: name, Size: size, PickCode: "pc-" + id}
}

func setup(t *testing.T) (*Service, *store.Store, store.Library, *fakePan, string) {
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
	acctID, _ := st.CreateAccount(ctx, "fake", "测试", "{}")
	libID, _ := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "影视", RemoteCID: "root",
		LocalPath: media, CollectionType: "mixed", KeepExt: true,
	})
	lib, _ := st.GetLibrary(ctx, libID)

	fp := &fakePan{tree: map[string][]pan.Entry{
		"root": {
			{ID: "d1", Name: "电影", IsDir: true},
			mkfile("f1", "流浪地球 (2019).mkv", 100),
			mkfile("f2", "readme.nfo", 1), // 非视频，必须忽略
			{ID: "d2", Name: "剧集", IsDir: true},
		},
		"d1": {mkfile("f3", "A.mkv", 200), mkfile("f4", "B.ISO", 300)},
		"d2": {mkfile("f5", "S01E01.mkv", 400)},
	}}

	mgr := pan.NewManager(fp, pan.Account{ID: acctID, Kind: "fake"}, pan.Options{
		MinGap: time.Millisecond, MaxAttempts: 1,
		Logf: func(string, ...any) {},
	})
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := New(st, func(context.Context, int64) (*pan.Manager, error) { return mgr, nil }, nil, quiet)
	return svc, st, lib, fp, media
}

func TestFullSyncWritesStrm(t *testing.T) {
	svc, st, lib, _, media := setup(t)
	ctx := context.Background()

	job, err := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job)

	if p := job.Snapshot(); p.Phase != "done" {
		t.Fatalf("同步未成功: %+v", p)
	}

	want := map[string]string{
		"流浪地球 (2019).strm":            "http://nas:6086/d/pc-f1.mkv?/流浪地球 (2019).mkv",
		filepath.Join("电影", "A.strm"): "http://nas:6086/d/pc-f3.mkv?/A.mkv",
		// ISO 保留双扩展名，Emby 靠它认光盘镜像
		filepath.Join("电影", "B.iso.strm"):  "http://nas:6086/d/pc-f4.iso?/B.ISO",
		filepath.Join("剧集", "S01E01.strm"): "http://nas:6086/d/pc-f5.mkv?/S01E01.mkv",
	}
	for rel, content := range want {
		b, err := os.ReadFile(filepath.Join(media, rel))
		if err != nil {
			t.Fatalf("缺少 %s: %v", rel, err)
		}
		if strings.TrimSpace(string(b)) != content {
			t.Errorf("%s 内容不对:\n got %q\nwant %q", rel, b, content)
		}
	}
	// 非视频不该生成
	if _, err := os.Stat(filepath.Join(media, "readme.strm")); !os.IsNotExist(err) {
		t.Error("nfo 不该生成 strm")
	}

	n, _ := st.CountFiles(ctx, lib.ID)
	if n != 4 {
		t.Errorf("台账应为 4 条，实际 %d", n)
	}
}

func TestSyncRemovesStaleAndOldName(t *testing.T) {
	svc, st, lib, fp, media := setup(t)
	ctx := context.Background()
	job, _ := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	waitJob(t, job)

	// 网盘侧：A.mkv 改名成 A2.mkv，S01E01.mkv 被删除
	fp.tree["d1"] = []pan.Entry{mkfile("f3", "A2.mkv", 200), mkfile("f4", "B.ISO", 300)}
	fp.tree["d2"] = nil

	job2, err := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job2)

	if _, err := os.Stat(filepath.Join(media, "电影", "A.strm")); !os.IsNotExist(err) {
		t.Error("改名后旧 .strm 应被删除（否则 Emby 会看到两个条目）")
	}
	if _, err := os.Stat(filepath.Join(media, "电影", "A2.strm")); err != nil {
		t.Error("改名后的新 .strm 应存在")
	}
	if _, err := os.Stat(filepath.Join(media, "剧集", "S01E01.strm")); !os.IsNotExist(err) {
		t.Error("网盘删除后本地 .strm 应被清理")
	}
	n, _ := st.CountFiles(ctx, lib.ID)
	if n != 3 {
		t.Errorf("台账应剩 3 条，实际 %d", n)
	}
}

func TestSyncKeepsStaleWhenDisabled(t *testing.T) {
	svc, _, lib, fp, media := setup(t)
	job, _ := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	waitJob(t, job)

	fp.tree["d2"] = nil
	job2, _ := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: false})
	waitJob(t, job2)

	if _, err := os.Stat(filepath.Join(media, "剧集", "S01E01.strm")); err != nil {
		t.Error("关掉清理后本地文件应保留")
	}
}

func TestSyncRequiresDomain(t *testing.T) {
	svc, _, lib, _, _ := setup(t)
	job, err := svc.Start(lib, Options{})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job)
	p := job.Snapshot()
	if p.Phase != "error" || !strings.Contains(p.Err, "播放域名") {
		t.Fatalf("没配域名应报错，实际 %+v", p)
	}
}

func TestMinSizeFilters(t *testing.T) {
	svc, st, lib, _, media := setup(t)
	ctx := context.Background()
	job, _ := svc.Start(lib, Options{Domain: "http://a:6086", KeepExt: true, DeleteStale: true, MinSize: 250})
	waitJob(t, job)

	if _, err := os.Stat(filepath.Join(media, "流浪地球 (2019).strm")); !os.IsNotExist(err) {
		t.Error("小于 MinSize 的视频应被跳过")
	}
	n, _ := st.CountFiles(ctx, lib.ID)
	if n != 2 { // 只剩 f4(300) 与 f5(400)
		t.Errorf("应剩 2 条，实际 %d", n)
	}
}

func waitJob(t *testing.T, j *Job) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !j.Running() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("同步超时")
}

// —— 去重：同一部作品只留一条 .strm ——

func TestPlanDirCollapsesCopies(t *testing.T) {
	// 真机现场：一个作品目录里三份拷贝（mp4 + mkv + WEB-DL），Emby 会出三条同名条目
	files := []pan.Entry{
		mkfile("a", "空枪.V.2026.mp4", 2<<30),
		mkfile("b", "空枪.V.2026.mkv", 8<<30),
		mkfile("c", "空枪.V.2026.WEB-DL.DV.2160p.H265.DDP.5.1.mkv", 16<<30),
	}
	keep, drop := planDir("电影/华语电影/空枪.2026.{tmdbid=1567937}", files)
	if len(keep) != 1 || len(drop) != 2 {
		t.Fatalf("应保留 1 条、丢弃 2 条，实际 keep=%d drop=%d", len(keep), len(drop))
	}
	if keep[0].ID != "c" {
		t.Errorf("应保留体积最大的 c，实际 %s", keep[0].ID)
	}
}

func TestPlanDirKeepsEpisodesApart(t *testing.T) {
	// 一季 20 集不能塌成 1 集；同一个 E01 的两个版本才该合并
	files := []pan.Entry{
		mkfile("e1a", "Show.S01E01.1080p.WEB-DL.mkv", 1<<30),
		mkfile("e1b", "Show.S01E01.2160p.WEB-DL.mkv", 4<<30),
		mkfile("e2", "Show.S01E02.1080p.WEB-DL.mkv", 1<<30),
		mkfile("e3", "Show.S01E03.1080p.WEB-DL.mkv", 1<<30),
	}
	keep, drop := planDir("剧集/Show/Season 1", files)
	if len(keep) != 3 || len(drop) != 1 {
		t.Fatalf("应保留 3 集、丢弃 1 个副本，实际 keep=%d drop=%d", len(keep), len(drop))
	}
	if drop[0].ID != "e1a" {
		t.Errorf("应丢弃小体积的 e1a，实际 %s", drop[0].ID)
	}
}

func TestPlanDirKeepsDifferentMovies(t *testing.T) {
	// 合集目录里平铺多部片子：片名不同，绝不能合并
	files := []pan.Entry{
		mkfile("m1", "流浪地球.2019.mkv", 1<<30),
		mkfile("m2", "沙丘2.2024.mkv", 1<<30),
		mkfile("m3", "奥本海默.2023.mkv", 1<<30),
	}
	keep, drop := planDir("电影/2024合集", files)
	if len(keep) != 3 || len(drop) != 0 {
		t.Fatalf("不同影片不该被合并，实际 keep=%d drop=%d", len(keep), len(drop))
	}
}

func TestPlanDirKeepsNumberedEpisodes(t *testing.T) {
	// 纯数字命名的剧集：认不出片名，宁可不去重也不能删掉 19 集
	files := []pan.Entry{
		mkfile("n1", "01.mkv", 1<<30),
		mkfile("n2", "02.mkv", 1<<30),
		mkfile("n3", "03.mkv", 1<<30),
	}
	keep, drop := planDir("剧集/某剧/Season 2", files)
	if len(keep) != 3 || len(drop) != 0 {
		t.Fatalf("数字命名不该被合并，实际 keep=%d drop=%d", len(keep), len(drop))
	}
}

func TestSyncDedupeRemovesExtraStrm(t *testing.T) {
	svc, st, lib, fp, media := setup(t)
	ctx := context.Background()
	// 把 d1 换成「同一部片两个拷贝」的目录
	fp.tree["d1"] = []pan.Entry{
		mkfile("f3", "空枪.2026.mp4", 200),
		mkfile("f4", "空枪.2026.mkv", 900),
	}

	// 第一轮不开关：两条都写出去，模拟「已经跑过一次的老库」
	job, err := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job)
	if n, _ := st.CountFiles(ctx, lib.ID); n != 4 { // f1 + f3 + f4 + f5
		t.Fatalf("第一轮应有 4 条，实际 %d", n)
	}
	if n := countStrm(t, filepath.Join(media, "电影")); n != 2 {
		t.Fatalf("第一轮应写出 2 条 .strm，实际 %d", n)
	}

	// 第二轮开去重：多余的 .strm 和台账行都要消失
	job2, err := svc.Start(lib, Options{Domain: "http://nas:6086", KeepExt: true, DeleteStale: true, Dedupe: true})
	if err != nil {
		t.Fatal(err)
	}
	waitJob(t, job2)
	if p := job2.Snapshot(); p.Phase != "done" {
		t.Fatalf("同步未成功: %+v", p)
	}
	if p := job2.Snapshot(); p.Skipped != 1 {
		t.Errorf("应记 1 条被去重，实际 %d", p.Skipped)
	}
	if n := countStrm(t, filepath.Join(media, "电影")); n != 1 {
		t.Errorf("去重后目录里只该剩 1 条 .strm，实际 %d", n)
	}
	if _, err := os.Stat(filepath.Join(media, "电影", "空枪.2026.strm")); err != nil {
		t.Errorf("保留的那条应该还在: %v", err)
	}
	if n, _ := st.CountFiles(ctx, lib.ID); n != 3 { // f1 + f4 + f5
		t.Errorf("台账应剩 3 条，实际 %d", n)
	}
}

func countStrm(t *testing.T, dir string) int {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range ents {
		if strings.EqualFold(filepath.Ext(e.Name()), ".strm") {
			n++
		}
	}
	return n
}

// 这一条守的是真机踩过的坑：库在 Emby 里扫 /media/movies/电影，
// 而整理把新片放到了 /media/movies/空枪 (2026) —— 同步报「完成」，
// Emby 里却一条都不出现。诊断只能靠把两条路径摆在一起比。
func TestUnderAnyRoot(t *testing.T) {
	cases := []struct {
		roots []string
		local string
		want  bool
	}{
		{[]string{"/media/movies/电影"}, "/media/movies", false},   // 库比镜像更细：看不到
		{[]string{"/media/movies"}, "/media/movies", true},       // 完全一致
		{[]string{"/media/movies"}, "/media/movies/影视/电影", true}, // 镜像在库里面
		{[]string{"/media/movies/"}, "/media/movies/空枪 (2026)", true},
		{[]string{"/media/movies"}, "/media/movies-old", false}, // 前缀像但不是子目录
		{[]string{"/media/movies", "/media/tv"}, "/media/tv/剧集", true},
	}
	for _, c := range cases {
		if got := underAnyRoot(c.roots, c.local); got != c.want {
			t.Errorf("underAnyRoot(%v, %q) = %v, 期望 %v", c.roots, c.local, got, c.want)
		}
	}
}
