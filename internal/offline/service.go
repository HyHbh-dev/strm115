// Package offline 负责「把链接交给网盘去下」，以及盯着这些任务直到出结果。
//
// 为什么不在 HTTP handler 里直接提交：离线下载要花几分钟到几小时，任务提交完
// 请求就返回了，但用户想知道的是「下好了没」。所以这里有一个常驻的轮询器，
// 把「任务完成」变成一个事件回调（推通知、触发同步），而不是让前端一直转圈。
package offline

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/pan"
	"strm115/internal/panpool"
)

// 轮询节奏。115 的任务列表接口很轻，但也没必要更密：离线下载本身要几分钟，
// 45 秒的延迟对用户没有感知，对风控却差别很大。
const defaultTick = 45 * time.Second

// watchTimeout 单个任务最多盯多久。超过就放手 —— 115 有任务会卡在队列里
// 一整晚，没必要为此常驻一个 goroutine。
const watchTimeout = 6 * time.Hour

// Event 是一个离线任务走到终态时的事件。
type Event struct {
	AccountID int64           `json:"account_id"`
	Task      pan.OfflineTask `json:"task"`
	DirID     string          `json:"dir_id"`
	Label     string          `json:"label,omitempty"`
}

// client 是 Service 需要的那部分网盘能力。抽成接口是为了测试时能塞假的。
type client interface {
	SupportsOffline() bool
	AddOffline(ctx context.Context, uris []string, saveDirID string) ([]string, error)
	ListOfflineTasks(ctx context.Context, page int64) ([]pan.OfflineTask, int64, error)
}

// Service 提交离线任务并轮询其状态。
type Service struct {
	get  func(ctx context.Context, accountID int64) (client, error)
	log  *slog.Logger
	tick time.Duration

	mu      sync.Mutex
	watches map[string]*watch
	onDone  func(Event)

	stopOnce sync.Once
	stop     chan struct{}
	wg       sync.WaitGroup
}

type watch struct {
	accountID int64
	hash      string
	dirID     string
	label     string
	name      string
	status    pan.OfflineStatus
	misses    int
	deadline  time.Time
}

// New 用连接池创建服务，并启动轮询器。
func New(pool *panpool.Pool, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return newService(func(ctx context.Context, id int64) (client, error) {
		m, err := pool.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		return m, nil
	}, log)
}

func newService(get func(context.Context, int64) (client, error), log *slog.Logger) *Service {
	s := &Service{
		get:     get,
		log:     log,
		tick:    defaultTick,
		watches: map[string]*watch{},
		stop:    make(chan struct{}),
	}
	s.wg.Add(1)
	go s.loop()
	return s
}

// OnDone 注册终态回调。会在后台 goroutine 里被调用，实现方自己保证不阻塞。
func (s *Service) OnDone(fn func(Event)) {
	s.mu.Lock()
	s.onDone = fn
	s.mu.Unlock()
}

// Supports 报告该账号的网盘能不能离线下载。
func (s *Service) Supports(ctx context.Context, accountID int64) (bool, error) {
	c, err := s.get(ctx, accountID)
	if err != nil {
		return false, err
	}
	return c.SupportsOffline(), nil
}

// Add 提交一批链接，并把返回的任务全部纳入轮询。
func (s *Service) Add(ctx context.Context, accountID int64, uris []string, dirID, label string) ([]string, error) {
	uris = cleanURIs(uris)
	if len(uris) == 0 {
		return nil, pan.NewErr(pan.ErrRejected, "离线下载", "没有可提交的链接", nil)
	}
	c, err := s.get(ctx, accountID)
	if err != nil {
		return nil, err
	}
	if !c.SupportsOffline() {
		return nil, pan.NewErr(pan.ErrUnsupported, "离线下载", "当前网盘账号不支持离线下载", nil)
	}
	hashes, addErr := c.AddOffline(ctx, uris, dirID)

	// 中途被拒时，前面已经进了队列的那些照样要盯起来——否则它们默默下完了，
	// 而我们还以为整批都没提交成功。
	//
	// 115 返回的 hash 数量也可能与提交数不等（有的链接它直接判重复）。
	// 只对有 hash 的那些建 watch，没 hash 的不必骗自己。
	s.mu.Lock()
	for _, h := range hashes {
		if h = strings.ToLower(strings.TrimSpace(h)); h == "" {
			continue
		}
		s.watches[watchKey(accountID, h)] = &watch{
			accountID: accountID,
			hash:      h,
			dirID:     dirID,
			label:     label,
			status:    pan.OfflineWaiting,
			deadline:  time.Now().Add(watchTimeout),
		}
	}
	s.mu.Unlock()

	return hashes, addErr
}

