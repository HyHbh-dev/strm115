package embygw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"strm115/internal/playback"
)

// strmFile 造一个真实的 .strm 文件，返回它的路径。
// 网关读的是文件内容，而不是 Emby 返回的 URL —— Emby 的 Path 指向文件本身。
func strmFile(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "F1.mkv.strm")
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeEmby 只实现网关用到的两个接口：条目查询 + 兜底代理的落点。
func fakeEmby(t *testing.T, itemPath string) (*httptest.Server, *int) {
	t.Helper()
	proxied := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Items" || r.URL.Path == "/emby/Items" {
			if r.Header.Get("X-Emby-Token") == "" && r.URL.Query().Get("api_key") == "" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			// 用 %q 转义：Windows 下临时路径带反斜杠，手拼 JSON 会变成非法转义。
			body, _ := json.Marshal(map[string]any{
				"Items": []map[string]string{{"Id": "437", "Path": itemPath}},
			})
			_, _ = w.Write(body)
			return
		}
		proxied++
		_, _ = io.WriteString(w, "proxied:"+r.URL.Path)
	}))
	t.Cleanup(srv.Close)
	return srv, &proxied
}

func newGateway(t *testing.T, upstream string, resolve Resolver, bypass ...string) *Gateway {
	t.Helper()
	g, err := New(Options{Upstream: upstream, Bypass: bypass, Resolve: resolve})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
	return g
}

// 静态直出请求要 302 到网盘直链，视频字节不再经过 Emby。
func TestStaticStreamRedirectsToCDN(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	var gotID, gotUA string
	g := newGateway(t, up.URL, func(_ context.Context, id, ua string) (string, error) {
		gotID, gotUA = id, ua
		return "https://dl-c.drive.quark.cn/x", nil
	})

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?static=true", nil)
	req.Header.Set("X-Emby-Token", "tok")
	req.Header.Set("User-Agent", "Hills Windows/1.6.0")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("应该 302，实际 %d（body=%q）", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "https://dl-c.drive.quark.cn/x" {
		t.Fatalf("Location = %q", loc)
	}
	if gotID != "q/04ac73.mkv" {
		t.Fatalf("传给解析器的 id = %q，应该保留 q/ 前缀与扩展名", gotID)
	}
	if gotUA != "Hills Windows/1.6.0" {
		t.Fatalf("UA 必须原样透传（115 按 UA 签名），实际 %q", gotUA)
	}
	if *proxied != 0 {
		t.Fatalf("不该再走 Emby 代理，实际代理了 %d 次", *proxied)
	}
}

// 转码请求必须留在 Emby：把 m3u8 换成 mkv 直链客户端会播不了。
func TestTranscodeStaysOnEmby(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		t.Fatal("转码请求不该被接管")
		return "", nil
	})

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?VideoCodec=h264&AudioCodec=aac", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || *proxied != 1 {
		t.Fatalf("应该原样代理，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
}

// UA 例外名单里的客户端不拦（个别播放器不跟 302）。
func TestBypassUA(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		t.Fatal("例外 UA 不该被接管")
		return "", nil
	}, "TV Box")

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?static=true", nil)
	req.Header.Set("X-Emby-Token", "tok")
	req.Header.Set("User-Agent", "okhttp/4.9 (TV Box)")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if *proxied != 1 {
		t.Fatalf("应该代理，实际 proxied=%d", *proxied)
	}
}

// 条目不是 .strm（本地真实文件）就别接管，老老实实代理。
func TestLocalFileNotRedirected(t *testing.T) {
	up, proxied := fakeEmby(t, "/media/电影/本地片.mkv")
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		t.Fatal("本地文件不该走直链解析")
		return "", nil
	})

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/9/stream?static=true", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if *proxied != 1 {
		t.Fatalf("应该代理，实际 proxied=%d", *proxied)
	}
}

// 解析失败（网盘掉线 / 账号失效）时必须退化成代理，不能让播放直接失败。
func TestResolveFailureFallsBackToProxy(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		return "", context.DeadlineExceeded
	})

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?static=true", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if *proxied != 1 || rec.Code == http.StatusFound {
		t.Fatalf("解析失败应回退代理，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
}

// settingSource 是 gatewayOn 的最小实现，测试里当假设置表用。
type settingSource map[string]string

func (m settingSource) GetSetting(_ context.Context, key string) (string, bool) {
	v, ok := m[key]
	return v, ok
}

