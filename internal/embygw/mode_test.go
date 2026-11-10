package embygw

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// 三种播放方式的共同测试脚手架。Play 是同端口的播放代理（/d/ 端点），
// 这里用假的 302 handler 代替，好验证「直连 /d/」那条路确实闭合。
func modeGateway(t *testing.T, upstream string, mode PlayMode, resolve Resolver) *Gateway {
	t.Helper()
	play := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://cdn.example.com/movie.mkv", http.StatusFound)
	})
	g, err := New(Options{
		Upstream: upstream, Mode: mode, Resolve: resolve, Play: play,
	})
	if err != nil {
		t.Fatalf("构造网关失败: %v", err)
	}
	return g
}

func staticResolve(context.Context, string, string) (string, error) {
	return "https://cdn.example.com/movie.mkv", nil
}

// 模拟公网客户端：Host 被隧道改成后端地址，靠转发头还原客户端真正访问的域名。
func publicPlaybackRequest(t *testing.T) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	req.Host = "127.0.0.1:6086"
	req.Header.Set("X-Forwarded-Host", "emby.example.com")
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Emby-Token", "user-token")
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	return req
}

func playbackSource(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON：%v / %s", err, body)
	}
	if len(out.MediaSources) != 1 {
		t.Fatalf("媒体源数量不对：%s", body)
	}
	return out.MediaSources[0]
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want PlayMode
	}{
		{"", ModeRedirect}, {"bogus", ModeRedirect}, {"REDIRECT", ModeRedirect},
		{"hybrid", ModeHybrid}, {" Hybrid ", ModeHybrid},
		{"direct", ModeDirect}, {"DIRECT", ModeDirect},
	} {
		if got := ParseMode(tc.in); got != tc.want {
			t.Errorf("%q → %q，期望 %q", tc.in, got, tc.want)
		}
	}
}

func TestClientBaseOf(t *testing.T) {
	cases := []struct {
		name, host, fwdHost, proto, scheme, wantHost string
		tls, ok                                     bool
	}{
		{"局域网直连", "192.168.1.10:6086", "", "", "http", "192.168.1.10:6086", false, true},
		{"HTTPS 直连", "emby.example.com", "", "", "https", "emby.example.com", true, true},
		{"经隧道转发", "127.0.0.1:6086", "emby.example.com", "https", "https", "emby.example.com", false, true},
		{"转发头是内网时不覆盖", "emby.example.com", "127.0.0.1:6086", "https", "https", "emby.example.com", false, true},
		{"Host 带路径要拒绝", "evil.com/xx", "", "", "", "", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Host = tc.host
			if tc.fwdHost != "" {
				req.Header.Set("X-Forwarded-Host", tc.fwdHost)
			}
			if tc.proto != "" {
				req.Header.Set("X-Forwarded-Proto", tc.proto)
			}
			if tc.tls {
				req.TLS = &tlsStateForTest
			}
			b, ok := clientBaseOf(req)
			if ok != tc.ok {
				t.Fatalf("ok=%v，期望 %v（%+v）", ok, tc.ok, b)
			}
			if ok && (b.Scheme != tc.scheme || b.Host != tc.wantHost) {
				t.Fatalf("拿到 %+v，期望 %s://%s", b, tc.scheme, tc.wantHost)
			}
		})
	}
}

// 只为了给测试构造一个非空 *tls.ConnectionState。
var tlsStateForTest = tls.ConnectionState{}

