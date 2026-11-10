package embygw

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeEmbyPlayback 除了条目查询，还能回一份 PlaybackInfo JSON。
func fakeEmbyPlayback(t *testing.T, itemPath, playbackJSON string) (*httptest.Server, *int) {
	t.Helper()
	proxied := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case rePlaybackInfo.MatchString(r.URL.Path):
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, playbackJSON)
		case r.URL.Path == "/Items" || r.URL.Path == "/emby/Items":
			w.Header().Set("Content-Type", "application/json")
			body, _ := json.Marshal(map[string]any{
				"Items": []map[string]string{{"Id": "437", "Path": itemPath}},
			})
			_, _ = w.Write(body)
		default:
			proxied++
			_, _ = io.WriteString(w, "proxied:"+r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &proxied
}

// 实测公网 Emby 返回的 Path 就是 .strm 内容那个 /d/ URL（且没有 DirectStreamUrl）。
const testPlaybackJSON = `{"MediaSources":[{
  "Id":"mediasource_437",
  "Path":"http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv","Container":"mkv",
  "Protocol":"Http","IsRemote":true,
  "SupportsDirectPlay":true,"SupportsDirectStream":true,"SupportsTranscoding":true,
  "DirectStreamUrl":"/emby/videos/437/stream?static=true",
  "TranscodingUrl":"/emby/videos/437/master.m3u8"
}]}`

func testGatewayForPlayback(t *testing.T, upstream string) *Gateway {
	t.Helper()
	return newGateway(t, upstream, func(context.Context, string, string) (string, error) {
		return "https://cdn.example.com/movie.mkv", nil
	})
}

// 参数名大小写不敏感：SenPlayer 发的正是 /stream.mkv?...&Static=true。
func TestStaticParamCaseInsensitive(t *testing.T) {
	up, proxied := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	g := testGatewayForPlayback(t, up.URL)

	for _, q := range []string{
		"Static=true&MediaSourceId=mediasource_437",
		"STATIC=TRUE",
		"static=true",
		"StAtIc=TrUe",
	} {
		req := httptest.NewRequest(http.MethodGet,
			"/emby/videos/437/stream.mkv?DeviceId=ios1&"+q, nil)
		req.Header.Set("X-Emby-Token", "tok")
		rec := httptest.NewRecorder()
		g.ServeHTTP(rec, req)
		if rec.Code != http.StatusFound {
			t.Errorf("%q 应该 302 直连，实际 %d（代理 %d 次）", q, rec.Code, *proxied)
		}
	}
	// 明确要转码的请求必须留在 Emby，不能把 m3u8 换成 mkv 直链。
	req := httptest.NewRequest(http.MethodGet,
		"/emby/Videos/437/stream?Static=false&VideoCodec=h264", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || *proxied == 0 {
		t.Fatalf("转码请求应回 Emby，实际 code=%d proxied=%d", rec.Code, *proxied)
	}
}

// 核心闭环：PlaybackInfo 被改写 → 客户端请求改写后的地址 → 302 到 CDN。
// 这正是 115-station / STRMhub 的做法，不再依赖客户端原本发哪种 stream 路径。
func TestPlaybackInfoRewritesToOwn302Entry(t *testing.T) {
	sp := strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv")
	up, _ := fakeEmbyPlayback(t, sp, testPlaybackJSON)
	g := testGatewayForPlayback(t, up.URL)

	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", "user-token")
	req.Header.Set("User-Agent", "SenPlayer/1.0.0")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PlaybackInfo 反代失败 %d: %s", rec.Code, rec.Body.String())
	}

	var out struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是 JSON：%v / %s", err, rec.Body.String())
	}
	if len(out.MediaSources) != 1 {
		t.Fatalf("媒体源数量不对：%s", rec.Body.String())
	}
	ms := out.MediaSources[0]
	loc, _ := ms["DirectStreamUrl"].(string)
	if !strings.HasPrefix(loc, "/Videos/437/stream?") ||
		!strings.Contains(loc, "Static=true") ||
		!strings.Contains(loc, "api_key=user-token") ||
		!strings.Contains(loc, "MediaSourceId=mediasource_437") {
		t.Fatalf("播放地址没改成 302 入口：%q", loc)
	}
	if ms["Path"] != loc || ms["SupportsTranscoding"] != false ||
		ms["SupportsDirectStream"] != true || ms["SupportsDirectPlay"] != false {
		t.Errorf("字段没按直连设置：%+v", ms)
	}
	if _, has := ms["TranscodingUrl"]; has {
		t.Error("TranscodingUrl 必须删掉，否则 SenPlayer 会去走 HLS")
	}
	if ms["Protocol"] != "Http" || ms["IsRemote"] != true {
		t.Errorf("Protocol / IsRemote 不对：%+v", ms)
	}

	// 客户端拿到这个地址后真的去请求：必须 302，且不再经过 Emby 代理。
	rec2 := httptest.NewRecorder()
	g.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, loc, nil))
	if rec2.Code != http.StatusFound {
		t.Fatalf("改写后的地址应 302，实际 %d（body=%s）", rec2.Code, rec2.Body.String())
	}
	if got := rec2.Header().Get("Location"); got != "https://cdn.example.com/movie.mkv" {
		t.Errorf("Location = %q", got)
	}
}

