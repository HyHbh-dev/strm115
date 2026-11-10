package offline

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"strm115/internal/pan"
)

// fakePan 冒充一个支持离线下载的网盘：提交时给每个链接发一个假 hash，
// 列表接口原样吐回来，测试通过改 status 来模拟下载进展。
type fakePan struct {
	mu    sync.Mutex
	tasks map[string]pan.OfflineTask
	order []string
	saved string
	page  int
}

func newFakePan() *fakePan {
	return &fakePan{tasks: map[string]pan.OfflineTask{}}
}

func (f *fakePan) SupportsOffline() bool { return true }

func (f *fakePan) AddOffline(ctx context.Context, uris []string, dirID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.saved = dirID
	hashes := make([]string, 0, len(uris))
	for i, u := range uris {
		h := fmt.Sprintf("hash-%d", len(f.order)+i)
		f.tasks[h] = pan.OfflineTask{Hash: h, Name: u, Status: pan.OfflineWaiting}
		f.order = append(f.order, h)
		hashes = append(hashes, h)
	}
	return hashes, nil
}

func (f *fakePan) ListOfflineTasks(ctx context.Context, page int64) ([]pan.OfflineTask, int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.page++
	out := make([]pan.OfflineTask, 0, len(f.order))
	for _, h := range f.order {
		out = append(out, f.tasks[h])
	}
	return out, int64(len(out)), nil
}

func (f *fakePan) setStatus(hash string, st pan.OfflineStatus, percent float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := f.tasks[hash]
	t.Status = st
	t.Percent = percent
	f.tasks[hash] = t
}

func (f *fakePan) drop(hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.tasks, hash)
	for i, h := range f.order {
		if h == hash {
			f.order = append(f.order[:i], f.order[i+1:]...)
			break
		}
	}
}

