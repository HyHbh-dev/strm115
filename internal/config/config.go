// Package config 只管「进程启动就需要知道的东西」。
// 运行期可改的配置（播放域名、Emby 地址等）在数据库 settings 表里，不在这里。
package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

type Config struct {
	Addr      string // 管理后台监听地址
	ProxyAddr string // 播放代理监听地址
	DataDir   string // 数据目录（SQLite 库）
	MediaRoot string // 媒体根目录，所有 .strm 写在这下面
	// MediaHostRoot 是同一份媒体目录在「宿主机」上的绝对路径。
	// 只在 Docker 部署时有意义：容器里看到的是 /media，用户要打开的是自己
	// 机器上的 D:/xxx/media。留空时详情里只显示容器内路径。
	MediaHostRoot string
	// CookieSecure 只在「前面挂了 HTTPS 反代」时打开：会话 cookie 带 Secure，
	// 明文 HTTP 下浏览器不会回传它。会话本身仍是进程内 map，重启必然掉线，
	// 所以这里没有「重启不掉线」的假承诺，也不需要落盘一个没人用的 session.key。
	CookieSecure bool

	// 管理员凭据只作为「首次启动的初始值」。正常用法是在网页里设置，
	// 数据库里已经有账号时这两个字段被忽略（否则改网页密码会被环境变量覆盖）。
	AdminUser string
	AdminPass string
}

func Load() Config {
	c := Config{
		Addr:      getenv("ADDR", ":6060"),
		ProxyAddr: getenv("PROXY_ADDR", ":6086"),
		DataDir:   getenv("DATA_DIR", "./data"),
		MediaRoot: getenv("MEDIA_ROOT", "./media"),
		// docker-compose 把宿主目录原样传进来。相对路径（./media）在容器里
		// 换算不出宿主机绝对路径，后面会按「未配置」处理。
		MediaHostRoot: os.Getenv("MEDIA_HOST_PATH"),
		AdminUser:     getenv("ADMIN_USER", "admin"),
		AdminPass:     os.Getenv("ADMIN_PASSWORD"),
		CookieSecure:  ParseBool(os.Getenv("COOKIE_SECURE"), false),
	}
	// 兼容旧版 .env：老变量名继续认，但提示一次，免得升级后不知道为什么没人读它。
	if c.AdminPass == "" {
		if v := os.Getenv("AUTH_PASSWORD"); v != "" {
			c.AdminPass = v
			slog.Warn("AUTH_PASSWORD 已更名为 ADMIN_PASSWORD，且只作为首次初始化的密码；建议改在网页里设置")
		}
	}
	return c
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ResolveMediaRoot 把媒体根转成绝对路径，避免相对路径随工作目录漂移。
func ResolveMediaRoot(p string) string {
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return abs
}

func ParseBool(s string, def bool) bool {
	b, err := strconv.ParseBool(s)
	if err != nil {
		return def
	}
	return b
}
