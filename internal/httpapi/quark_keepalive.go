package httpapi

// 夸克网页 Cookie 的自动续期。
//
// __puus 大约 3 小时就会轮换，但夸克的网页接口在「请求里没带 __puus」时
// 会重新下发一个新的（AlistGo/alist#830 里也是这么做的）。所以只要进程
// 不长期停机，每 ~100 分钟主动打一次只读的 /config，Cookie 就能一直续下去，
// 用户只需要第一次贴一次 Cookie，不用每隔几小时回来重贴。
//
// 另外每次普通请求（Ping / 转存）的 Set-Cookie 也会被 quark.Client 合并；
// 保活循环负责把合并后的结果写回 accounts.credential 持久化。

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"

	"strm115/internal/quark"
)

const (
	// 比 __puus 的 3 小时短不少，留足重试窗口；±5 分钟抖动避免多实例同时打。
	quarkKeepInterval = 100 * time.Minute
	quarkKeepJitter   = 5 * time.Minute
	// 启动后先等一会儿再跑：New() 里数据库刚打开，别跟启动流程抢。
	quarkKeepWarmup = 30 * time.Second
)

type quarkKeepState struct {
	AccountID int64      `json:"account_id"`
	CookieSet bool       `json:"cookie_set"`
	OK        bool       `json:"ok"`
	LastAt    *time.Time `json:"last_at,omitempty"`
	LastError string     `json:"last_error,omitempty"`
	NextAt    time.Time  `json:"next_at"`
	Refreshes int        `json:"refreshes"`
}

type quarkKeeper struct {
	s     *Server
	mu    sync.Mutex
	state map[int64]*quarkKeepState
	wake  chan struct{}
}

func newQuarkKeeper(s *Server) *quarkKeeper {
	return &quarkKeeper{s: s, state: map[int64]*quarkKeepState{}, wake: make(chan struct{}, 1)}
}

func (k *quarkKeeper) start() {
	go func() {
		timer := time.NewTimer(quarkKeepWarmup)
		defer timer.Stop()
		for {
			select {
			case <-timer.C:
			case <-k.wake:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
			k.runOnce(ctx)
			cancel()
			d := quarkKeepInterval + time.Duration(rand.Int63n(int64(2*quarkKeepJitter))) - quarkKeepJitter
			k.setNext(time.Now().Add(d))
			timer.Reset(d)
		}
	}()
}

// poke 让循环立刻跑一轮（刚贴完 Cookie 时调用，用户不用等 100 分钟）。
func (k *quarkKeeper) poke() {
	select {
	case k.wake <- struct{}{}:
	default:
	}
}

func (k *quarkKeeper) setNext(t time.Time) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, v := range k.state {
		v.NextAt = t
	}
}

func (k *quarkKeeper) put(id int64, fn func(*quarkKeepState)) {
	k.mu.Lock()
	defer k.mu.Unlock()
	st := k.state[id]
	if st == nil {
		st = &quarkKeepState{AccountID: id}
		k.state[id] = st
	}
	fn(st)
}

func (k *quarkKeeper) snapshot() []quarkKeepState {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := make([]quarkKeepState, 0, len(k.state))
	for _, v := range k.state {
		out = append(out, *v)
	}
	return out
}

// runOnce 给每个「有网页 Cookie 的夸克账号」续一次期。
// 单个账号失败不影响其它账号，也不会让整个循环退出。
func (k *quarkKeeper) runOnce(ctx context.Context) {
	accts, err := k.s.st.ListAccounts(ctx)
	if err != nil {
		return
	}
	base := strings.TrimSpace(k.s.setting(ctx, "quark_web_base"))
	for _, a := range accts {
		if !strings.EqualFold(a.Kind, quarkKind) {
			continue
		}
		cr, err := k.s.quarkCredential(ctx, a.ID)
		if err != nil {
			k.put(a.ID, func(st *quarkKeepState) { st.LastError = err.Error() })
			continue
		}
		if strings.TrimSpace(cr.WebCookie) == "" {
			k.put(a.ID, func(st *quarkKeepState) {
				st.CookieSet, st.OK, st.LastError = false, false, ""
			})
			continue
		}

		c := quark.NewWithBase(cr.WebCookie, base)
		fresh, err := c.RefreshPuus(ctx)
		now := time.Now()
		if errors.Is(err, quark.ErrNoRefresh) {
			// 服务端这次没轮换，但普通响应里可能已经并进了新值，照常收下。
			fresh, err = c.Cookie(), nil
		}
		if err != nil {
			k.put(a.ID, func(st *quarkKeepState) {
				st.CookieSet, st.OK, st.LastAt, st.LastError = true, false, &now, err.Error()
			})
			continue
		}
		if fresh != "" && fresh != cr.WebCookie {
			cr.WebCookie = fresh
			raw, _ := json.Marshal(cr)
			if err := k.s.st.UpdateAccountCredential(ctx, a.ID, string(raw)); err != nil {
				k.put(a.ID, func(st *quarkKeepState) {
					st.CookieSet, st.OK, st.LastAt, st.LastError = true, false, &now, "写回 Cookie 失败: "+err.Error()
				})
				continue
			}
		}
		k.put(a.ID, func(st *quarkKeepState) {
			st.CookieSet, st.OK, st.LastAt, st.LastError = true, true, &now, ""
			st.Refreshes++
		})
	}
}

// handleQuarkKeepAlive GET 查状态 / POST 立刻续一次（"现在就保活"按钮）。
func (s *Server) handleQuarkKeepAlive(w http.ResponseWriter, r *http.Request) {
	if s.quarkKeep == nil {
		writeJSON(w, http.StatusOK, []quarkKeepState{})
		return
	}
	if r.Method == http.MethodPost {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		s.quarkKeep.runOnce(ctx)
	}
	writeJSON(w, http.StatusOK, s.quarkKeep.snapshot())
}
