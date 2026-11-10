package httpapi

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"strm115/internal/pan"
)

// fakeDirs 是一棵内存里的目录树，够 dirCache 用。
//
// 只认「目录」：文件用 fileIDs 单独记，用来验证 dirCache 不会把同名文件当目录复用。
type fakeDirs struct {
	tree     map[string][]pan.Entry // 父 cid → 子项
	seq      int
	listCall map[string]int // 每个 cid 被列了几次
	mkdirLog []string       // "父cid/名字"，按调用顺序
	listErr  error
	mkdirErr error
}

func newFakeDirs() *fakeDirs {
	return &fakeDirs{
		tree:     map[string][]pan.Entry{},
		listCall: map[string]int{},
	}
}

// add 在父目录下放一项，返回它的 id。
func (f *fakeDirs) add(parent, name string, isDir bool) string {
	f.seq++
	id := fmt.Sprintf("c%d", f.seq)
	f.tree[parent] = append(f.tree[parent], pan.Entry{ID: id, ParentID: parent, Name: name, IsDir: isDir})
	return id
}

func (f *fakeDirs) ListAll(_ context.Context, dirID string, _ int) ([]pan.Entry, error) {
	f.listCall[dirID]++
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.tree[dirID], nil
}

func (f *fakeDirs) Mkdir(_ context.Context, parentID, name string) (string, error) {
	f.mkdirLog = append(f.mkdirLog, parentID+"/"+name)
	if f.mkdirErr != nil {
		return "", f.mkdirErr
	}
	return f.add(parentID, name, true), nil
}

func (f *fakeDirs) Move(context.Context, string, ...string) (int, error) { return 0, nil }

func (f *fakeDirs) mkdirs() int { return len(f.mkdirLog) }

