package notify

import (
	"context"
	"net/http"
	"sync"
)

// Doer 是 net/http.Client 的最小抽象，便于测试时替换成 mock。
type Doer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Channel 一个通知渠道。实现后调用 Register 即可接入 Send 的扇出。
type Channel interface {
	// Name 渠道标识，出现在 Result.Channel 与 /notify/channels 响应里。
	Name() string
	// Enabled 返回当前配置下是否启用；Send 只调用已启用的渠道。
	Enabled(cfg *Config) bool
	// Send 发送一条消息。实现方应尊重 ctx 超时。
	Send(ctx context.Context, cfg *Config, do Doer, m Message) error
}

// RichChannel 可选接口：带海报 / 链接的富媒体消息。
// 未实现时 Client.SendRich 自动回退到 Send 的纯文本。
type RichChannel interface {
	SendRich(ctx context.Context, cfg *Config, do Doer, m Message) error
}

// NewsChannel 可选接口：多篇图文卡片。未实现时 SendNews 回退到逐条文本。
type NewsChannel interface {
	SendNews(ctx context.Context, cfg *Config, do Doer, articles []Article) error
}

// 内置渠道。保持切片不可变，调用方通过 Register 追加自定义渠道。
var builtinChannels = []Channel{
	wecomChannel{}, dingTalkChannel{}, telegramChannel{}, feishuChannel{},
	oneBotChannel{}, qqOfficialChannel{}, iyuuChannel{}, webhookChannel{},
}

var (
	registryMu     sync.RWMutex
	customChannels []Channel
)

// Register 追加一个自定义渠道。同名渠道会覆盖内置或先前注册的实现，
// 因此也可用来替换某个内置渠道的行为。应在首次 Send 前调用。
func Register(ch Channel) {
	if ch == nil {
		return
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	for i, old := range customChannels {
		if old.Name() == ch.Name() {
			customChannels[i] = ch
			return
		}
	}
	customChannels = append(customChannels, ch)
}

// RegisteredChannels 返回当前生效的渠道名（内置 + 自定义，同名已去重覆盖）。
func RegisteredChannels() []string {
	chs := allChannels()
	out := make([]string, 0, len(chs))
	seen := make(map[string]bool, len(chs))
	for _, ch := range chs {
		if !seen[ch.Name()] {
			seen[ch.Name()] = true
			out = append(out, ch.Name())
		}
	}
	return out
}

func allChannels() []Channel {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Channel, 0, len(builtinChannels)+len(customChannels))
	out = append(out, builtinChannels...)
	out = append(out, customChannels...)
	return out
}

// activeChannels 返回已启用且去重后的渠道（后注册的同名渠道优先）。
func activeChannels(cfg *Config) []Channel {
	all := allChannels()
	byName := make(map[string]Channel, len(all))
	var order []string
	for _, ch := range all {
		if _, exists := byName[ch.Name()]; !exists {
			order = append(order, ch.Name())
		}
		byName[ch.Name()] = ch
	}
	out := make([]Channel, 0, len(order))
	for _, name := range order {
		ch := byName[name]
		if ch.Enabled(cfg) {
			out = append(out, ch)
		}
	}
	return out
}
