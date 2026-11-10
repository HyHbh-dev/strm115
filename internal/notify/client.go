package notify

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errSkip 渠道按类型过滤主动跳过该条消息，不算发送失败。
var errSkip = errors.New("notify: message skipped by channel filter")

// FallbackError 表示消息已经发出去了，但降级了（封面没传上去，退回纯文本）。
//
// 单独一个类型而不是普通 error：这类「半成功」既不该算失败（用户确实收到了
// 消息），也不能悄悄吞掉 —— 以前正是被静默吞掉，才让「入库卡片带上海报」这件事
// 看起来像从来没实现过。
type FallbackError struct{ Reason string }

func (e *FallbackError) Error() string {
	if strings.TrimSpace(e.Reason) == "" {
		return "已退回纯文本"
	}
	return "已退回纯文本：" + e.Reason
}

// Logger 最小日志接口，log.Logger 直接满足。传 nil 可关闭日志。
type Logger interface {
	Printf(format string, args ...any)
}

// Client 通知客户端，可安全并发使用。
type Client struct {
	mu     sync.RWMutex
	cfg    Config
	do     Doer
	logger Logger
}

// New 用默认 http.Client 创建客户端。超时 / 代理从 cfg 读取。
func New(cfg Config) *Client {
	return &Client{cfg: cfg, do: newDoer(cfg), logger: log.Default()}
}

// NewWithDoer 注入自定义 Doer（测试或需要统一治理 HTTP 客户端时用）。
func NewWithDoer(cfg Config, do Doer, logger Logger) *Client {
	if do == nil {
		do = newDoer(cfg)
	}
	return &Client{cfg: cfg, do: do, logger: logger}
}

func newDoer(cfg Config) Doer {
	timeout := 15 * time.Second
	if cfg.HTTPTimeoutSec > 0 {
		timeout = time.Duration(cfg.HTTPTimeoutSec) * time.Second
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if s := strings.TrimSpace(cfg.ProxyURL); s != "" {
		if u, err := url.Parse(s); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Timeout: timeout, Transport: tr}
}

// Reload 热更新配置（同时重建 Doer，使超时 / 代理生效）。
func (c *Client) Reload(cfg Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cfg = cfg
	c.do = newDoer(cfg)
}

// Config 返回当前配置副本。
func (c *Client) Config() Config {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg
}

func (c *Client) snapshot() (Config, Doer) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cfg, c.do
}

func (c *Client) logf(format string, args ...any) {
	if c.logger != nil {
		c.logger.Printf(format, args...)
	}
}

// Channels 返回当前配置下已启用的渠道名。
func (c *Client) Channels() []string {
	cfg := c.Config()
	chs := activeChannels(&cfg)
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.Name())
	}
	return out
}

// Send 同步扇出一条消息到所有启用渠道，逐个返回结果，等待全部完成。
// 单个渠道失败不影响其他渠道。
func (c *Client) Send(ctx context.Context, m Message) []Result {
	return c.send(ctx, m, false)
}

// SendRich 富媒体消息：支持 RichChannel 的渠道发图文 / 图片，
// 其余渠道自动退回纯文本；Image 为空时等价于 Send。
func (c *Client) SendRich(ctx context.Context, m Message) []Result {
	return c.send(ctx, m, true)
}

func (c *Client) send(ctx context.Context, m Message, rich bool) []Result {
	cfg, do := c.snapshot()
	m = m.normalized()
	active := activeChannels(&cfg)
	if len(active) == 0 {
		c.logf("[notify] 没有启用的通知渠道，消息未发送: %s", truncate(firstNonEmpty(m.Title, m.Content), 40))
		return nil
	}
	label := truncate(firstNonEmpty(m.Title, m.Content), 40)
	return fanOut(ctx, active, func(ctx context.Context, ch Channel) error {
		if rich {
			// 图片来源有两种：Image 是公网 URL，ImageFile 是本地文件（TG 上传）。
			// 只判断 Image 会让「本地海报」这条路永远走不到 SendRich ——
			// 入库卡片明明落了 poster.jpg，发出去却只剩文字。
			if rc, ok := ch.(RichChannel); ok && (m.Image != "" || m.ImageFile != "") {
				return rc.SendRich(ctx, &cfg, do, m)
			}
		}
		return ch.Send(ctx, &cfg, do, m)
	}, c.logger, label)
}

