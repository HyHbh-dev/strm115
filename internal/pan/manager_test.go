package pan

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeDriver 是一个可控的 Driver，用来验证 Manager 的节流/重试/熔断。
type fakeDriver struct {
	mu    sync.Mutex
	calls int
	// 每次调用返回什么：按顺序消费；用完后一直返回最后一个
	errs []error
	dirs [][]Entry
}

func (f *fakeDriver) Kind() string { return "fake" }

func (f *fakeDriver) next() ([]Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if i < len(f.errs) {
		return nil, f.errs[i]
	}
	return []Entry{{ID: "x"}}, nil
}

func (f *fakeDriver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDriver) ListPage(ctx context.Context, dirID string, offset, limit int) ([]Entry, int, error) {
	e, err := f.next()
	return e, -1, err
}
func (f *fakeDriver) Stat(ctx context.Context, id string) (Entry, error) {
	_, err := f.next()
	return Entry{ID: id}, err
}
func (f *fakeDriver) DownloadURL(ctx context.Context, pc, ua string) (string, error) {
	_, err := f.next()
	return "http://cdn/" + pc, err
}
func (f *fakeDriver) Mkdir(ctx context.Context, parent, name string) (string, error) {
	_, err := f.next()
	return "new", err
}
func (f *fakeDriver) Move(ctx context.Context, to string, ids ...string) error {
	_, err := f.next()
	return err
}
func (f *fakeDriver) Rename(ctx context.Context, id, name string) error {
	_, err := f.next()
	return err
}
func (f *fakeDriver) Remove(ctx context.Context, ids ...string) error {
	_, err := f.next()
	return err
}
func (f *fakeDriver) Ping(ctx context.Context) error {
	_, err := f.next()
	return err
}
func (f *fakeDriver) Close() error { return nil }

// recordingDriver 记下每次 ListPage 收到的 limit，用来验分页大小有没有被收敛。
type recordingDriver struct {
	fakeDriver
	limits []int
}

func (d *recordingDriver) ListPage(ctx context.Context, dirID string, offset, limit int) ([]Entry, int, error) {
	d.limits = append(d.limits, limit)
	e, err := d.next()
	return e, -1, err
}

// cappedDriver 模拟「每页条数有硬上限」的驱动（夸克 TV 只收 1–100）。
type cappedDriver struct{ recordingDriver }

func (c *cappedDriver) MaxPageSize() int { return 100 }

func TestListAllRespectsDriverMaxPageSize(t *testing.T) {
	d := &cappedDriver{}
	m, _ := newTestManager(d, Options{MinGap: time.Millisecond, MaxAttempts: 1})
	if _, err := m.ListAll(context.Background(), "0", 200); err != nil {
		t.Fatal(err)
	}
	if len(d.limits) == 0 {
		t.Fatal("ListAll 没有调用驱动的 ListPage")
	}
	for i, got := range d.limits {
		if got != 100 {
			t.Errorf("第 %d 页 pageSize=%d，夸克驱动最多只接受 100", i+1, got)
		}
	}
}

func TestListPageRespectsDriverMaxPageSize(t *testing.T) {
	// sync / organize 不走 ListAll，直接调 ListPage 翻页，也必须收敛。
	d := &cappedDriver{}
	m, _ := newTestManager(d, Options{MinGap: time.Millisecond, MaxAttempts: 1})
	if _, _, err := m.ListPage(context.Background(), "0", 0, 200); err != nil {
		t.Fatal(err)
	}
	if len(d.limits) != 1 || d.limits[0] != 100 {
		t.Errorf("直接调 ListPage 也应收敛到 100，实际 %v", d.limits)
	}
}

func TestListAllKeepsPageSizeForUnlimitedDriver(t *testing.T) {
	// 115 没实现 MaxPageSize，不该被无端降级到 100。
	d := &recordingDriver{}
	m, _ := newTestManager(d, Options{MinGap: time.Millisecond, MaxAttempts: 1})
	if _, err := m.ListAll(context.Background(), "0", 200); err != nil {
		t.Fatal(err)
	}
	if len(d.limits) != 1 || d.limits[0] != 200 {
		t.Errorf("115 应继续用 200，实际 %v", d.limits)
	}
}