// 老配置里「填了端口」就等于启用；新配置只看开关，且开关优先。
func TestGatewayOn(t *testing.T) {
	cases := []struct {
		name string
		kv   settingSource
		want bool
	}{
		{"什么都没有", settingSource{}, false},
		{"新开关打开", settingSource{"emby_gateway_on": "1"}, true},
		{"新开关关闭", settingSource{"emby_gateway_on": "0"}, false},
		{"新开关优先于老端口", settingSource{"emby_gateway_on": "0", "emby_gateway_port": "18096"}, false},
		{"老配置填了端口", settingSource{"emby_gateway_port": "18096"}, true},
		{"老配置填 0", settingSource{"emby_gateway_port": "0"}, false},
		{"老配置留空", settingSource{"emby_gateway_port": ""}, false},
	}
	for _, c := range cases {
		if got := gatewayOn(context.Background(), c.kv); got != c.want {
			t.Errorf("%s：gatewayOn = %v，期望 %v", c.name, got, c.want)
		}
	}
}

func TestStrmTarget(t *testing.T) {
	cases := []struct {
		raw  string
		want string
		ok   bool
	}{
		{"http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv", "q/04ac73.mkv", true},
		{"http://192.168.1.10:6086/d/ctdvbd6p1km4udxn4.mkv", "ctdvbd6p1km4udxn4.mkv", true},
		{"https://emby.example.com/d/q/abc", "q/abc", true},
		{"/media/电影/本地片.mkv", "", false},
		{"https://cdn.example.com/x.mkv", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := strmTarget(c.raw)
		if got != c.want || ok != c.ok {
			t.Errorf("strmTarget(%q) = (%q,%v)，期望 (%q,%v)", c.raw, got, ok, c.want, c.ok)
		}
	}
}

// 目录页 / PlaybackInfo 这类请求一个字都不能改，必须原样转发。
func TestNonStreamRequestsAreProxied(t *testing.T) {
	up, proxied := fakeEmby(t, "")
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		t.Fatal("非播放请求不该走直链解析")
		return "", nil
	})
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", strings.NewReader("{}"))
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if *proxied != 1 {
		t.Fatalf("应该代理，实际 proxied=%d", *proxied)
	}
	if !strings.HasPrefix(rec.Body.String(), "proxied:") {
		t.Fatalf("代理响应异常: %q", rec.Body.String())
	}
}

