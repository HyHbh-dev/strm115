package embygw

// 网关不再自己监听端口：它和播放代理（/d/ 跳转）现在共用播放端口，
// 由一个动态 handler 决定「这次请求走 Emby 网关还是走播放代理」。
//
// 为什么合并：两个端口对用户来说只是多一个要记、要映射、要写进播放器的数字，
// 而它们服务的是同一件事 —— 让人在外网看片。合并后只需要记住一个端口：
// 既能填进播放器当 Emby 地址，也是 .strm 里 /d/ 的地址。
//
// 热更新依然保留：设置页改完开关 / UA 例外，换的是 handler 而不是监听。

import (
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
)

// Manager 持有「当前该用哪个 handler」。
//
// 播放端口启动时先把播放代理 Bind 进来（默认行为），配置完整后再 Apply 换成网关；
// Apply 失败（Emby 地址写错等）就退回播放代理，绝不让端口变成不可用。
type Manager struct {
	log *slog.Logger

	mu   sync.Mutex
	play http.Handler
	addr string
	on   atomic.Bool
	cur  atomic.Value // http.Handler

	// lastResolve 记住上一次装进来的直链解析器：设置页只改了开关 / UA 例外时，
	// 调用方传 nil 也能继续用同一份实现（它绑定的是 play.Proxy，不会变）。
	lastResolve Resolver
}

// handlerBox 是 atomic.Value 里的统一载体。
//
// atomic.Value 要求每次 Store 的**具体类型**一致：直接存 http.Handler 时，
// 播放代理是 http.HandlerFunc、网关是 *Gateway，第二次 Store 会 panic
// （inconsistently typed value）。套一层固定结构体就没这个问题。
type handlerBox struct{ h http.Handler }

// NewManager 造一个只跑播放代理的管理器（网关未启用）。
func NewManager(log *slog.Logger) *Manager {
	if log == nil {
		log = slog.Default()
	}
	m := &Manager{log: log, play: http.NotFoundHandler()}
	m.cur.Store(handlerBox{m.play})
	return m
}

// Bind 绑定同端口的播放代理，并记下监听地址（只用于状态展示）。
// main 在起 HTTP 服务之前调一次。
func (m *Manager) Bind(addr string, play http.Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if play != nil {
		m.play = play
	}
	m.addr = addr
	// 网关还没启用时，端口的行为就是纯播放代理。
	if !m.on.Load() {
		m.cur.Store(handlerBox{m.play})
	}
}

// Handler 返回动态分派的 handler，直接交给 http.Server 用。
func (m *Manager) Handler() http.Handler { return m }

func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if box, ok := m.cur.Load().(handlerBox); ok && box.h != nil {
		box.h.ServeHTTP(w, r)
		return
	}
	http.NotFound(w, r)
}

// Addr 返回网关生效时对外宣称的地址（空 = 没启用）。
func (m *Manager) Addr() string {
	if !m.on.Load() {
		return ""
	}
	return m.ListenAddr()
}

// ListenAddr 返回这个端口实际的监听地址，不管网关开没开。
func (m *Manager) ListenAddr() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addr
}

// Apply 打开或关闭网关。
//
// enabled=false 或配置不全时退回纯播放代理；New 失败也退回，
// 因为「网关挂了」最坏应等于「播放回到 Emby 中转」，而不是整端口不通。
func (m *Manager) Apply(enabled bool, opt Options) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !enabled {
		m.cur.Store(handlerBox{m.play})
		if m.on.Swap(false) {
			m.log.Info("Emby 302 网关已关闭，端口只做播放跳转", "addr", m.addr)
		}
		return nil
	}

	if opt.Resolve == nil {
		opt.Resolve = m.lastResolve
	}
	m.lastResolve = opt.Resolve
	opt.Play = m.play

	gw, err := New(opt)
	if err != nil {
		m.cur.Store(handlerBox{m.play})
		m.on.Store(false)
		return err
	}
	m.cur.Store(handlerBox{gw})
	if !m.on.Swap(true) {
		m.log.Info("Emby 302 网关已启用（与播放代理共用端口）",
			"addr", m.addr, "upstream", opt.Upstream)
	}
	return nil
}

// Close 供进程退出时调用。监听从 main 的 http.Server 停，这里没有资源要收。
func (m *Manager) Close() {}