// newTestManager 把 sleep 换成立即返回（并记录），否则测试要跑真实时间。
func newTestManager(d Driver, opt Options) (*Manager, *[]time.Duration) {
	var slept []time.Duration
	opt.now = time.Now
	opt.sleep = func(ctx context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}
	opt.Logf = func(string, ...any) {}
	return NewManager(d, Account{ID: 1, Kind: "fake"}, opt), &slept
}

func TestMinGapBetweenRequests(t *testing.T) {
	f := &fakeDriver{}
	m, slept := newTestManager(f, Options{MinGap: 1200 * time.Millisecond, MaxAttempts: 1})

	ctx := context.Background()
	if _, _, err := m.ListPage(ctx, "0", 0, 10); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 0 {
		t.Fatalf("首次请求不该等待，实际等了 %v", *slept)
	}
	if _, _, err := m.ListPage(ctx, "0", 0, 10); err != nil {
		t.Fatal(err)
	}
	if len(*slept) != 1 || (*slept)[0] < 900*time.Millisecond {
		t.Fatalf("第二次请求应等待接近 1200ms，实际 %v", *slept)
	}
}

func TestRiskErrorTripsBreakerAndNeverRetries(t *testing.T) {
	risk := NewErr(ErrRisk, "列目录", "服务器开小差了", nil)
	f := &fakeDriver{errs: []error{risk, nil}}
	m, _ := newTestManager(f, Options{MinGap: time.Millisecond, MaxAttempts: 3, RiskCooldown: time.Minute})

	ctx := context.Background()
	_, _, err := m.ListPage(ctx, "0", 0, 10)
	if !errors.Is(err, ErrRisk) {
		t.Fatalf("应返回 ErrRisk，实际 %v", err)
	}
	if f.callCount() != 1 {
		t.Fatalf("风控错误不该重试，driver 被调用 %d 次", f.callCount())
	}

	// 熔断期内：不发请求，直接本地失败
	_, _, err = m.ListPage(ctx, "0", 0, 10)
	if !errors.Is(err, ErrCooling) {
		t.Fatalf("熔断期内应返回 ErrCooling，实际 %v", err)
	}
	if f.callCount() != 1 {
		t.Fatalf("熔断期内不该再调用 driver，实际 %d 次", f.callCount())
	}
}

func TestTransientRetriesThenSucceeds(t *testing.T) {
	f := &fakeDriver{errs: []error{
		NewErr(ErrTransient, "列目录", "timeout", nil),
		NewErr(ErrTransient, "列目录", "timeout", nil),
		nil,
	}}
	m, slept := newTestManager(f, Options{MinGap: time.Millisecond, MaxAttempts: 3})

	ctx := context.Background()
	if _, _, err := m.ListPage(ctx, "0", 0, 10); err != nil {
		t.Fatalf("第三次应该成功，实际 %v", err)
	}
	if f.callCount() != 3 {
		t.Fatalf("应调用 3 次，实际 %d", f.callCount())
	}
	if len(*slept) < 2 {
		t.Fatalf("应有退避等待，实际 %v", *slept)
	}
}

func TestAuthErrorDoesNotRetry(t *testing.T) {
	f := &fakeDriver{errs: []error{NewErr(ErrAuth, "列目录", "cookie 失效", nil), nil}}
	m, _ := newTestManager(f, Options{MinGap: time.Millisecond, MaxAttempts: 3})

	_, _, err := m.ListPage(context.Background(), "0", 0, 10)
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("应返回 ErrAuth，实际 %v", err)
	}
	if f.callCount() != 1 {
		t.Fatalf("凭证失效不该重试，实际 %d 次", f.callCount())
	}
}

