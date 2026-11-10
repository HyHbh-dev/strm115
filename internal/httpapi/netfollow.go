package httpapi

// 播放地址自愈。
//
// 背景：play_domain 会被写死进每一个 .strm，而家用宽带/路由器默认开 DHCP，
// 宿主机 IP 换一次（192.168.1.17 → .13），整库就集体「点开没反应」。
// 服务端日志一片正常，因为请求根本没到达它——播放器在连一个已经不存在的地址。
//
// 这个文件解决两件事：
//  1. 发现：直接探测 play_domain 的 /healthz。能连上自己，播放器就大概率连得上，
//     这比枚举网卡可靠——生产跑在容器里，枚举只能看到 172.x 的容器网卡。
//  2. 修复：候选地址来自「用户实际打开后台时用的 Host」（见 rememberHost）。
//     用户能用这个地址打开页面，就说明它对本机可达；再探一次 6086 确认能播，
//     才真正改写，避免把好地址改成坏地址。
//
// 只在「当前地址是 IP 且确定不通」时动手。域名失效通常是 DNS/DDNS 的问题，
// 换成一个内网 IP 只会更糟，所以那种情况只告警不改。

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"strm115/internal/notify"
)

const (
	// 启动后先等一会儿：容器刚起来时网络可能还没就绪，
	// 这时候探测失败并不代表地址真的坏了，误改代价很大。
	playFirstCheck = 45 * time.Second
	playCheckEvery = 5 * time.Minute
	playProbeLimit = 6 * time.Second
	// 只认最近半小时内出现过的访问地址：太久以前的 Host 可能也已经过期了。
	seenHostTTL = 30 * time.Minute
)

// rememberHost 记住「用户实际用什么地址打开了后台」。
// 容器里枚举网卡拿不到宿主机 IP，这是唯一可靠的候选来源。
func (s *Server) rememberHost(raw string) {
	host := hostOnly(raw)
	if !usableHost(host) {
		return
	}
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	if s.seenHosts == nil {
		s.seenHosts = map[string]time.Time{}
	}
	s.seenHosts[host] = time.Now()
	for h, t := range s.seenHosts {
		if time.Since(t) > seenHostTTL {
			delete(s.seenHosts, h)
		}
	}
}

// seenCandidates 返回最近出现过的、适合当播放地址的 IP，最新的在前。
// 只返回 IP：域名型访问可能走反代，拼上 6086 未必通，不该拿来猜。
func (s *Server) seenCandidates() []string {
	s.seenMu.Lock()
	defer s.seenMu.Unlock()
	type item struct {
		host string
		at   time.Time
	}
	var list []item
	for h, t := range s.seenHosts {
		if time.Since(t) > seenHostTTL {
			continue
		}
		if net.ParseIP(h) == nil {
			continue
		}
		list = append(list, item{h, t})
	}
	// 元素很少（一两个），插入排序够用，省得为它引入 sort.Slice 的闭包。
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j].at.After(list[j-1].at); j-- {
			list[j], list[j-1] = list[j-1], list[j]
		}
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		out = append(out, it.host)
	}
	return out
}

// watchPlayDomain 周期检查播放地址，必要时自愈。
func (s *Server) watchPlayDomain(ctx context.Context) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(playFirstCheck):
	}
	for {
		s.checkPlayDomainOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(playCheckEvery):
		}
	}
}

func (s *Server) checkPlayDomainOnce(ctx context.Context) {
	// 单独给这一轮设上限：rewritePlayDomain 要遍历整个库，
	// 库很大时不能让这个 goroutine 无限期挂着。
	c, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()

	from, to, files, fixed := s.autoFixPlayDomain(c)
	switch {
	case fixed:
		s.log.Warn("播放地址已自动修正", "from", from, "to", to, "strm", files)
	case from == to:
		// 正常，什么都不用做（探测成功时刻意不打日志，否则每 5 分钟一条噪音）
	default:
		s.log.Warn("播放地址探测不通，且没有可用候选", "domain", from)
	}
}

