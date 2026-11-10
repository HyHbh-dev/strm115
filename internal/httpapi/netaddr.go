package httpapi

// 播放地址（play_domain）是整套系统里最容易配错的一项。
//
// 它会被写进 .strm，而读它的是「播放器」或「另一个容器里的 Emby」，
// 不是本服务。所以它必须是对方能访问到的地址：局域网的机器名、宿主 IP，
// 或者一个域名。写 127.0.0.1 只会指向播放器自己；写容器内网 IP（172.x）
// 出了 docker 网络就不可达——这两种情况的共同表现是「点开没反应」，
// 而且服务端日志干干净净，极难排查。
//
// 这里的三个能力：
//  1. 告诉前端「你当前是用哪个地址打开的」，一键填进去；
//  2. 保存时校验，把明显不可用的地址挡在门外；
//  3. 改了地址之后，把已经写好的 .strm 批量改过来，不用重新同步。

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"strm115/internal/strm"
)

// handleNetAddress 返回「当前访问用的主机名」和本机的候选地址。
//
// 从请求里取 Host 是最可靠的一招：用户能打开这个页面，就说明这个地址
// 对他而言是通的，把它填进 .strm 大概率也对。
func (s *Server) handleNetAddress(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	// localIPv4 枚举的是「本进程所在的网卡」。生产跑在容器里时它只能看到
	// 172.x 的容器网卡（被过滤掉），列表就是空的——用户当时只能盲填一个 IP，
	// 后面 DHCP 一漂就整库播不了。所以把「实际访问用的 Host」也并进来。
	cands := make([]string, 0, 4)
	push := func(h string) {
		if !usableHost(h) {
			return
		}
		for _, c := range cands {
			if c == h {
				return
			}
		}
		cands = append(cands, h)
	}
	// 用户当前用的地址排第一：能打开页面就说明它对他而言是通的。
	push(host)
	for _, h := range localIPv4() {
		push(h)
	}
	for _, h := range s.seenCandidates() {
		push(h)
	}
	out := map[string]any{
		"host":       host,
		"play_port":  6086,
		"current":    s.setting(r.Context(), "play_domain"),
		"candidates": cands,
	}
	writeJSON(w, http.StatusOK, out)
}

// localIPv4 列出本机的局域网地址，给「选择地址」用。
//
// 过滤掉的东西比留下的多：docker0 / br-* / veth* 这些是容器网桥，
// 宿主机能 ping 通、别的设备不行；127.0.0.1 和 169.254.* 同理不可用。
func localIPv4() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		name := strings.ToLower(ifc.Name)
		if strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "br-") ||
			strings.HasPrefix(name, "veth") || strings.HasPrefix(name, "virbr") ||
			strings.HasPrefix(name, "veth") {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			ip := ipNet.IP.To4()
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() {
				continue
			}
			// 172.16.0.0/12 是 docker 默认地址池的重灾区。容器内看到的
			// eth0 就是这个网段，填进 .strm 只有同一网络里的容器能访问。
			// 少数公司内网也用这段，但那种情况下用户本来就得手动填。
			if ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31 {
				continue
			}
			out = append(out, ip.String())
		}
	}
	sort.Strings(out)
	return out
}

// normalizePlayDomain 补 scheme、去尾斜杠，并挡掉回环地址。
func normalizePlayDomain(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("播放地址不能为空")
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", errors.New("播放地址格式不对，示例：http://192.168.1.10:6086")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", errors.New("播放地址只支持 http / https")
	}
	switch strings.ToLower(u.Hostname()) {
	case "localhost", "127.0.0.1", "0.0.0.0", "::1":
		return "", errors.New("不能填本机回环地址：Emby 或播放器在另一个容器/机器里，127.0.0.1 指向的是它自己")
	}
	// 保留路径：反代把服务挂在 /strm 下时（http://host/strm/d/…）也是合法配置。
	return strings.TrimRight(u.Scheme+"://"+u.Host+strings.TrimRight(u.Path, "/"), "/"), nil
}

