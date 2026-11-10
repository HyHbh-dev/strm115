package httpapi

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"strm115/internal/config"
	"strm115/internal/store"
)

// testNetServer 造一个「一个库 + 一个 .strm」的最小环境。
func testNetServer(t *testing.T, strmURL string) (*Server, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	media := filepath.Join(dir, "media")
	show := filepath.Join(media, "movies", "沙丘 (2021)")
	if err := os.MkdirAll(show, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	p := filepath.Join(show, "Dune.2021.strm")
	if err := os.WriteFile(p, []byte(strmURL+"\n"), 0o644); err != nil {
		t.Fatalf("写 .strm 失败: %v", err)
	}

	ctx := context.Background()
	acct, err := st.CreateAccount(ctx, "115", "测试账号", "{}")
	if err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	if _, err := st.CreateLibrary(ctx, store.Library{
		AccountID:      acct,
		Name:           "电影",
		CollectionType: "movies",
		LocalPath:      "",
	}); err != nil {
		t.Fatalf("建库失败: %v", err)
	}

	return &Server{
		st:  st,
		log: slog.Default(),
		cfg: config.Config{MediaRoot: media, ProxyAddr: ":6086"},
	}, p
}

func TestUsableHost(t *testing.T) {
	cases := map[string]bool{
		"192.168.1.13":  true,
		"10.0.0.5":      true,
		"172.18.0.2":    false, // docker 默认地址池
		"172.31.255.1":  false,
		"172.32.0.1":    true, // 出了 /12，可能是真内网
		"127.0.0.1":     false,
		"169.254.10.20": false,
		"0.0.0.0":       false,
		"play.example":  false, // 域名不参与猜地址
		"":              false,
	}
	for host, want := range cases {
		if got := usableHost(host); got != want {
			t.Errorf("usableHost(%q) = %v，想要 %v", host, got, want)
		}
	}
}

func TestRememberHostFilters(t *testing.T) {
	s := &Server{log: slog.Default()}
	s.rememberHost("192.168.1.13:6060")
	s.rememberHost("172.18.0.2:6060")
	s.rememberHost("127.0.0.1:6060")
	s.rememberHost("play.example.com")

	got := s.seenCandidates()
	if len(got) != 1 || got[0] != "192.168.1.13" {
		t.Fatalf("候选应只有 192.168.1.13，实际 %v", got)
	}
}

// 宿主机换 IP 之后：旧地址探测不通、学到的地址探测通 → 自动改写 .strm 与设置。
func TestAutoFixPlayDomainRewrites(t *testing.T) {
	old := "http://192.168.1.17:6086"
	s, strmPath := testNetServer(t, old+"/d/abc.mkv")
	ctx := context.Background()

	if err := s.st.SetSetting(ctx, "play_domain", old); err != nil {
		t.Fatalf("写设置失败: %v", err)
	}
	s.rememberHost("192.168.1.13:6060")
	// 只把新地址当「通」：真实探测会打 /healthz，这里用假探测代替。
	s.playProbe = func(_ context.Context, d string) bool {
		return strings.Contains(d, "192.168.1.13")
	}

	from, to, files, fixed := s.autoFixPlayDomain(ctx)
	if !fixed {
		t.Fatalf("应该自动修正，from=%q to=%q", from, to)
	}
	if from != old || to != "http://192.168.1.13:6086" {
		t.Fatalf("from/to 不对：%q -> %q", from, to)
	}
	if files != 1 {
		t.Fatalf("应改写 1 个 .strm，实际 %d", files)
	}

	body, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatalf("读 .strm 失败: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != "http://192.168.1.13:6086/d/abc.mkv" {
		t.Fatalf(".strm 内容没改对：%q", got)
	}
	if got := s.setting(ctx, "play_domain"); got != "http://192.168.1.13:6086" {
		t.Fatalf("play_domain 没更新：%q", got)
	}

	// 修好了要留一条通知，用户才知道「刚才为什么播不了」。
	items, err := s.st.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatalf("读通知失败: %v", err)
	}
	if len(items) == 0 || items[0].Event != "play_domain_fixed" {
		t.Fatalf("应有 play_domain_fixed 通知，实际 %+v", items)
	}
}

// 地址是通的就一个字都别动。
func TestAutoFixPlayDomainSkipsHealthy(t *testing.T) {
	cur := "http://192.168.1.13:6086"
	s, strmPath := testNetServer(t, cur+"/d/abc.mkv")
	ctx := context.Background()
	_ = s.st.SetSetting(ctx, "play_domain", cur)
	s.rememberHost("192.168.1.99:6060")
	s.playProbe = func(context.Context, string) bool { return true }

	from, to, files, fixed := s.autoFixPlayDomain(ctx)
	if fixed || files != 0 || from != cur || to != cur {
		t.Fatalf("健康时不该改：from=%q to=%q files=%d fixed=%v", from, to, files, fixed)
	}
	if got := s.setting(ctx, "play_domain"); got != cur {
		t.Fatalf("设置被动了：%q", got)
	}
	if body, _ := os.ReadFile(strmPath); strings.TrimSpace(string(body)) != cur+"/d/abc.mkv" {
		t.Fatal(".strm 被动了")
	}
}