// .strm 文件路径那种写法也要认（不同 Emby 版本 / 库类型会变）。
func TestPlaybackInfoRewritesStrmFilePath(t *testing.T) {
	body := `{"MediaSources":[{"Id":"1","Path":"/media/电影/F1.mkv.strm","SupportsTranscoding":true,"TranscodingUrl":"/hls"}]}`
	up, _ := fakeEmbyPlayback(t, "/media/F1.mkv.strm", body)
	g := testGatewayForPlayback(t, up.URL)
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "/Videos/437/stream?") {
		t.Fatalf(".strm 文件路径也应改写：%s", rec.Body.String())
	}
}

// 不是本站 .strm、或者客户端没带凭据时，一个字节都不改 —— 别把本来能播的弄坏。
func TestPlaybackInfoLeavesOthersAlone(t *testing.T) {
	for _, tc := range []struct {
		name, path, jsonBody string
		withToken            bool
	}{
		{"本地真实文件", "/media/real.mkv", `{"MediaSources":[{"Id":"1","Path":"/media/real.mkv","TranscodingUrl":"/hls"}]}`, true},
		{"别家 http 直链", "/media/real.mkv", `{"MediaSources":[{"Id":"1","Path":"https://cdn.other.com/movie.mkv","TranscodingUrl":"/hls"}]}`, true},
		{"无凭据", "/media/F1.mkv.strm", testPlaybackJSON, false},
		{"带错误码", "/media/F1.mkv.strm", `{"ErrorCode":"AccessDenied"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up, _ := fakeEmbyPlayback(t, "/media/F1.mkv.strm", tc.jsonBody)
			g := testGatewayForPlayback(t, up.URL)
			req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
			if tc.withToken {
				req.Header.Set("X-Emby-Token", "tok")
			}
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if rec.Body.String() != tc.jsonBody {
				t.Fatalf("不该改写：\n got %s\nwant %s", rec.Body.String(), tc.jsonBody)
			}
		})
	}
}

// 复合鉴权头里的 token 也要能回给客户端，否则这类客户端改写后取流会 401。
func TestPlaybackInfoUsesCompositeAuthToken(t *testing.T) {
	up, _ := fakeEmbyPlayback(t, "/media/F1.mkv.strm", testPlaybackJSON)
	g := testGatewayForPlayback(t, up.URL)
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/437/PlaybackInfo", nil)
	req.Header.Set("X-Emby-Authorization",
		`MediaBrowser Client="SenPlayer", Device="iPhone", Token="tok-from-auth"`)
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	var out struct {
		MediaSources []map[string]any `json:"MediaSources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	loc, _ := out.MediaSources[0]["DirectStreamUrl"].(string)
	if !strings.Contains(loc, "api_key=tok-from-auth") {
		t.Fatalf("复合头里的 token 没带进播放地址：%q", loc)
	}
}