// 合并端口之后，/d/ 和 /healthz 必须还给播放代理。
// 少了这一步，播放器请求 .strm 里的地址会变成「转给 8096 再 404」。
func TestPlayPathsBypassEmbyProxy(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://127.0.0.1:6086/d/q/x.mkv"))
	var hit string
	play := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = r.URL.Path
		_, _ = io.WriteString(w, "play:"+r.URL.Path)
	})
	g, err := New(Options{
		Upstream: up.URL,
		Play:     play,
		Resolve:  func(context.Context, string, string) (string, error) { return "", nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, p := range []string{"/d/q/04ac73.mkv", "/healthz"} {
		hit = ""
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if hit != p {
			t.Fatalf("%s 应该交给播放代理，实际落到 %q", p, hit)
		}
	}
	if *proxied != 0 {
		t.Fatalf("播放路径不该转给 Emby，实际转发 %d 次", *proxied)
	}
}

// 网关关掉之后，同一个端口回到纯播放代理（不是整个端口不可用）。
func TestManagerFallsBackToPlayWhenDisabled(t *testing.T) {
	m := NewManager(nil)
	var got string
	play := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	m.Bind(":6086", play)
	if err := m.Apply(false, Options{}); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/emby/System/Info", nil))
	if got != "/emby/System/Info" {
		t.Fatalf("关闭网关后请求应落到播放代理，实际 %q", got)
	}
	if m.Addr() != "" {
		t.Fatalf("关闭后 Addr() 应为空，实际 %q", m.Addr())
	}
	if m.ListenAddr() != ":6086" {
		t.Fatalf("端口的监听地址不该随网关开关变化，实际 %q", m.ListenAddr())
	}
}

// 打开网关后，同一端口由网关接管，Addr() 报出实际监听地址。
func TestManagerEnablesGatewayOnSamePort(t *testing.T) {
	m := NewManager(nil)
	m.Bind(":6086", http.NotFoundHandler())

	up, _ := fakeEmby(t, strmFile(t, "http://127.0.0.1:6086/d/q/x.mkv"))
	err := m.Apply(true, Options{
		Upstream: up.URL,
		Resolve:  func(context.Context, string, string) (string, error) { return "https://cdn/x", nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Addr() != ":6086" {
		t.Fatalf("Addr() 应为 :6086，实际 %q", m.Addr())
	}

	req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?static=true", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	m.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("网关应接管播放请求并 302，实际 %d", rec.Code)
	}
}

// PlaybackInfo 到达时就该把直链换好：真正的取流请求随后就到，那时必须命中缓存。
func TestPlaybackInfoPrewarmsLink(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	calls := make(chan string, 4)
	g, err := New(Options{
		Upstream: up.URL,
		Play:     http.NotFoundHandler(), // 预热要求 Play 非空（同端口部署才需要）
		Resolve: func(_ context.Context, id, _ string) (string, error) {
			calls <- id
			return "https://cdn/x", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo?UserId=u1", nil)
	req.Header.Set("X-Emby-Token", "tok")
	req.Header.Set("User-Agent", "Hills Windows/1.6.0")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || *proxied != 1 {
		t.Fatalf("PlaybackInfo 必须原样代理，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
	select {
	case id := <-calls:
		if id != "q/04ac73.mkv" {
			t.Fatalf("预热的 id = %q", id)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PlaybackInfo 之后没有预热直链")
	}

	// 预热完成后再来取流请求，解析器不应该被再叫一次（命中缓存由 Proxy 负责，
	// 这里只验证网关自己不重复解析）。
	select {
	case extra := <-calls:
		t.Fatalf("不该重复解析，却收到 %q", extra)
	case <-time.After(150 * time.Millisecond):
	}
}

// 没有 Play / Resolve 时（比如只做反代）预热要安静跳过，不能起 goroutine 乱撞。
func TestPlaybackInfoWithoutResolverIsIgnored(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	g := newGateway(t, up.URL, nil)

	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || *proxied != 1 {
		t.Fatalf("应该原样代理，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
}

// 第三方客户端（SenPlayer / Infuse / Fileball …）常把路径写成 `/emby/Videos/...`
// 或干脆省掉 /emby 前缀。Emby 自己的路由不区分大小写，网关以前只认小写，
// 结果这些播放器静默退回 Emby 中转 —— 公网下又慢又容易断。
func TestThirdPartyClientPathsRedirect(t *testing.T) {
	const cdn = "https://cdn.example.com/movie.mkv"
	cases := []struct {
		name, path, authHeader, authValue string
	}{
		{"大写 Videos", "/emby/Videos/437/stream?static=true", "X-Emby-Token", "tok"},
		{"无前缀大写", "/Videos/437/stream.mkv?static=true", "X-Emby-Token", "tok"},
		{"全大写", "/EMBY/VIDEOS/437/STREAM?static=true", "X-Emby-Token", "tok"},
		{"大写 original", "/emby/Videos/437/original.mkv", "X-Emby-Token", "tok"},
		{"大写 Download", "/emby/Items/437/Download", "X-Emby-Token", "tok"},
		{"static 大写值", "/emby/Videos/437/stream?static=TRUE", "X-Emby-Token", "tok"},
		{
			"X-Emby-Authorization 抽 token", "/emby/Videos/437/stream?static=true",
			"X-Emby-Authorization",
			`MediaBrowser Client="SenPlayer", Device="iPhone", DeviceId="ios1", Version="3.2", Token="tok-from-auth"`,
		},
		{
			"Authorization 抽 token", "/emby/Videos/437/stream?static=true",
			"Authorization", `MediaBrowser Token="tok-from-auth2"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
			g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
				return cdn, nil
			})
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set(tc.authHeader, tc.authValue)
			req.Header.Set("User-Agent", "SenPlayer/3.2 CFNetwork/1496 Darwin/23.5")
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("应该 302，实际 %d（body=%q）", rec.Code, rec.Body.String())
			}
			if loc := rec.Header().Get("Location"); loc != cdn {
				t.Fatalf("Location = %q", loc)
			}
			if *proxied != 0 {
				t.Fatalf("不该走 Emby 代理，实际 %d 次", *proxied)
			}
		})
	}
}

// 大写路径的转码请求仍然必须留在 Emby，别因为放宽了大小写就把 m3u8 换成 mkv。
func TestUppercaseTranscodeStaysOnEmby(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv"))
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		t.Fatal("转码请求不该被接管")
		return "", nil
	})
	req := httptest.NewRequest(http.MethodGet, "/emby/Videos/437/stream?VideoCodec=h264", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || *proxied != 1 {
		t.Fatalf("应该原样代理，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
}

func TestEmbyTokenFallbackOrder(t *testing.T) {
	g := &Gateway{opt: Options{APIKey: "server-key"}}
	req := httptest.NewRequest(http.MethodGet, "/emby/Videos/1/stream", nil)
	if got := g.embyToken(req); got != "server-key" {
		t.Fatalf("没带任何 token 时该用兜底 Key，实际 %q", got)
	}
	req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="SenPlayer", Token="abc123"`)
	if got := g.embyToken(req); got != "abc123" {
		t.Fatalf("应从 X-Emby-Authorization 抽出 token，实际 %q", got)
	}
	req.Header.Set("X-Emby-Token", "explicit")
	if got := g.embyToken(req); got != "explicit" {
		t.Fatalf("显式 X-Emby-Token 优先，实际 %q", got)
	}
}

// 同时播放设备上限：默认 2 台，第 3 台起播要被明确拦下（429），
// 释放一台后立刻能补上。HEAD 探测和 PlaybackInfo 都不该占名额。
func TestPlaybackDeviceLimit(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	reg := playback.New()
	reg.SetWait(0)
	reg.SetLimitFunc(func() int { return 2 })
	g, err := New(Options{
		Upstream: up.URL,
		Playback: reg,
		Resolve: func(context.Context, string, string) (string, error) {
			return "https://cdn.example.com/movie.mkv", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	request := func(device string, method string, path string) int {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		if device != "" {
			req.Header.Set("X-Emby-Authorization",
				`MediaBrowser Client="SenPlayer", Device="iPhone", DeviceId="`+device+`", Token="tok"`)
		} else {
			req.Header.Set("X-Emby-Token", "tok")
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		return rec.Code
	}
	stream := "/emby/videos/437/stream?static=true"
	if got := request("phone", http.MethodGet, stream); got != http.StatusFound {
		t.Fatalf("第 1 台应该 302，实际 %d", got)
	}
	if got := request("tv", http.MethodGet, stream); got != http.StatusFound {
		t.Fatalf("第 2 台应该 302，实际 %d", got)
	}
	if got := request("pad", http.MethodGet, stream); got != http.StatusTooManyRequests {
		t.Fatalf("第 3 台应该 429，实际 %d", got)
	}
	// 探测与「问怎么播」都不算设备，不能把名额吃掉。
	request("", http.MethodHead, stream)
	request("", http.MethodPost, "/emby/Items/437/PlaybackInfo")
	if got := len(reg.Snapshot()); got != 2 {
		t.Fatalf("HEAD / PlaybackInfo 不该占名额，实际 %d 台", got)
	}
	if !reg.Release("dev:tv") {
		t.Fatal("释放失败")
	}
	if got := request("pad", http.MethodGet, stream); got != http.StatusFound {
		t.Fatalf("释放后应该能补上，实际 %d", got)
	}
}

// 没配上限（单测构造的默认网关）时一台都不拦，老行为零变化。
func TestPlaybackUnlimitedByDefault(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	g := newGateway(t, up.URL, func(context.Context, string, string) (string, error) {
		return "https://cdn.example.com/movie.mkv", nil
	})
	for i := 0; i < 5; i++ {
		req := httptest.NewRequest(http.MethodGet,
			"/emby/videos/437/stream?static=true&DeviceId=dev-"+strconv.Itoa(i), nil)
		req.Header.Set("X-Emby-Token", "tok")
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("第 %d 台不该被拦，实际 %d", i+1, rec.Code)
		}
	}
}

// Emby 自己的 ffprobe 也会来拉静态流，绝不能把它算成「一台设备在看」，
// 否则默认 2 台的上限会被探测占满，真正要播的人反而收到 429。
func TestPlaybackIgnoresServerProbes(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	reg := playback.New()
	reg.SetWait(0)
	reg.SetLimitFunc(func() int { return 2 })
	g, err := New(Options{
		Upstream: up.URL, Playback: reg,
		Resolve: func(context.Context, string, string) (string, error) {
			return "https://cdn.example.com/movie.mkv", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	probes := []string{
		"Lavf/59.16.100",           // ffprobe
		"Emby Server/4.8",          // Emby 自己
		"",                         // 无标识的脚本探测
		"Mozilla/5.0 (compatible)", // 有 UA 但没 DeviceId
	}
	for _, ua := range probes {
		req := httptest.NewRequest(http.MethodGet, "/emby/videos/437/stream?static=true", nil)
		req.Header.Set("X-Emby-Token", "tok")
		if ua != "" {
			req.Header.Set("User-Agent", ua)
		}
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Fatalf("探测请求不该被拦（ua=%q）：%d", ua, rec.Code)
		}
	}
	if got := len(reg.Snapshot()); got != 0 {
		t.Fatalf("服务端探测不该占名额，实际 %d 台", got)
	}
	// 真正的客户端带 DeviceId，仍然正常计入。
	req := httptest.NewRequest(http.MethodGet,
		"/emby/videos/437/stream?static=true&DeviceId=ios-1", nil)
	req.Header.Set("X-Emby-Token", "tok")
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound || len(reg.Snapshot()) != 1 {
		t.Fatalf("真实客户端应正常计入，code=%d devices=%d", rec.Code, len(reg.Snapshot()))
	}
}