// 没有候选地址时只告警一次，不能每 5 分钟刷一条通知。
func TestAutoFixPlayDomainAlertsOnce(t *testing.T) {
	cur := "http://192.168.1.17:6086"
	s, _ := testNetServer(t, cur+"/d/abc.mkv")
	ctx := context.Background()
	_ = s.st.SetSetting(ctx, "play_domain", cur)
	s.playProbe = func(context.Context, string) bool { return false }

	for i := 0; i < 3; i++ {
		if _, _, _, fixed := s.autoFixPlayDomain(ctx); fixed {
			t.Fatal("没有候选时不该改")
		}
	}
	items, err := s.st.ListNotifications(ctx, 20)
	if err != nil {
		t.Fatalf("读通知失败: %v", err)
	}
	if len(items) != 1 || items[0].Event != "play_domain_broken" {
		t.Fatalf("应只有 1 条告警，实际 %d 条: %+v", len(items), items)
	}
	if got := s.setting(ctx, "play_domain"); got != cur {
		t.Fatalf("地址不该被改：%q", got)
	}
}

// 换路由器会连网段一起换：跨 /24 的候选也必须能自愈。
// 早期版本把「同一 /24」当准入门槛，恰好把最常见的场景排除在外。
func TestAutoFixPlayDomainFollowsRouterChange(t *testing.T) {
	cur := "http://192.168.50.3:6086"
	s, strmPath := testNetServer(t, cur+"/d/abc.mkv")
	ctx := context.Background()
	_ = s.st.SetSetting(ctx, "play_domain", cur)
	s.rememberHost("192.168.1.13:6060")
	s.playProbe = func(_ context.Context, d string) bool {
		return strings.Contains(d, "192.168.1.13")
	}

	from, to, files, fixed := s.autoFixPlayDomain(ctx)
	if !fixed || from != cur || to != "http://192.168.1.13:6086" || files != 1 {
		t.Fatalf("换网段应该自动修正，from=%q to=%q files=%d fixed=%v", from, to, files, fixed)
	}
	body, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatalf("读 .strm 失败: %v", err)
	}
	if got := strings.TrimSpace(string(body)); got != "http://192.168.1.13:6086/d/abc.mkv" {
		t.Fatalf(".strm 没改成新网段地址：%q", got)
	}
	if got := s.setting(ctx, "play_domain"); got != "http://192.168.1.13:6086" {
		t.Fatalf("设置没更新：%q", got)
	}
}

// 两个候选都通时，仍优先同一 /24（那多半只是 DHCP 续租换号）。
// 这里故意让跨网段候选是「更晚看到」的，验证排序压过时间顺序。
func TestAutoFixPlayDomainPrefersSameSubnet(t *testing.T) {
	cur := "http://192.168.1.17:6086"
	s, _ := testNetServer(t, cur+"/d/abc.mkv")
	ctx := context.Background()
	_ = s.st.SetSetting(ctx, "play_domain", cur)
	s.seenMu.Lock()
	s.seenHosts = map[string]time.Time{
		"192.168.1.13": time.Now().Add(-time.Minute), // 同段，较早
		"10.0.0.9":     time.Now(),                    // 跨段，较新
	}
	s.seenMu.Unlock()
	s.playProbe = func(_ context.Context, d string) bool {
		return !strings.Contains(d, "192.168.1.17")
	}

	_, to, _, fixed := s.autoFixPlayDomain(ctx)
	if !fixed || to != "http://192.168.1.13:6086" {
		t.Fatalf("应优先同 /24 候选，to=%q fixed=%v", to, fixed)
	}
}

// 域名型地址不通时不能猜：换成内网 IP 只会更难排查。
func TestAutoFixPlayDomainLeavesDomainAlone(t *testing.T) {
	cur := "https://play.example.com"
	s, _ := testNetServer(t, cur+"/d/abc.mkv")
	ctx := context.Background()
	_ = s.st.SetSetting(ctx, "play_domain", cur)
	s.rememberHost("192.168.1.13:6060")
	s.playProbe = func(_ context.Context, d string) bool {
		return strings.Contains(d, "192.168.1.13")
	}

	if _, _, _, fixed := s.autoFixPlayDomain(ctx); fixed {
		t.Fatal("域名不该被自动换成 IP")
	}
	if got := s.setting(ctx, "play_domain"); got != cur {
		t.Fatalf("域名被改了：%q", got)
	}
}