// domainHint 对「看起来能填、实际很可能不通」的地址给一句提示。
func domainHint(domain string) string {
	u, err := url.Parse(domain)
	if err != nil {
		return ""
	}
	ip := net.ParseIP(u.Hostname()).To4()
	if ip == nil {
		return ""
	}
	// 172.16.0.0/12 是 docker 默认网段的重灾区
	if ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31 {
		return "这个地址在 172.16–31 网段，很可能只是 docker 内部网络，Emby 或手机可能访问不到"
	}
	return ""
}

type rewriteReq struct {
	Domain string `json:"domain"`
}

// handleRewriteDomain 把各库里所有 .strm 的地址改写成新地址。
//
// 存在的意义：用户一开始可能填了容器 IP，发现 Emby 播不了，改成域名之后
// 不想重新同步一遍（大库要几十分钟）。这里只改 URL 的 scheme+host，
// path 和文件名原样保留，播放器那边的进度、Emby 的条目都不会变。
func (s *Server) handleRewriteDomain(w http.ResponseWriter, r *http.Request) {
	var req rewriteReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	domain, err := normalizePlayDomain(req.Domain)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx := r.Context()
	files, changed, err := s.rewritePlayDomain(ctx, domain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetSetting(ctx, "play_domain", domain); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"domain":  domain,
		"files":   files,
		"changed": changed,
		"hint":    domainHint(domain),
	})
}

// rewritePlayDomain 改写已有 .strm 里的地址。
//
// 一般遍历各媒体库根目录。但「接手一台已经有一堆 .strm 的机器」很常见：
// 磁盘上的文件是上一套系统留下的，本项目台账还是空的（libraries 0 行）。
// 这时旧逻辑直接返回 0/0 —— 用户在页面填了播放地址、点了「保存并改写
// 已有 STRM」，界面显示「已改写 0/0 个」，那些文件一个都没动，公网依旧播不了。
// 所以台账为空时兜底扫一遍 MEDIA_ROOT（容器里就是 /media）。
func (s *Server) rewritePlayDomain(ctx context.Context, domain string) (files, changed int, err error) {
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		return 0, 0, err
	}
	roots := make([]string, 0, len(libs)+1)
	for _, lib := range libs {
		roots = append(roots, s.libRoot(lib))
	}
	if len(roots) == 0 {
		if root := strings.TrimSpace(s.cfg.MediaRoot); root != "" {
			roots = append(roots, root)
		}
	}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return files, changed, err
		}
		walkErr := filepath.WalkDir(root, func(p string, d os.DirEntry, werr error) error {
			if werr != nil {
				return nil // 单个目录读不到不该中断整个重写
			}
			if d.IsDir() || !strings.EqualFold(filepath.Ext(p), ".strm") {
				return nil
			}
			files++
			content, rerr := os.ReadFile(p)
			if rerr != nil {
				return nil
			}
			next, ok := replaceDomainOnce(string(content), domain)
			if !ok {
				return nil
			}
			if werr := strm.Write(p, next); werr != nil {
				s.log.Warn("改写 .strm 失败", "path", p, "err", werr)
				return nil
			}
			changed++
			return nil
		})
		if walkErr != nil {
			return files, changed, walkErr
		}
	}
	return files, changed, nil
}

// replaceDomainOnce 只改第一行 URL 的 scheme+host(+可选路径前缀)，其余原样保留。
// 返回 ok=false 表示内容没变，调用方不必写盘。
func replaceDomainOnce(content, domain string) (string, bool) {
	du, err := url.Parse(domain)
	if err != nil || du.Host == "" {
		return content, false
	}
	prefix := strings.TrimRight(du.Path, "/")

	lines := strings.Split(content, "\n")
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		u, err := url.Parse(trimmed)
		if err != nil || u.Host == "" || u.Scheme == "" {
			return content, false
		}
		// 剥掉已经存在的路径前缀再加，避免反复保存变成 /strm/strm/d/…
		rel := u.Path
		if prefix != "" && strings.HasPrefix(rel, prefix+"/") {
			rel = strings.TrimPrefix(rel, prefix)
		}
		next := du.Scheme + "://" + du.Host + prefix + rel
		if u.RawQuery != "" {
			next += "?" + u.RawQuery
		}
		if next == strings.TrimRight(trimmed, "/") {
			return content, false
		}
		lines[i] = next
		return strings.Join(lines, "\n"), true
	}
	return content, false
}