// autoFixPlayDomain 检查当前播放地址；不通且有更好的候选时改写。
// 返回值里 from==to 表示「没有问题」或「改不了但也没换」。
func (s *Server) autoFixPlayDomain(ctx context.Context) (from, to string, files int, fixed bool) {
	cur := strings.TrimSpace(s.setting(ctx, "play_domain"))
	if cur == "" {
		return "", "", 0, false
	}

	if s.probePlay(ctx, cur) {
		// 恢复了就把告警状态清掉，下次真坏了还能再提醒一次。
		s.clearPlayAlert()
		return cur, cur, 0, false
	}
	// 单次失败可能只是抖了一下（115 拉流占满带宽时 /healthz 也会迟钝）。
	// 隔一下再探一次，两次都不通才认定真的坏了 —— 误改地址比晚 5 分钟修好更伤。
	time.Sleep(time.Second)
	if s.probePlay(ctx, cur) {
		s.clearPlayAlert()
		return cur, cur, 0, false
	}

	u, err := url.Parse(cur)
	if err != nil || u.Host == "" {
		return cur, cur, 0, false
	}
	// 域名不通多半是 DNS / DDNS / 证书问题，改用内网 IP 只会更难排查。
	if net.ParseIP(u.Hostname()) == nil {
		s.alertPlayBroken(ctx, cur, "当前播放地址不是 IP，可能是域名解析或反代出了问题")
		return cur, cur, 0, false
	}
	oldIP := net.ParseIP(u.Hostname()).To4()

	port := u.Port()
	if port == "" {
		port = proxyPort(s.cfg.ProxyAddr)
	}
	prefix := strings.TrimRight(u.Path, "/")

	for _, host := range s.seenCandidates() {
		if host == u.Hostname() {
			continue
		}
		// 只认同一 /24：DHCP 换 IP 不会跨网段。跨了网段说明用户的网络环境变了
		// （换了路由器 / 接了另一个网口），这时候猜一个地址只会让排查更难。
		if !sameSubnet24(oldIP, host) {
			continue
		}
		cand := u.Scheme + "://" + net.JoinHostPort(host, port) + prefix
		// 候选必须自己先能连通 6086，否则等于把「播不了」换个地址继续播不了。
		if !s.probePlay(ctx, cand) {
			continue
		}
		n, changed, err := s.rewritePlayDomain(ctx, cand)
		if err != nil {
			s.log.Warn("自动改写播放地址失败", "err", err)
			return cur, cur, changed, false
		}
		if err := s.st.SetSetting(ctx, "play_domain", cand); err != nil {
			s.log.Warn("自动保存播放地址失败", "err", err)
			return cur, cur, changed, false
		}
		s.clearPlayAlert()
		s.recordNotification("play_domain_fixed", notify.Message{
			Kind:  notify.KindSystem,
			Title: "播放地址已自动修正",
			Content: fmt.Sprintf(
				"宿主机地址从 %s 变成了 %s（多半是 DHCP 重新分配），已改写 %d 个 .strm（共扫描 %d 个）。\n"+
					"如果经常发生，建议在路由器上给这台机器做 DHCP 保留，或直接用一个固定域名。",
				cur, cand, changed, n),
			Link: "/settings",
		})
		return cur, cand, changed, true
	}

	s.alertPlayBroken(ctx, cur, "也没能找到一个可用的替代地址")
	return cur, cur, 0, false
}

// alertPlayBroken 同一条失效地址只提醒一次，否则每轮检查都会往通知中心塞一条。
func (s *Server) alertPlayBroken(ctx context.Context, domain, why string) {
	s.playFixMu.Lock()
	if s.playAlerted == domain {
		s.playFixMu.Unlock()
		return
	}
	s.playAlerted = domain
	s.playFixMu.Unlock()

	s.recordNotification("play_domain_broken", notify.Message{
		Kind:  notify.KindSystem,
		Title: "播放地址可能已失效",
		Content: fmt.Sprintf(
			"当前播放地址 %s 连不上（%s），Emby 里点播放会没反应。\n"+
				"到「设置 → 播放地址」改成本机现在的局域网 IP，保存时会自动改写已有 .strm。",
			domain, why),
		Link: "/settings",
	})
}

func (s *Server) clearPlayAlert() {
	s.playFixMu.Lock()
	s.playAlerted = ""
	s.playFixMu.Unlock()
}

// probePlay 直接打自己的 /healthz：这是「播放器能不能连上」最直接的替身。
func (s *Server) probePlay(ctx context.Context, domain string) bool {
	if s.playProbe != nil {
		return s.playProbe(ctx, domain)
	}
	return probePlayDomain(ctx, domain)
}

func probePlayDomain(ctx context.Context, domain string) bool {
	c, cancel := context.WithTimeout(ctx, playProbeLimit)
	defer cancel()
	u := strings.TrimRight(domain, "/") + "/healthz"
	req, err := http.NewRequestWithContext(c, http.MethodGet, u, nil)
	if err != nil {
		return false
	}
	// 探测走 localhost/内网，不能被系统代理接管（否则永远探到一个假成功）。
	client := &http.Client{Transport: &http.Transport{Proxy: nil}}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	// 不读完 body 会漏掉连接复用，这个 goroutine 每 5 分钟跑一次，不必在意，
	// 但读掉更干净。
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 512))
	return resp.StatusCode == http.StatusOK
}

// hostOnly 去掉端口和 IPv6 方括号。
func hostOnly(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(raw); err == nil {
		return h
	}
	return strings.Trim(raw, "[]")
}

// usableHost 判断一个地址值不值得当候选。
func usableHost(host string) bool {
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return false
	}
	v4 := ip.To4()
	if v4 == nil {
		return false
	}
	// 172.16.0.0/12 是 docker 默认地址池：容器自己的 eth0 就在这里，
	// 填进 .strm 只有同一 docker 网络里的容器能访问。
	if v4[0] == 172 && v4[1] >= 16 && v4[1] <= 31 {
		return false
	}
	return true
}

// sameSubnet24 判断两个地址是否在同一 /24。
// 家用局域网基本都是 /24，DHCP 续租换 IP 不会换网段；
// 用它当「这多半是同一块网卡换了个号」的判据。
func sameSubnet24(a net.IP, host string) bool {
	b := net.ParseIP(host).To4()
	if a == nil || b == nil {
		return false
	}
	return a[0] == b[0] && a[1] == b[1] && a[2] == b[2]
}

// proxyPort 从监听地址里取播放端口，取不到就用默认值。
func proxyPort(addr string) string {
	if _, p, err := net.SplitHostPort(addr); err == nil && p != "" {
		return p
	}
	return "6086"
}
