// Package panpool 按账号缓存 pan.Manager。
//
// 为什么必须缓存而不是每次新建：节流状态（last request time、熔断截止时间）都在
// Manager 里。每次请求新建一个 Manager，等于每次都是「第一次请求」，
// 最小间隔就完全失效了——那正是 115 风控最爱抓的模式。
package panpool

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"strm115/internal/pan"
	"strm115/internal/store"
)

type Pool struct {
	st  *store.Store
	opt pan.Options

	mu sync.Mutex
	m  map[int64]*pan.Manager
}

func New(st *store.Store, opt pan.Options) *Pool {
	return &Pool{st: st, opt: opt, m: make(map[int64]*pan.Manager, 4)}
}

// Get 返回该账号的受控客户端。调用方不要 Close 它——生命周期归 Pool。
func (p *Pool) Get(ctx context.Context, accountID int64) (*pan.Manager, error) {
	p.mu.Lock()
	if m, ok := p.m[accountID]; ok {
		p.mu.Unlock()
		return m, nil
	}
	p.mu.Unlock()

	acct, err := p.st.GetAccount(ctx, accountID)
	if err != nil {
		return nil, err
	}
	drv, err := pan.Open(ctx, pan.Account{
		ID:         acct.ID,
		Kind:       acct.Kind,
		Name:       acct.Name,
		Credential: acct.Credential,
	})
	if err != nil {
		return nil, err
	}
	mgr := pan.NewManager(drv, pan.Account{ID: acct.ID, Kind: acct.Kind, Name: acct.Name}, p.opt)
	// 接上今天已经用掉的调用量（进程重启后不归零）。只在建 Manager 时查一次，
	// 之后由内存计数兜着，按分钟落库。
	mgr.SeedToday(p.today(), p.todayCalls(ctx, accountID))

	p.mu.Lock()
	defer p.mu.Unlock()
	if old, ok := p.m[accountID]; ok {
		// 并发下别人先放进去了，用别人的，把自己这个关掉
		mgr.Close()
		return old, nil
	}
	p.m[accountID] = mgr
	return mgr, nil
}

// Evict 在账号被删除或凭证更新后调用，丢掉旧的连接与节流状态。
func (p *Pool) Evict(accountID int64) {
	p.mu.Lock()
	m, ok := p.m[accountID]
	delete(p.m, accountID)
	p.mu.Unlock()
	if ok {
		m.Close()
	}
}

func (p *Pool) CloseAll() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, m := range p.m {
		m.Close()
		delete(p.m, id)
	}
}

// Snapshot 给界面显示某个账号的请求计数与冷却状态。
func (p *Pool) Snapshot(accountID int64) (pan.Snapshot, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if m, ok := p.m[accountID]; ok {
		return m.Snapshot(), true
	}
	return pan.Snapshot{}, false
}

// ==================== 当日 115 调用量落库 ====================
//
// 计数本身在 pan.Manager 里（每次真正发请求时 +1），这里只负责「定期写回」和
// 「重启后接上」。为什么不每次请求都写库：115 请求是每秒个位数，但写库要抢锁，
// 而且这个数字丢了最多重算一天，不值得为它把热路径变慢。

// today 是「当日」的口径：本地时区。用 UTC 会让晚上 8 点之后的调用算到明天，
// 用户对着墙钟看这个数字会觉得是错的。
func (p *Pool) today() string { return time.Now().Format("2006-01-02") }

func (p *Pool) todayCalls(ctx context.Context, accountID int64) int64 {
	if p.st == nil {
		return 0
	}
	m, err := p.st.APICallsOn(ctx, p.today())
	if err != nil {
		return 0
	}
	return m[accountID]
}

// Start 起后台落库循环。和 Pool 同生命周期，ctx 取消时再补最后一次。
func (p *Pool) Start(ctx context.Context) {
	if p.st == nil {
		return
	}
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				p.flush(context.Background())
				return
			case <-t.C:
				p.flush(ctx)
			}
		}
	}()
}

func (p *Pool) flush(ctx context.Context) {
	p.mu.Lock()
	ms := make([]*pan.Manager, 0, len(p.m))
	for _, m := range p.m {
		ms = append(ms, m)
	}
	p.mu.Unlock()
	for _, m := range ms {
		s := m.Snapshot()
		if s.Day == "" || s.RequestsToday <= 0 {
			continue
		}
		if err := p.st.SetAPICalls(ctx, s.Day, m.Account().ID, s.RequestsToday); err != nil {
			// 写不进去不影响业务，下次还会再写一遍（覆盖语义，天然幂等）
			slog.Warn("115 调用量落库失败", "account", m.Account().ID, "err", err)
		}
	}
}
