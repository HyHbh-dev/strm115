// Package playback 记「此刻有几台设备在看」。
//
// 302 直连之后视频字节不再经过本机，服务端看不到「播放结束」这种事件，
// 所以会话只能按「最后一次起播 + TTL」判定结束。思路与 115-station 的
// proxyAcctLimiter 一致，只是把「连接数」换成「设备数」：直连模式下我们
// 能观察到的是每个客户端的起播请求，不是持续的 TCP 连接。
//
// 默认上限 2 是 115 账号的常见安全水位（同一账号同时开太多路容易被风控）。
// 设 0 表示不限制，但仍会记录设备列表，方便在设置页看到「谁在看」。
package playback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	DefaultLimit = 2
	DefaultTTL   = 45 * time.Minute
	DefaultWait  = 8 * time.Second

	// 名额满了之后等多久再放弃。轮询间隔短一点，别的设备一释放就能立刻补上。
	retryTick = 200 * time.Millisecond
)

var ErrFull = errors.New("同时播放设备已达上限")

// Session 是一次起播请求的身份信息。
type Session struct {
	Key    string // 设备指纹，见 KeyFor
	Label  string // 给人看的设备名
	IP     string
	UA     string
	ItemID string
}

// Device 是设置页里展示的一行。
type Device struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	IP        string `json:"ip"`
	UA        string `json:"ua"`
	ItemID    string `json:"item_id"`
	StartedAt string `json:"started_at"`
	LastSeen  string `json:"last_seen"`
}

type liveDevice struct {
	Device
	start time.Time
	last  time.Time
}

// Registry 是进程内的一份活跃设备表。零值可用。
type Registry struct {
	mu      sync.Mutex
	devices map[string]*liveDevice
	limitFn func() int
	ttl     time.Duration
	wait    time.Duration
	now     func() time.Time
}

func New() *Registry {
	return &Registry{
		devices: map[string]*liveDevice{},
		ttl:     DefaultTTL,
		wait:    DefaultWait,
		now:     time.Now,
	}
}

// Default 是整机共用的一份表。网关与后台 API 都读它，不需要到处传指针。
// 没设置 limitFn 时 Limit() 返回 0（不限制），单测构造的网关因此不受影响。
var Default = New()

// SetLimitFunc 注入「当前允许几台设备」的读取函数，改设置后下一轮起播就生效。
func (r *Registry) SetLimitFunc(fn func() int) {
	r.mu.Lock()
	r.limitFn = fn
	r.mu.Unlock()
}

// Limit 返回上限；0 表示不限制。
func (r *Registry) Limit() int {
	r.mu.Lock()
	fn := r.limitFn
	r.mu.Unlock()
	if fn == nil {
		return 0
	}
	n := fn()
	if n < 0 {
		return 0
	}
	return n
}

// SetWait 调整满额后的等待时间，单测里设 0 可以立刻返回 ErrFull。
func (r *Registry) SetWait(d time.Duration) {
	r.mu.Lock()
	r.wait = d
	r.mu.Unlock()
}

// TTLMinutes 给设置页显示「多久没活动就自动释放」。
func (r *Registry) TTLMinutes() int {
	r.mu.Lock()
	d := r.ttl
	r.mu.Unlock()
	if d <= 0 {
		d = DefaultTTL
	}
	return int(d / time.Minute)
}

// Acquire 占一个播放名额。同一设备重复起播只刷新时间，不重复占坑。
// 满额时最多等 wait，期间有设备过期或被手动释放就补上；超时返回 ErrFull。
func (r *Registry) Acquire(ctx context.Context, s Session) error {
	if s.Key == "" {
		return nil
	}
	r.mu.Lock()
	if r.devices == nil {
		r.devices = map[string]*liveDevice{}
	}
	if r.ttl <= 0 {
		r.ttl = DefaultTTL
	}
	if r.now == nil {
		r.now = time.Now
	}
	wait := r.wait
	fn := r.limitFn
	r.mu.Unlock()

	limit := 0
	if fn != nil {
		limit = fn()
	}
	if limit < 0 {
		limit = 0
	}
	deadline := r.now().Add(wait)

	for {
		r.mu.Lock()
		now := r.now()
		r.prune(now)
		if d, ok := r.devices[s.Key]; ok {
			d.last = now
			d.IP, d.UA, d.ItemID = s.IP, s.UA, s.ItemID
			d.LastSeen = now.Format(time.RFC3339)
			r.mu.Unlock()
			return nil
		}
		if limit == 0 || len(r.devices) < limit {
			stamp := now.Format(time.RFC3339)
			r.devices[s.Key] = &liveDevice{
				Device: Device{
					Key: s.Key, Label: s.Label, IP: s.IP, UA: s.UA, ItemID: s.ItemID,
					StartedAt: stamp, LastSeen: stamp,
				},
				start: now, last: now,
			}
			r.mu.Unlock()
			return nil
		}
		r.mu.Unlock()

		if wait <= 0 || !r.now().Before(deadline) {
			return ErrFull
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retryTick):
		}
	}
}

// Release 手动踢掉一台设备，返回它原来是否存在。
func (r *Registry) Release(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.devices[key]
	delete(r.devices, key)
	return ok
}

// Snapshot 按开始时间排序的当前设备列表（会顺手清掉过期的）。
func (r *Registry) Snapshot() []Device {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.nowOrNow()
	r.prune(now)
	out := make([]Device, 0, len(r.devices))
	for _, d := range r.devices {
		out = append(out, d.Device)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt < out[j].StartedAt })
	return out
}

func (r *Registry) nowOrNow() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// prune 必须在持锁时调用。
func (r *Registry) prune(now time.Time) {
	ttl := r.ttl
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	for k, d := range r.devices {
		if now.Sub(d.last) > ttl {
			delete(r.devices, k)
		}
	}
}

var unsafeKeyChar = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

// KeyFor 生成设备指纹。
//
// 优先用 Emby 客户端都会带的 DeviceId：同一 WiFi 下手机和电视共享出口 IP，
// 只靠 IP 会把一家人算成一台，或者反过来把一台设备的多次 Range 算成多台。
// 拿不到 DeviceId 时用 IP + UA 的短哈希兜底。结果里不能出现斜杠，
// 因为它要直接放进 /api/playback/devices/{key} 的路径段。
func KeyFor(deviceID, ip, ua string) string {
	if d := strings.TrimSpace(deviceID); d != "" {
		return "dev:" + unsafeKeyChar.ReplaceAllString(d, "_")
	}
	sum := sha256.Sum256([]byte(ip + "|" + ua))
	return "ip:" + hex.EncodeToString(sum[:6])
}