func TestListAllPaginates(t *testing.T) {
	f := &fakeDriver{}
	m, _ := newTestManager(f, Options{MinGap: time.Millisecond, MaxAttempts: 1})

	// fake 每页返回 1 条且 len < pageSize，应停在一页
	got, err := m.ListAll(context.Background(), "0", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || f.callCount() != 1 {
		t.Fatalf("ListAll 翻页逻辑不对：entries=%d calls=%d", len(got), f.callCount())
	}
}

func TestKindOfUnknownIsTransient(t *testing.T) {
	if k := KindOf(errors.New("something")); k != ErrTransient {
		t.Fatalf("未知错误应归为 ErrTransient，实际 %v", k)
	}
}

// TestSnapshotCountsToday 当日计数：Requests 是进程总量，RequestsToday 按本地
// 日期翻篇。用户看的是后者 —— 115 的风控额度按天算。
func TestSnapshotCountsToday(t *testing.T) {
	f := &fakeDriver{}
	now := time.Date(2026, 11, 8, 23, 30, 0, 0, time.Local)
	opt := Options{MinGap: time.Millisecond, MaxAttempts: 1}
	opt.now = func() time.Time { return now }
	opt.sleep = func(context.Context, time.Duration) error { return nil }
	opt.Logf = func(string, ...any) {}
	m := NewManager(f, Account{ID: 1, Kind: "fake"}, opt)

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, _, err := m.ListPage(ctx, "0", 0, 10); err != nil {
			t.Fatal(err)
		}
	}
	s := m.Snapshot()
	if s.RequestsToday != 2 || s.Day != "2026-11-08" {
		t.Fatalf("当天调用次数不对: today=%d day=%s", s.RequestsToday, s.Day)
	}

	// 跨零点：真正的调用才把计数带到新的一天，不能靠 Snapshot 自己翻篇
	now = now.Add(2 * time.Hour) // 11-09 01:30
	if got := m.Snapshot(); got.RequestsToday != 0 || got.Day != "2026-11-09" {
		t.Fatalf("跨天还没调用就应当显示 0: today=%d day=%s", got.RequestsToday, got.Day)
	}
	if _, _, err := m.ListPage(ctx, "0", 0, 10); err != nil {
		t.Fatal(err)
	}
	s = m.Snapshot()
	if s.RequestsToday != 1 || s.Requests != 3 {
		t.Fatalf("第二天应从 1 重新数、总量继续累加: today=%d total=%d", s.RequestsToday, s.Requests)
	}
}

// TestSeedToday 重启后把库里今天的数接上，而且只在同一天生效。
func TestSeedToday(t *testing.T) {
	f := &fakeDriver{}
	now := time.Date(2026, 11, 8, 9, 0, 0, 0, time.Local)
	opt := Options{MinGap: time.Millisecond, MaxAttempts: 1}
	opt.now = func() time.Time { return now }
	opt.sleep = func(context.Context, time.Duration) error { return nil }
	opt.Logf = func(string, ...any) {}
	m := NewManager(f, Account{ID: 1, Kind: "fake"}, opt)

	m.SeedToday("2026-11-07", 88) // 昨天的账不要
	if s := m.Snapshot(); s.RequestsToday != 0 {
		t.Fatalf("昨天的数不该接进来: %d", s.RequestsToday)
	}
	m.SeedToday("2026-11-08", 41)
	if s := m.Snapshot(); s.RequestsToday != 41 {
		t.Fatalf("今天的数没接上: %d", s.RequestsToday)
	}
	if _, _, err := m.ListPage(context.Background(), "0", 0, 10); err != nil {
		t.Fatal(err)
	}
	if s := m.Snapshot(); s.RequestsToday != 42 {
		t.Fatalf("接上之后应当继续累加: %d", s.RequestsToday)
	}
	// 已经在数了就不该被覆盖
	m.SeedToday("2026-11-08", 7)
	if s := m.Snapshot(); s.RequestsToday != 42 {
		t.Fatalf("重复 seed 覆盖了真实计数: %d", s.RequestsToday)
	}
}