func testService(f *fakePan) *Service {
	return newService(
		func(context.Context, int64) (client, error) { return f, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
}

// TestOfflineLifecycle 走一遍「提交 → 下载中 → 完成 → 回调」。
func TestOfflineLifecycle(t *testing.T) {
	f := newFakePan()
	s := testService(f)
	defer s.Close()

	events := make(chan Event, 4)
	s.OnDone(func(e Event) { events <- e })

	ctx := context.Background()
	hashes, err := s.Add(ctx, 7, []string{"magnet:?xt=urn:btih:aaa", "ed2k://|file|x|1|hash|/"}, "dir-42", "沙丘2")
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if len(hashes) != 2 {
		t.Fatalf("应该拿到 2 个 hash，实际 %v", hashes)
	}
	if f.saved != "dir-42" {
		t.Fatalf("保存目录没传下去: %q", f.saved)
	}
	if s.Watching() != 2 {
		t.Fatalf("应该盯着 2 个任务，实际 %d", s.Watching())
	}

	// 下载中：不回调，但状态要更新
	f.setStatus(hashes[0], pan.OfflineRunning, 40)
	s.pollOnce()
	if s.Watching() != 2 {
		t.Fatalf("下载中不该摘掉任务，实际剩 %d", s.Watching())
	}
	select {
	case e := <-events:
		t.Fatalf("下载中不该回调: %+v", e)
	default:
	}

	// 完成：回调一次，任务摘掉
	f.setStatus(hashes[0], pan.OfflineDone, 100)
	s.pollOnce()
	select {
	case e := <-events:
		if e.AccountID != 7 || e.Task.Hash != hashes[0] || e.DirID != "dir-42" || e.Label != "沙丘2" {
			t.Fatalf("回调内容不对: %+v", e)
		}
		if e.Task.Status != pan.OfflineDone {
			t.Fatalf("状态不对: %v", e.Task.Status)
		}
	default:
		t.Fatal("完成时应该有回调")
	}
	if s.Watching() != 1 {
		t.Fatalf("完成后应只剩 1 个在盯，实际 %d", s.Watching())
	}

	// 再轮一次不应重复回调
	s.pollOnce()
	select {
	case e := <-events:
		t.Fatalf("不该重复回调: %+v", e)
	default:
	}
}

// TestOfflineFailed 失败也要回调，而且状态是 failed。
func TestOfflineFailed(t *testing.T) {
	f := newFakePan()
	s := testService(f)
	defer s.Close()

	events := make(chan Event, 1)
	s.OnDone(func(e Event) { events <- e })

	hashes, err := s.Add(context.Background(), 3, []string{"magnet:?xt=urn:btih:bbb"}, "1", "")
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	f.setStatus(hashes[0], pan.OfflineFailed, 0)
	s.pollOnce()

	select {
	case e := <-events:
		if e.Task.Status != pan.OfflineFailed {
			t.Fatalf("状态不对: %v", e.Task.Status)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("失败时应该有回调")
	}
}

// TestOfflineGone 任务从列表里消失（被清掉 / 页翻不到）时不能永远盯着。
func TestOfflineGone(t *testing.T) {
	f := newFakePan()
	s := testService(f)
	defer s.Close()
	s.OnDone(func(Event) { t.Error("任务消失不该触发完成回调") })

	hashes, _ := s.Add(context.Background(), 1, []string{"magnet:?xt=urn:btih:ccc"}, "1", "")
	f.drop(hashes[0])

	for i := 0; i < 12; i++ {
		s.pollOnce()
	}
	if s.Watching() != 0 {
		t.Fatalf("连续 12 轮都找不到应放弃，实际还在盯 %d 个", s.Watching())
	}
}

// TestOfflineDedupURIs 同一批里重复的链接只提交一次。
func TestOfflineDedupURIs(t *testing.T) {
	f := newFakePan()
	s := testService(f)
	defer s.Close()

	hashes, err := s.Add(context.Background(), 1,
		[]string{"magnet:?xt=urn:btih:ddd", " magnet:?xt=urn:btih:ddd ", ""}, "1", "")
	if err != nil {
		t.Fatalf("提交失败: %v", err)
	}
	if len(hashes) != 1 {
		t.Fatalf("重复链接应去重成 1 个，实际 %d", len(hashes))
	}
}

// TestOfflineEmptyURIs 空输入要明确报错，不能发请求。
func TestOfflineEmptyURIs(t *testing.T) {
	f := newFakePan()
	s := testService(f)
	defer s.Close()

	if _, err := s.Add(context.Background(), 1, []string{"  ", ""}, "1", ""); err == nil {
		t.Fatal("空链接应该报错")
	}
	if f.page != 0 {
		t.Fatal("不该发出网盘请求")
	}
}

// TestOfflineNoSupport 网盘不支持时要给明确错误，而不是空结果。
type noOfflinePan struct{ fakePan }

func (n *noOfflinePan) SupportsOffline() bool { return false }
func (n *noOfflinePan) AddOffline(context.Context, []string, string) ([]string, error) {
	panic("不该被调用")
}

func TestOfflineNoSupport(t *testing.T) {
	f := &noOfflinePan{}
	s := newService(
		func(context.Context, int64) (client, error) { return f, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer s.Close()

	if _, err := s.Add(context.Background(), 1, []string{"magnet:?xt=urn:btih:eee"}, "1", ""); err == nil {
		t.Fatal("不支持的网盘应该报错")
	}
	ok, err := s.Supports(context.Background(), 1)
	if err != nil || ok {
		t.Fatalf("Supports 应返回 false/nil，实际 %v %v", ok, err)
	}
}

// partialPan 模拟「前两条提交成功、第三条被拒」。
type partialPan struct{ fakePan }

func (p *partialPan) AddOffline(ctx context.Context, uris []string, dirID string) ([]string, error) {
	hashes, _ := p.fakePan.AddOffline(ctx, uris[:2], dirID)
	return hashes, pan.NewErr(pan.ErrRejected, "离线下载", "链接无效", nil)
}

// TestOfflinePartialSubmit 部分成功时：真实的 hash 要盯起来，错误照样往上报。
//
// 反例的代价很直接：只报错不建 watch，那两条已经进了 115 队列的任务下完了
// 没人同步、没人推送，用户以为整批都失败了。
func TestOfflinePartialSubmit(t *testing.T) {
	p := &partialPan{fakePan: *newFakePan()}
	s := newService(
		func(context.Context, int64) (client, error) { return p, nil },
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	defer s.Close()

	hashes, err := s.Add(context.Background(), 3, []string{"magnet:?xt=urn:btih:a", "magnet:?xt=urn:btih:b", "magnet:?xt=urn:btih:c"}, "9", "")
	if err == nil {
		t.Fatal("部分失败也应该返回错误")
	}
	if len(hashes) != 2 {
		t.Fatalf("应返回已提交成功的 2 个 hash，实际 %v", hashes)
	}
	if s.Watching() != 2 {
		t.Fatalf("已提交成功的任务必须继续盯着，实际盯 %d 个", s.Watching())
	}
}