// 新建：不存在就建，建完记住，同名再来一次不再建。
func TestDirCacheCreatesOnce(t *testing.T) {
	f := newFakeDirs()
	d := newDirCache(f)
	ctx := context.Background()

	id1, err := d.ensure(ctx, "root", []string{"片名 (2024)"})
	if err != nil {
		t.Fatalf("首次 ensure 出错: %v", err)
	}
	if f.mkdirs() != 1 {
		t.Fatalf("首次应该建 1 个目录，实际 %d 次: %v", f.mkdirs(), f.mkdirLog)
	}
	if f.listCall["root"] != 1 {
		t.Fatalf("首次应该只列一次根目录，实际 %d 次", f.listCall["root"])
	}

	id2, err := d.ensure(ctx, "root", []string{"片名 (2024)"})
	if err != nil {
		t.Fatalf("二次 ensure 出错: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("同名目录应该拿到同一个 cid，%q != %q", id1, id2)
	}
	if f.mkdirs() != 1 {
		t.Fatalf("二次不该再建目录，实际共 %d 次: %v", f.mkdirs(), f.mkdirLog)
	}
	if f.listCall["root"] != 1 {
		t.Fatalf("二次不该再列根目录，实际 %d 次", f.listCall["root"])
	}
}

// 已存在的目录直接复用 cid，不重复创建。
func TestDirCacheReusesExisting(t *testing.T) {
	f := newFakeDirs()
	exist := f.add("root", "剧名 (2023)", true)
	d := newDirCache(f)

	got, err := d.ensure(context.Background(), "root", []string{"剧名 (2023)"})
	if err != nil {
		t.Fatalf("ensure 出错: %v", err)
	}
	if got != exist {
		t.Fatalf("应复用已有目录 %q，实际 %q", exist, got)
	}
	if f.mkdirs() != 0 {
		t.Fatalf("已存在的目录不该再建，实际建了: %v", f.mkdirLog)
	}
}

// 同名文件不能当目录复用，必须自己建目录。
func TestDirCacheIgnoresSameNameFile(t *testing.T) {
	f := newFakeDirs()
	f.add("root", "片名 (2024)", false) // 是个文件，不是目录
	d := newDirCache(f)

	got, err := d.ensure(context.Background(), "root", []string{"片名 (2024)"})
	if err != nil {
		t.Fatalf("ensure 出错: %v", err)
	}
	if f.mkdirs() != 1 {
		t.Fatalf("同名项是文件时应该新建目录，实际建了 %d 次", f.mkdirs())
	}
	if got == "c1" {
		t.Fatal("不该把同名文件的 id 当成目录 cid")
	}
}

// 逐级下钻：中间已存在的目录走缓存，最里层才新建。
func TestDirCacheNested(t *testing.T) {
	f := newFakeDirs()
	show := f.add("root", "剧名 (2023)", true)
	f.add(show, "Season 1", true)
	d := newDirCache(f)

	got, err := d.ensure(context.Background(), "root", []string{"剧名 (2023)", "Season 1"})
	if err != nil {
		t.Fatalf("ensure 出错: %v", err)
	}
	if f.mkdirs() != 0 {
		t.Fatalf("两级都已存在，不该建目录，实际: %v", f.mkdirLog)
	}
	if f.listCall["root"] != 1 || f.listCall[show] != 1 {
		t.Fatalf("应该各列一次，实际 root=%d 剧名=%d", f.listCall["root"], f.listCall[show])
	}

	// 再来一次同一目标：两级都命中缓存，一次列表都不该发生。
	before := f.listCall["root"] + f.listCall[show]
	if _, err := d.ensure(context.Background(), "root", []string{"剧名 (2023)", "Season 1"}); err != nil {
		t.Fatalf("二次 ensure 出错: %v", err)
	}
	if after := f.listCall["root"] + f.listCall[show]; after != before {
		t.Fatalf("缓存命中时不该再列目录，列表次数 %d → %d", before, after)
	}
	if got != f.tree[show][0].ID {
		t.Fatalf("应返回到 Season 1 的 cid，实际 %q", got)
	}
}

// segs 为空（目标就是库根目录）时不该发任何请求。
func TestDirCacheNoSegs(t *testing.T) {
	f := newFakeDirs()
	d := newDirCache(f)

	got, err := d.ensure(context.Background(), "root", nil)
	if err != nil {
		t.Fatalf("ensure 出错: %v", err)
	}
	if got != "root" {
		t.Fatalf("空 segs 应返回 root，实际 %q", got)
	}
	if len(f.listCall) != 0 || f.mkdirs() != 0 {
		t.Fatalf("空 segs 不该发请求，list=%v mkdir=%v", f.listCall, f.mkdirLog)
	}
}

// 空白段（片名清洗后可能留下空串）跳过，不建空名字目录。
func TestDirCacheSkipsBlankSegs(t *testing.T) {
	f := newFakeDirs()
	d := newDirCache(f)

	got, err := d.ensure(context.Background(), "root", []string{"", "   ", "片名 (2024)"})
	if err != nil {
		t.Fatalf("ensure 出错: %v", err)
	}
	if f.mkdirs() != 1 {
		t.Fatalf("空白段应被跳过，只建 1 个目录，实际: %v", f.mkdirLog)
	}
	if f.mkdirLog[0] != "root/片名 (2024)" {
		t.Fatalf("建错了目录: %s", f.mkdirLog[0])
	}
	if got == "" || got == "root" {
		t.Fatalf("应返回新建目录的 cid，实际 %q", got)
	}
}

// 列目录失败要原样往上报，不能当成「目录不存在」去建。
func TestDirCacheListError(t *testing.T) {
	f := newFakeDirs()
	f.listErr = errors.New("115: 请求过于频繁")
	d := newDirCache(f)

	if _, err := d.ensure(context.Background(), "root", []string{"片名 (2024)"}); err == nil {
		t.Fatal("列目录失败时应该返回错误")
	}
	if f.mkdirs() != 0 {
		t.Fatalf("列目录失败后不该继续建目录，实际: %v", f.mkdirLog)
	}
}

// 建目录失败同样往上报，且不把失败的 cid 写进缓存。
func TestDirCacheMkdirError(t *testing.T) {
	f := newFakeDirs()
	f.mkdirErr = errors.New("115: 空间不足")
	d := newDirCache(f)

	if _, err := d.ensure(context.Background(), "root", []string{"片名 (2024)"}); err == nil {
		t.Fatal("建目录失败时应该返回错误")
	}
	if kids := d.kids["root"]; len(kids) != 0 {
		t.Fatalf("失败的目录不该进缓存，实际: %v", kids)
	}
}
