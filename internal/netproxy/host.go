package netproxy

// 容器内「127.0.0.1 不是宿主机」这一坑。
//
// 用户在本机浏览器里填 http://127.0.0.1:7890（Clash / mihomo 的默认端口）
// 完全正确；但项目跑在 Docker 里，这个地址指向容器自身，连过去必定是
// connection refused。表现就是「代理明明配了却一点用没有」，而且 Telegram
// 长轮询会一直刷 proxyconnect tcp: dial tcp 127.0.0.1:7890 的错误。
//
// 这里在读取设置时把 loopback 自动改写成宿主机在容器网络里的地址：
//   1. host.docker.internal（compose 里配了 host-gateway）
//   2. 否则读 /proc/net/route 的默认网关（Linux 容器通用）
// 库里仍保存用户填的原值 —— 同一份配置在宿主机直跑时不需要改写。

import (
	"encoding/binary"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
)

// 这两个做成变量，单测不用真的跑在容器里也能验改写逻辑。
var (
	containerCheck = inContainer
	hostLookup     = dockerHost
)

// Normalize 把宿主机 loopback 代理改写成容器内可达的地址。
// 非容器环境、非 loopback、拿不到网关时都原样返回。
func Normalize(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !containerCheck() {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	switch strings.ToLower(u.Hostname()) {
	case "127.0.0.1", "localhost", "::1", "0.0.0.0":
	default:
		return raw // 已经是 LAN IP / 域名，用户自己填对了
	}
	host := hostLookup()
	if host == "" {
		return raw
	}
	port := u.Port()
	if port == "" {
		if strings.EqualFold(u.Scheme, "https") {
			port = "443"
		} else {
			port = "80"
		}
	}
	u.Host = net.JoinHostPort(host, port) // u.User 会由 u.String() 自动带上
	return u.String()
}

func inContainer() bool {
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

func dockerHost() string {
	if ips, err := net.LookupHost("host.docker.internal"); err == nil && len(ips) > 0 {
		return ips[0]
	}
	return defaultGateway()
}

// defaultGateway 从 /proc/net/route 里取默认网关。
// Gateway 列是主机字节序的十六进制（小端机器上 010015AC = 172.21.0.1）。
func defaultGateway() string {
	b, err := os.ReadFile("/proc/net/route")
	if err != nil {
		return ""
	}
	lines := strings.Split(string(b), "\n")
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) < 3 || f[1] != "00000000" {
			continue
		}
		v, err := strconv.ParseUint(f[2], 16, 32)
		if err != nil || v == 0 {
			continue
		}
		var buf [4]byte
		binary.LittleEndian.PutUint32(buf[:], uint32(v))
		return net.IP(buf[:]).String()
	}
	return ""
}