// Tasks 直接透传一页任务列表（前端「离线任务」列表用）。
func (s *Service) Tasks(ctx context.Context, accountID int64, page int64) ([]pan.OfflineTask, int64, error) {
	c, err := s.get(ctx, accountID)
	if err != nil {
		return nil, 0, err
	}
	return c.ListOfflineTasks(ctx, page)
}

// Watching 返回正在盯着的任务数（界面上显示「N 个任务等待通知」）。
func (s *Service) Watching() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.watches)
}

// Close 停掉轮询器。已经在盯的任务会被放弃（进程都要退了）。
func (s *Service) Close() {
	s.stopOnce.Do(func() { close(s.stop) })
	s.wg.Wait()
}

// ==================== 轮询 ====================

func (s *Service) loop() {
	defer s.wg.Done()
	t := time.NewTicker(s.tick)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
			s.pollOnce()
		}
	}
}

func (s *Service) pollOnce() {
	s.mu.Lock()
	if len(s.watches) == 0 {
		s.mu.Unlock()
		return
	}
	byAcct := make(map[int64]map[string]bool, len(s.watches))
	for _, w := range s.watches {
		if byAcct[w.accountID] == nil {
			byAcct[w.accountID] = map[string]bool{}
		}
		byAcct[w.accountID][w.hash] = true
	}
	s.mu.Unlock()

	for acctID, hashes := range byAcct {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		found := s.scan(ctx, acctID, hashes)
		cancel()
		s.apply(acctID, found)
	}
}

// scan 翻页找目标 hash。找到全部就不再往下翻。
func (s *Service) scan(ctx context.Context, accountID int64, want map[string]bool) map[string]pan.OfflineTask {
	c, err := s.get(ctx, accountID)
	if err != nil {
		s.log.Warn("离线任务轮询失败：取账号客户端出错", "account", accountID, "err", err)
		return nil
	}
	out := make(map[string]pan.OfflineTask, len(want))
	for page := int64(1); page <= 5; page++ {
		tasks, total, err := c.ListOfflineTasks(ctx, page)
		if err != nil {
			s.log.Warn("离线任务轮询失败", "account", accountID, "page", page, "err", err)
			break
		}
		for _, t := range tasks {
			if want[strings.ToLower(t.Hash)] {
				out[strings.ToLower(t.Hash)] = t
			}
		}
		if len(out) >= len(want) || len(tasks) == 0 {
			break
		}
		if total > 0 && page*int64(len(tasks)) >= total {
			break
		}
	}
	return out
}

// apply 把本轮查到的状态写回，终态的抛事件并摘掉。
func (s *Service) apply(accountID int64, found map[string]pan.OfflineTask) {
	var events []Event

	s.mu.Lock()
	for key, w := range s.watches {
		if w.accountID != accountID {
			continue
		}
		t, ok := found[w.hash]
		if !ok {
			// 列表里没有：可能还没出现在第一页，也可能早就被清掉了。
			// 给 12 轮（约 9 分钟）的耐心，之后放手。
			w.misses++
			if w.misses >= 12 || time.Now().After(w.deadline) {
				delete(s.watches, key)
			}
			continue
		}
		w.misses = 0
		w.status = t.Status
		if t.Name != "" {
			w.name = t.Name
		}
		if t.Status != pan.OfflineDone && t.Status != pan.OfflineFailed {
			if time.Now().After(w.deadline) {
				delete(s.watches, key)
			}
			continue
		}
		ev := Event{AccountID: accountID, Task: t, DirID: w.dirID, Label: w.label}
		if ev.Label == "" {
			ev.Label = w.name
		}
		events = append(events, ev)
		delete(s.watches, key)
	}
	cb := s.onDone
	s.mu.Unlock()

	for _, ev := range events {
		if cb != nil {
			cb(ev)
		}
	}
}

func watchKey(accountID int64, hash string) string {
	return strconv.FormatInt(accountID, 10) + "|" + hash
}

func cleanURIs(uris []string) []string {
	seen := make(map[string]bool, len(uris))
	out := make([]string, 0, len(uris))
	for _, u := range uris {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		out = append(out, u)
	}
	return out
}