func TestNormalizeDirectURL(t *testing.T) {
	const raw = "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"
	got, ok := normalizeDirectURL(raw, clientBase{Scheme: "https", Host: "emby.example.com"})
	if !ok {
		t.Fatal("本站 /d/ 地址应该能归一化")
	}
	if want := "https://emby.example.com/d/q/04ac73.mkv?/F1.mkv"; got != want {
		t.Fatalf("归一化 = %q，期望 %q", got, want)
	}
	u, _ := url.Parse(got)
	if _, ok := strmTarget(got); !ok || !strings.HasPrefix(u.Path, "/d/") {
		t.Fatalf("归一化后丢了播放入口：%s", got)
	}
	for _, bad := range []string{
		"https://cdn.other.com/movie.mkv", "/media/F1.mkv", "", "http://x/d/",
	} {
		if _, ok := normalizeDirectURL(bad, clientBase{Scheme: "https", Host: "emby.example.com"}); ok {
			t.Errorf("不该归一化 %q", bad)
		}
	}
}

func TestDirectContainer(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv", "mkv"},
		{"http://192.168.1.10:6086/d/abc.mp4", "mp4"},
		{"/media/电影/F1.2160p.mkv.strm", "mkv"},
		{"http://192.168.1.10:6086/d/abc?/片名.2025", ""},
		{"http://192.168.1.10:6086/d/abc", ""},
	} {
		if got := directContainer(tc.raw); got != tc.want {
			t.Errorf("%q → %q，期望 %q", tc.raw, got, tc.want)
		}
	}
}

// 三档模式的字段差异：默认仍是最兼容的相对入口，hybrid 两条路都给，
// direct 只留 /d/ 直链。三者都必须关掉转码并修正容器。
func TestPlaybackModes(t *testing.T) {
	for _, tc := range []struct {
		mode      PlayMode
		wantPath  string
		wantDSU   string
		directPlay, directStream bool
	}{
		{ModeRedirect, "/Videos/437/stream?", "/Videos/437/stream?", false, true},
		{ModeHybrid, "https://emby.example.com/d/q/04ac73.mkv", "/Videos/437/stream?", true, true},
		{ModeDirect, "https://emby.example.com/d/q/04ac73.mkv", "", true, false},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			sp := strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv")
			up, _ := fakeEmbyPlayback(t, sp, testPlaybackJSON)
			g := modeGateway(t, up.URL, tc.mode, staticResolve)
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, publicPlaybackRequest(t))
			if rec.Code != http.StatusOK {
				t.Fatalf("PlaybackInfo 反代失败 %d: %s", rec.Code, rec.Body.String())
			}
			ms := playbackSource(t, rec.Body.Bytes())
			path, _ := ms["Path"].(string)
			dsu, _ := ms["DirectStreamUrl"].(string)
			if !strings.HasPrefix(path, tc.wantPath) {
				t.Fatalf("Path = %q，期望前缀 %q", path, tc.wantPath)
			}
			if !strings.HasPrefix(dsu, tc.wantDSU) || (tc.wantDSU == "" && dsu != "") {
				t.Fatalf("DirectStreamUrl = %q，期望前缀 %q", dsu, tc.wantDSU)
			}
			if ms["SupportsDirectPlay"] != tc.directPlay || ms["SupportsDirectStream"] != tc.directStream {
				t.Errorf("DirectPlay/DirectStream 不对：%v / %v", ms["SupportsDirectPlay"], ms["SupportsDirectStream"])
			}
			if ms["SupportsTranscoding"] != false || ms["Container"] != "mkv" ||
				ms["Protocol"] != "Http" || ms["IsRemote"] != true {
				t.Errorf("公共字段不对：%+v", ms)
			}
			if _, has := ms["TranscodingUrl"]; has {
				t.Error("TranscodingUrl 必须删掉")
			}
		})
	}
}

// hybrid 模式：播放器照着 Path 直连 /d/ 时，同端口的播放代理必须真的 302。
func TestHybridDirectPathHitsPlayProxy(t *testing.T) {
	sp := strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv")
	up, _ := fakeEmbyPlayback(t, sp, testPlaybackJSON)
	g := modeGateway(t, up.URL, ModeHybrid, staticResolve)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, publicPlaybackRequest(t))
	ms := playbackSource(t, rec.Body.Bytes())
	path, _ := ms["Path"].(string)
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	g.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, u.RequestURI(), nil))
	if rec2.Code != http.StatusFound ||
		rec2.Header().Get("Location") != "https://cdn.example.com/movie.mkv" {
		t.Fatalf("直连 /d/ 没 302：code=%d loc=%q", rec2.Code, rec2.Header().Get("Location"))
	}
}

