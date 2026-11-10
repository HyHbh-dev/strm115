package embygw

// 从设置表读一次配置并落到监听上。main 启动时调一次，设置保存后再调一次。

import (
	"context"
	"log/slog"
	"strings"
)

// SettingSource 是「能按 key 读一行设置」的最小接口，store.Store 直接满足。
type SettingSource interface {
	GetSetting(ctx context.Context, key string) (string, bool)
}

// ApplyFromStore 按设置打开 / 关闭网关。返回错误只用于提示，不影响主程序。
//
// 这里不再有「监听端口」这个概念：网关和播放代理共用播放端口。
func ApplyFromStore(ctx context.Context, src SettingSource, mgr *Manager, resolve Resolver, log *slog.Logger) error {
	if log == nil {
		log = slog.Default()
	}
	if !gatewayOn(ctx, src) {
		return mgr.Apply(false, Options{})
	}
	up, _ := src.GetSetting(ctx, "server_url")
	if strings.TrimSpace(up) == "" {
		_ = mgr.Apply(false, Options{})
		return errNoServer
	}
	key, _ := src.GetSetting(ctx, "server_key")
	bypass, _ := src.GetSetting(ctx, "emby_gateway_bypass")
	mode, _ := src.GetSetting(ctx, "emby_gateway_mode")

	from, _ := src.GetSetting(ctx, "server_path_from")
	to, _ := src.GetSetting(ctx, "server_path_to")

	opt := Options{
		Upstream: strings.TrimSpace(up),
		APIKey:   key,
		Bypass:   splitList(bypass),
		Mode:     ParseMode(mode),
		Resolve:  resolve,
		MapLocal: invertedPathMap(from, to),
		Log:      log,
	}
	return mgr.Apply(true, opt)
}

// gatewayOn 判断网关是否该开。
//
// 新配置看 emby_gateway_on（"1"/"0"）。老配置里只有 emby_gateway_port —— 那时候
// 填了端口就等于启用，所以按「非空且不是 0/off」视为开，升级后行为不变。
// 端口号本身已经没用了：合并之后只有播放端口。
func gatewayOn(ctx context.Context, src SettingSource) bool {
	if v, ok := src.GetSetting(ctx, "emby_gateway_on"); ok {
		return truthy(v)
	}
	raw, ok := src.GetSetting(ctx, "emby_gateway_port")
	if !ok {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "0", "off", "false", "no":
		return false
	}
	return true
}

// truthy 认常见的「开」写法，其余都算关。
func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "on", "true", "yes", "enabled":
		return true
	}
	return false
}

type gwError string

func (e gwError) Error() string { return string(e) }

const errNoServer = gwError("还没配置 Emby 地址，302 网关没有启用")

// splitList 把逗号 / 换行 / 分号分隔的一段文本拆成列表。
func splitList(raw string) []string {
	f := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == '\n' || r == '\r'
	})
	out := make([]string, 0, len(f))
	for _, s := range f {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// invertedPathMap 把「本机 → 服务器」的映射翻过来用。
//
// 设置里 server_path_from/to 描述的是「本机路径前缀 → 服务器看到的路径前缀」，
// 而网关手里拿到的是 Emby 的路径，要换回本机路径才能读那个 .strm。
// 两项任缺一个就恒等映射（两边挂同一个卷，绝大多数部署都是这样）。
func invertedPathMap(from, to string) func(string) string {
	f := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(from), "\\", "/"), "/")
	t := strings.TrimRight(strings.ReplaceAll(strings.TrimSpace(to), "\\", "/"), "/")
	if f == "" || t == "" {
		return func(p string) string { return p }
	}
	return func(p string) string {
		q := strings.ReplaceAll(p, "\\", "/")
		if q == t {
			return f
		}
		if strings.HasPrefix(q, t+"/") {
			return f + q[len(t):]
		}
		return p
	}
}
