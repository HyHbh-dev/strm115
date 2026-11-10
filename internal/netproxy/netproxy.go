// Package netproxy 是进程级的「出站代理开关」。
//
// 为什么要单独一层：设置页的 net_proxy 以前只被少数几个组件读到
// （豆瓣、IMDb、资源搜索、TG Bot 各自用参数传一遍），而电影墙/日历/刮削
// 用的 tmdb 包、TVDB、夸克登录这些客户端是包级单例或启动时就缓存好的，
// 压根不知道设置改过 —— 用户填了代理却「不起作用」，根因就在这里。
//
// 做法是把 http.Transport.Proxy 换成一个函数而不是固定 URL：函数每次请求
// 都读一遍当前值，所以已经构造好的 Client 也会在保存设置的瞬间切过去，
// 不用重启容器、也不用逐个包改构造签名。显式传了 proxy 的组件仍然优先，
// 这里只兜「没传」的那一路，并最后回退到 HTTPS_PROXY 那套环境变量。
package netproxy

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
)

// current 存 string。用 atomic.Value 是因为设置页在 HTTP goroutine 里写，
// 电影墙 / 日历 / 机器人会同时在别的 goroutine 里发请求。
var current atomic.Value

func init() { current.Store("") }

// Set 设置全局出站代理，立即对所有走 URL() 的客户端生效。
// 空串 = 恢复「跟随环境变量 / 直连」。格式坏掉的地址直接忽略，
// 免得一个手误把整站的外网请求全打挂。
func Set(raw string) {
	raw = strings.TrimSpace(raw)
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			return
		}
		switch u.Scheme {
		case "http", "https", "socks5", "socks5h":
		default:
			return
		}
	}
	current.Store(raw)
}

// Get 返回此刻生效的代理地址（不含环境变量兜底）。
func Get() string {
	v, _ := current.Load().(string)
	return v
}

// URL 给 http.Transport.Proxy 用。设置页的值优先，其次环境变量。
func URL(req *http.Request) (*url.URL, error) {
	if p := Get(); p != "" {
		if u, err := url.Parse(p); err == nil && u.Host != "" {
			return u, nil
		}
	}
	return http.ProxyFromEnvironment(req)
}