// 推导不出客户端 base 时，新模式自动退回相对入口 —— 绝不能因为开了 hybrid 播不了。
func TestHybridFallsBackWithoutClientBase(t *testing.T) {
	sp := strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv")
	up, _ := fakeEmbyPlayback(t, sp, testPlaybackJSON)
	g := modeGateway(t, up.URL, ModeHybrid, staticResolve)
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	// 推导不出客户端访问地址时（例如某些反代不传 Host），新模式必须
	// 自动退回相对入口，而不是把内网地址塞给公网播放器。
	req.Host = ""
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	ms := playbackSource(t, rec.Body.Bytes())
	if path, dsu := ms["Path"], ms["DirectStreamUrl"]; path != dsu ||
		!strings.HasPrefix(dsu.(string), "/Videos/437/stream?") {
		t.Fatalf("应退回统一入口：path=%v dsu=%v", path, dsu)
	}
	if ms["SupportsDirectPlay"] != false {
		t.Fatal("回退后不该再声称支持 DirectPlay")
	}
}

// 115-station 的鉴权兼容：Bearer / ApiKey 参数 / UserId 都要跟着回给客户端。
func TestPlaybackCarriesClientAuthAndUserID(t *testing.T) {
	sp := strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv")
	up, _ := fakeEmbyPlayback(t, sp, testPlaybackJSON)
	g := modeGateway(t, up.URL, ModeRedirect, staticResolve)
	for _, tc := range []struct {
		name, query, want string
		headers           map[string]string
	}{
		{
			"Bearer", "", "api_key=bearer-token",
			map[string]string{
				"Authorization":         "Bearer bearer-token",
				"X-Emby-Authorization": `MediaBrowser UserId="u42"`,
			},
		},
		{
			"Jellyfin ApiKey", "?ApiKey=apikey-token", "api_key=apikey-token",
			map[string]string{"X-Emby-Authorization": `MediaBrowser UserId="u42"`},
		},
		{
			"复合头", "", "api_key=composite-token",
			map[string]string{
				"X-Emby-Authorization": `MediaBrowser Client="SenPlayer", UserId="u42", Token="composite-token"`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo"+tc.query, nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			ms := playbackSource(t, rec.Body.Bytes())
			dsu, _ := ms["DirectStreamUrl"].(string)
			if !strings.Contains(dsu, tc.want) || !strings.Contains(dsu, "UserId=u42") {
				t.Fatalf("播放地址没带上凭据 / UserId：%q", dsu)
			}
		})
	}
}

// STRMhub 的详情页预取：打开详情就后台换链，且响应体一个字节都不改。
func TestPrefetchOnItemDetail(t *testing.T) {
	const detailJSON = `{"Id":"437","Name":"F1","Path":"http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, detailJSON)
	}))
	t.Cleanup(srv.Close)

	calls := make(chan string, 4)
	g := modeGateway(t, srv.URL, ModeRedirect,
		func(context.Context, string, string) (string, error) {
			calls <- "q/04ac73.mkv"
			return "https://cdn.example.com/movie.mkv", nil
		})
	req := httptest.NewRequest(http.MethodGet, "/emby/Users/u1/Items/437", nil)
	req.Header.Set("X-Emby-Token", "tok")
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Body.String() != detailJSON {
		t.Fatalf("详情页响应被改了：%s", rec.Body.String())
	}
	select {
	case pick := <-calls:
		if pick != "q/04ac73.mkv" {
			t.Fatalf("预热用的 pickcode = %q", pick)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("详情页没有触发直链预热")
	}
}