// SendNews 发送多篇图文卡片（支持 NewsChannel 的渠道发原生卡片，其余退回文本）。
func (c *Client) SendNews(ctx context.Context, articles []Article) []Result {
	if len(articles) == 0 {
		return nil
	}
	cfg, do := c.snapshot()
	active := activeChannels(&cfg)
	if len(active) == 0 {
		return nil
	}
	lines := make([]string, 0, len(articles))
	for _, a := range articles {
		line := a.Title
		if a.Description != "" {
			line += " · " + a.Description
		}
		lines = append(lines, line)
	}
	text := Message{
		Kind:    KindIngest,
		Title:   fmt.Sprintf("入库通知 · %d 部", len(articles)),
		Content: strings.Join(lines, "\n"),
	}.normalized()
	return fanOut(ctx, active, func(ctx context.Context, ch Channel) error {
		if nc, ok := ch.(NewsChannel); ok {
			return nc.SendNews(ctx, &cfg, do, articles)
		}
		return ch.Send(ctx, &cfg, do, text)
	}, c.logger, truncate(text.Title, 40))
}

// testMessage 是测试消息的固定文案，所有渠道共用，方便用户对照。
func testMessage() Message {
	return Message{
		Kind:    KindSystem,
		Title:   "strm115 消息通知测试",
		Content: "收到这条消息说明该通道配置正确。",
		Force:   true,
	}
}

// Test 向所有启用渠道发一条测试消息，忽略 IYUU / Webhook 的类型勾选。
func (c *Client) Test(ctx context.Context) []Result {
	return c.Send(ctx, testMessage())
}

// TestChannel 只向指定渠道发测试消息，其余启用渠道不动。
//
// 渠道没启用（或名字拼错）时返回空结果——界面据此提示「先勾选启用」，
// 而不是给一个看起来发送成功的假象。
func (c *Client) TestChannel(ctx context.Context, name string) []Result {
	return c.SendTo(ctx, []string{name}, testMessage())
}

// SendTo 只发给 names 里点名的渠道；names 为空等价于 Send。
//
// 存在的意义是「逐个渠道验证」：八个渠道全配好之后，一条测试消息扇出到
// 不相关的群里既扰民又难定位问题。只发指定的那个，失败原因才对得上人。
func (c *Client) SendTo(ctx context.Context, names []string, m Message) []Result {
	if len(names) == 0 {
		return c.Send(ctx, m)
	}
	want := make(map[string]bool, len(names))
	for _, n := range names {
		if s := strings.TrimSpace(n); s != "" {
			want[s] = true
		}
	}
	cfg, do := c.snapshot()
	active := make([]Channel, 0, len(want))
	for _, ch := range activeChannels(&cfg) {
		if want[ch.Name()] {
			active = append(active, ch)
		}
	}
	if len(active) == 0 {
		return nil
	}
	m = m.normalized()
	return fanOut(ctx, active, func(ctx context.Context, ch Channel) error {
		return ch.Send(ctx, &cfg, do, m)
	}, c.logger, truncate(firstNonEmpty(m.Title, m.Content), 40))
}

// fanOut 并发执行并保证结果顺序与 channels 一致。
func fanOut(ctx context.Context, channels []Channel, send func(context.Context, Channel) error, logger Logger, label string) []Result {
	results := make([]Result, len(channels))
	var wg sync.WaitGroup
	for i, ch := range channels {
		wg.Add(1)
		go func(i int, ch Channel) {
			defer wg.Done()
			r := Result{Channel: ch.Name()}
			err := send(ctx, ch)
			switch {
			case err == nil:
				r.OK = true
				if logger != nil {
					logger.Printf("[notify] ✓ %s: %s", r.Channel, label)
				}
			case errors.Is(err, errSkip):
				r.Skipped = true
			default:
				var fb *FallbackError
				switch {
				case errors.As(err, &fb):
					// 消息发出去了，只是没带上图。算成功，但要留一行日志：
					// 否则「卡片没封面」永远查不到原因。
					r.OK = true
					if logger != nil {
						logger.Printf("[notify] ✓ %s: %s（%s）", r.Channel, label, truncate(fb.Error(), 120))
					}
				default:
					r.Error = sanitize(err.Error())
					if logger != nil {
						logger.Printf("[notify] ✗ %s: %s", r.Channel, truncate(r.Error, 160))
					}
				}
			}
			results[i] = r
		}(i, ch)
	}
	wg.Wait()
	return results
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
