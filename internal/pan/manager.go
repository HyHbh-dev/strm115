package pan

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Options 是节流与熔断参数。默认值刻意保守：115 的风控是按「短时间内的请求密度」
// 判的，宁可慢一点也不要被封号——被封一次通常要等几小时到一天。
type Options struct {
	// MinGap 同一账号任意两次请求之间的最小间隔。
	MinGap time.Duration
	// MaxAttempts 临时故障的最大尝试次数（含首次）。
	MaxAttempts int
	// RiskCooldown 触发风控后的冷却时长，冷却期内所有调用直接失败。
	RiskCooldown time.Duration
	// RateLimitBackoff 风控前兆（比如偶发一次「操作频繁」）的额外退避。
	RateLimitBackoff time.Duration
	Logf             func(string, ...any)
	now              func() time.Time
	sleep            func(context.Context, time.Duration) error
}

func (o *Options) fill() {
	if o.MinGap <= 0 {
		o.MinGap = 1200 * time.Millisecond
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.RiskCooldown <= 0 {
		o.RiskCooldown = 10 * time.Minute
	}
	if o.RateLimitBackoff <= 0 {
		o.RateLimitBackoff = 30 * time.Second
	}
	if o.Logf == nil {
		o.Logf = func(f string, a ...any) { slog.Warn(f, a...) }
	}
	if o.now == nil {
		o.now = time.Now
	}
	if o.sleep == nil {
		o.sleep = sleepCtx
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Stats 是给界面看的运行时指标。
type Stats struct {
	Requests  atomic.Int64
	Retries   atomic.Int64
	RiskHits  atomic.Int64
	CoolDowns atomic.Int64
	// 当日调用次数（本地时间零点翻篇）。Requests 是进程生命周期的总量，
	// 那个数字对「今天被限流了吗」这个问题没有意义。
	Today    atomic.Int64
	todayDay atomic.Value // string：这一份计数属于哪一天
}

// Snapshot 是 Stats 的可序列化快照。
type Snapshot struct {
	Requests      int64     `json:"requests"`
	RequestsToday int64     `json:"requests_today"` // 今天（本地时间）调了 115 多少次
	Day           string    `json:"day"`            // 上面那个数字属于哪天，YYYY-MM-DD
	Retries       int64     `json:"retries"`
	RiskHits      int64     `json:"risk_hits"`
	CoolDowns     int64     `json:"cooldowns"`
	CooldownUntil time.Time `json:"cooldown_until"`
	LastError     string    `json:"last_error"`
	LastAt        time.Time `json:"last_at"`
}

// Manager 是所有网盘调用的唯一入口。
//
// 它做四件事，缺一不可：
//   - 串行：同一账号同时只允许一个在途请求（网盘侧不会因为并发而变快，只会更快被限）
//   - 节流：请求之间强制 MinGap
//   - 重试：只重试临时故障，指数退避
//   - 熔断：一旦判定被风控，整段时间内不再发请求，让业务层拿到明确错误而不是雪崩
type Manager struct {
	drv  Driver
	acct Account
	opt  Options

	sem       chan struct{} // 串行闸门
	mu        sync.Mutex    // 保护下面几个字段
	last      time.Time
	coolUntil time.Time
	lastErr   string
	lastAt    time.Time
	stat      Stats
}

func NewManager(drv Driver, acct Account, opt Options) *Manager {
	opt.fill()
	return &Manager{drv: drv, acct: acct, opt: opt, sem: make(chan struct{}, 1)}
}

func (m *Manager) Account() Account { return m.acct }
func (m *Manager) Driver() Driver   { return m.drv }
func (m *Manager) Close() error     { return m.drv.Close() }

// Snapshot 返回指标快照。
func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	today := m.today()
	todayN := int64(0)
	if d, _ := m.stat.todayDay.Load().(string); d == today {
		todayN = m.stat.Today.Load()
	}
	return Snapshot{
		Requests:      m.stat.Requests.Load(),
		RequestsToday: todayN,
		Day:           today,
		Retries:       m.stat.Retries.Load(),
		RiskHits:      m.stat.RiskHits.Load(),
		CoolDowns:     m.stat.CoolDowns.Load(),
		CooldownUntil: m.coolUntil,
		LastError:     m.lastErr,
		LastAt:        m.lastAt,
	}
}

// today 是「当日」的口径：本地时区的 YYYY-MM-DD。用 UTC 会让晚上 8 点之后的
// 调用算到「明天」，用户对着北京的墙钟看这个数字会对不上。
func (m *Manager) today() string { return m.opt.now().Format("2006-01-02") }

// bumpToday 记一次调用，跨天自动从 0 开始。
func (m *Manager) bumpToday() {
	day := m.today()
	if cur, _ := m.stat.todayDay.Load().(string); cur != day {
		m.stat.todayDay.Store(day)
		m.stat.Today.Store(1)
		return
	}
	m.stat.Today.Add(1)
}

// SeedToday 用落库的数字接上今天的计数（进程重启后不归零）。
// 只在账号第一次被取出来时调用一次。
func (m *Manager) SeedToday(day string, n int64) {
	if n <= 0 || day != m.today() {
		return
	}
	if cur, _ := m.stat.todayDay.Load().(string); cur == day {
		return // 已经在数了，别覆盖
	}
	m.stat.todayDay.Store(day)
	m.stat.Today.Store(n)
}

// enter 取得唯一一个请求名额，并按最小间隔等待。
// 返回的 release 必须调用（defer）。
func (m *Manager) enter(ctx context.Context, op string) (release func(), err error) {
	m.mu.Lock()
	if m.opt.now().Before(m.coolUntil) {
		until := m.coolUntil
		m.mu.Unlock()
		return nil, wrap(ErrCooling, op, "风控冷却中，恢复时间 "+until.Format("15:04:05"), nil)
	}
	m.mu.Unlock()

	select {
	case m.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, wrap(ErrTransient, op, "等待请求名额时取消", ctx.Err())
	}

	release = func() { <-m.sem }

	m.mu.Lock()
	wait := m.opt.MinGap - m.opt.now().Sub(m.last)
	m.mu.Unlock()
	if wait > 0 {
		if serr := m.opt.sleep(ctx, wait); serr != nil {
			release()
			return nil, wrap(ErrTransient, op, "节流等待时取消", serr)
		}
	}

	m.mu.Lock()
	m.last = m.opt.now()
	m.lastAt = m.last
	m.mu.Unlock()
	m.stat.Requests.Add(1)
	m.bumpToday()
	return release, nil
}

func (m *Manager) note(kind error, err error) {
	m.mu.Lock()
	if err != nil {
		m.lastErr = err.Error()
	}
	m.mu.Unlock()
	switch kind {
	case ErrRisk:
		m.stat.RiskHits.Add(1)
	case ErrTransient:
		// 不计入 risk
	}
}

func (m *Manager) trip(op string, d time.Duration) {
	m.mu.Lock()
	until := m.opt.now().Add(d)
	if until.After(m.coolUntil) {
		m.coolUntil = until
	}
	m.mu.Unlock()
	m.stat.CoolDowns.Add(1)
	m.opt.Logf("[pan] 熔断 %s：%s 内不再请求该账号", op, d)
}

// call 是唯一的执行通道。
func call[T any](ctx context.Context, m *Manager, op string, fn func(context.Context) (T, error)) (T, error) {
	var zero T

	for attempt := 1; ; attempt++ {
		release, err := m.enter(ctx, op)
		if err != nil {
			return zero, err
		}
		v, err := fn(ctx)
		release()

		if err == nil {
			m.note(nil, nil)
			return v, nil
		}

		kind := KindOf(err)
		m.note(kind, err)

		switch kind {
		case ErrAuth, ErrNotFound, ErrUnsupported, ErrRejected:
			return zero, err // 重试无意义
		case ErrRisk:
			// 立刻熔断：紧接着重试只会让风控窗口变长
			m.trip(op, m.opt.RiskCooldown)
			return zero, err
		}

		if attempt >= m.opt.MaxAttempts {
			if kind == ErrTransient && attempt > 1 {
				// 连续临时故障：短暂降速，不熔断（网络抖动不该让界面停摆十分钟）
				m.trip(op, m.opt.RateLimitBackoff)
			}
			return zero, err
		}

		m.stat.Retries.Add(1)
		backoff := time.Duration(attempt) * time.Second
		m.opt.Logf("[pan] %s 第 %d 次失败（%v），%s 后重试", op, attempt, err, backoff)
		if serr := m.opt.sleep(ctx, backoff); serr != nil {
			return zero, wrap(ErrTransient, op, "重试等待时取消", serr)
		}
	}
}

// ==================== 对外方法 ====================

// pageLimiter 由「每页条数有硬上限」的驱动实现。
//
// 夸克 TV 的 /file?method=list 要求 page_size 在 1–100，而上层列目录统一
// 用 200（115 一页能拿 200 条）。以前这个 200 会原样塞进夸克请求，
// 接口直接回 "page_size 范围: 1-100"，界面上就变成一句莫名其妙的报错。
// 让驱动自己声明上限，Manager 在这里兜底，调用方不必知道各家的脾气。
type pageLimiter interface{ MaxPageSize() int }

// clampPageSize 把调用方给的每页条数收敛到驱动能接受的范围。
// 不只是 ListAll 要用：sync 全量同步、整理扫描等都会直接调 ListPage，
// 只在 ListAll 里 clamp 的话这些路径仍会把 200 透给夸克。
func (m *Manager) clampPageSize(limit int) int {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	if lim, ok := m.drv.(pageLimiter); ok {
		if max := lim.MaxPageSize(); max > 0 && limit > max {
			limit = max
		}
	}
	return limit
}

func (m *Manager) ListPage(ctx context.Context, dirID string, offset, limit int) ([]Entry, int, error) {
	limit = m.clampPageSize(limit)
	type res struct {
		entries []Entry
		total   int
	}
	r, err := call(ctx, m, "列目录", func(ctx context.Context) (res, error) {
		e, t, err := m.drv.ListPage(ctx, dirID, offset, limit)
		return res{e, t}, err
	})
	return r.entries, r.total, err
}

// ListAll 把一个目录列全。仅用于「目录不大」的场景（选目录、整理时的单目录扫描）；
// 全量同步请用 ListPage 自己翻页，才能控制内存。
func (m *Manager) ListAll(ctx context.Context, dirID string, pageSize int) ([]Entry, error) {
	pageSize = m.clampPageSize(pageSize)
	var out []Entry
	for offset := 0; ; {
		entries, total, err := m.ListPage(ctx, dirID, offset, pageSize)
		if err != nil {
			return out, err
		}
		out = append(out, entries...)
		offset += len(entries)
		if len(entries) == 0 || len(entries) < pageSize || (total >= 0 && offset >= total) {
			return out, nil
		}
	}
}

func (m *Manager) Stat(ctx context.Context, id string) (Entry, error) {
	return call(ctx, m, "查条目", func(ctx context.Context) (Entry, error) { return m.drv.Stat(ctx, id) })
}

func (m *Manager) DownloadURL(ctx context.Context, pickCode, ua string) (string, error) {
	return call(ctx, m, "取直链", func(ctx context.Context) (string, error) {
		return m.drv.DownloadURL(ctx, pickCode, ua)
	})
}

func (m *Manager) Mkdir(ctx context.Context, parentID, name string) (string, error) {
	return call(ctx, m, "建目录", func(ctx context.Context) (string, error) {
		return m.drv.Mkdir(ctx, parentID, name)
	})
}

func (m *Manager) Move(ctx context.Context, toDirID string, ids ...string) (int, error) {
	return call(ctx, m, "移动", func(ctx context.Context) (int, error) {
		if err := m.drv.Move(ctx, toDirID, ids...); err != nil {
			return 0, err
		}
		return len(ids), nil
	})
}

func (m *Manager) Rename(ctx context.Context, id, name string) (int, error) {
	return call(ctx, m, "改名", func(ctx context.Context) (int, error) {
		if err := m.drv.Rename(ctx, id, name); err != nil {
			return 0, err
		}
		return 1, nil
	})
}

func (m *Manager) Remove(ctx context.Context, ids ...string) (int, error) {
	return call(ctx, m, "删除", func(ctx context.Context) (int, error) {
		if err := m.drv.Remove(ctx, ids...); err != nil {
			return 0, err
		}
		return len(ids), nil
	})
}

func (m *Manager) Ping(ctx context.Context) error {
	_, err := call(ctx, m, "校验凭证", func(ctx context.Context) (struct{}, error) { return struct{}{}, m.drv.Ping(ctx) })
	return err
}

// ==================== 离线下载（可选能力） ====================

// SupportsOffline 报告当前网盘能不能离线下载。前端据此决定要不要显示入口。
func (m *Manager) SupportsOffline() bool {
	_, ok := m.drv.(OfflineDownloader)
	return ok
}

// AddOffline 提交一批链接到离线下载。返回每个链接对应的任务标识。
//
// 顺序提交、逐条记账：115 网页端的 add_task_url 一次只收一条链接，批量提交是
// 上层的事。中途某条被拒时把已经成功的 hash 一起返回，调用方至少知道哪些进了队列。
func (m *Manager) AddOffline(ctx context.Context, uris []string, saveDirID string) ([]string, error) {
	od, ok := m.drv.(OfflineDownloader)
	if !ok {
		return nil, wrap(ErrUnsupported, "离线下载", "当前网盘不支持离线下载", nil)
	}
	if saveDirID == "" {
		saveDirID = "0" // 115 的根目录
	}
	return call(ctx, m, "离线下载", func(ctx context.Context) ([]string, error) {
		return od.AddOfflineURIs(ctx, uris, saveDirID)
	})
}

// ListOfflineTasks 列一页离线任务。
func (m *Manager) ListOfflineTasks(ctx context.Context, page int64) ([]OfflineTask, int64, error) {
	od, ok := m.drv.(OfflineDownloader)
	if !ok {
		return nil, 0, wrap(ErrUnsupported, "列离线任务", "当前网盘不支持离线下载", nil)
	}
	type res struct {
		tasks []OfflineTask
		total int64
	}
	r, err := call(ctx, m, "列离线任务", func(ctx context.Context) (res, error) {
		t, n, err := od.ListOfflineTasks(ctx, page)
		return res{t, n}, err
	})
	return r.tasks, r.total, err
}

// ==================== 分享转存（可选能力） ====================

// SupportsShareSave 报告当前网盘能不能把别人的分享转存进来。
func (m *Manager) SupportsShareSave() bool {
	_, ok := m.drv.(ShareSaver)
	return ok
}

// SaveShare 把一条分享转存到自己的网盘。
//
// 「之前转存过」不算错误（Already=true），但也不能重试：再发一次还是同一句
// 「已经转存过」，白白多一次请求，还会往风控计数里加一笔。
func (m *Manager) SaveShare(ctx context.Context, shareCode, password, toDirID string) (ShareSaveResult, error) {
	ss, ok := m.drv.(ShareSaver)
	if !ok {
		return ShareSaveResult{}, wrap(ErrUnsupported, "转存分享", "当前网盘不支持分享转存", nil)
	}
	if toDirID == "" {
		toDirID = "0" // 115 的根目录
	}
	return call(ctx, m, "转存分享", func(ctx context.Context) (ShareSaveResult, error) {
		return ss.SaveShare(ctx, shareCode, password, toDirID)
	})
}
