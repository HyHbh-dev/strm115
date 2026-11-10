package embygw

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 一部 MULTI REMUX 的真实流清单：内嵌繁体中文（BTM TRAD, idx 34）+ 本站
// 补字幕生成的简体外挂（.zh.srt, idx 36），两条都不带默认标记。
const testSubtitleJSON = `{"MediaSources":[{
  "Id":"mediasource_515",
  "Path":"http://192.168.1.10:6086/d/q/batman.mkv?/Batman.mkv",
  "Container":"mkv","Protocol":"Http",
  "SupportsDirectPlay":true,"SupportsDirectStream":true,"SupportsTranscoding":true,
  "MediaStreams":[
    {"Index":5,"Type":"Subtitle","Language":"eng","Title":"BTM","Codec":"subrip","IsExternal":false,"IsDefault":false},
    {"Index":34,"Type":"Subtitle","Language":"chi","Title":"BTM TRAD","Codec":"subrip","IsExternal":false,"IsDefault":false},
    {"Index":36,"Type":"Subtitle","Language":"zh","Codec":"srt","IsExternal":true,"IsDefault":false,
     "Path":"/media/电影/Batman.2026.2160p.zh.srt"}
  ]
}]}`

func subtitleStreams(t *testing.T, ms map[string]any) map[int]map[string]any {
	t.Helper()
	raw, _ := ms["MediaStreams"].([]any)
	out := map[int]map[string]any{}
	for _, v := range raw {
		st, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if idx, ok := streamIndex(st); ok {
			out[idx] = st
		}
	}
	return out
}

// 纯函数：只有存在本站简体轨时才指定默认，繁体轨保留但标注清楚。
func TestPreferSimplifiedSubtitles(t *testing.T) {
	var root map[string]any
	if err := json.Unmarshal([]byte(testSubtitleJSON), &root); err != nil {
		t.Fatal(err)
	}
	ms := root["MediaSources"].([]any)[0].(map[string]any)
	got := preferSimplifiedSubtitles(ms, "515", "mediasource_515", "user-token", "u42")
	if got != 36 {
		t.Fatalf("默认字幕轨应该是 36（简体），实际 %d", got)
	}
	if ms["DefaultSubtitleStreamIndex"] != 36 {
		t.Fatalf("DefaultSubtitleStreamIndex = %v", ms["DefaultSubtitleStreamIndex"])
	}
	streams := subtitleStreams(t, ms)
	if streams[36]["IsDefault"] != true {
		t.Error("简体轨没被设成默认")
	}
	du, _ := streams[36]["DeliveryUrl"].(string)
	for _, want := range []string{
		"/emby/Videos/515/mediasource_515/Subtitles/36/Stream.srt",
		"api_key=user-token", "UserId=u42",
	} {
		if !strings.Contains(du, want) {
			t.Errorf("DeliveryUrl 缺 %q：%q", want, du)
		}
	}
	if streams[36]["Title"] != "简体中文（本站）" {
		t.Errorf("简体轨标题 = %v", streams[36]["Title"])
	}
	if title, _ := streams[34]["Title"].(string); !strings.Contains(title, "繁体") {
		t.Errorf("繁体轨没标注，用户还是分不清两条 Chinese：%q", title)
	}
	if streams[34]["IsDefault"] != false {
		t.Error("繁体轨不该是默认")
	}
}

// 没有本站简体轨（还没补字幕 / 只有繁体源）：一个字节都不动，
// 不能把繁体轨藏掉让用户彻底没中文字幕。
func TestPreferSimplifiedWithoutOurSubtitle(t *testing.T) {
	body := `{"MediaSources":[{"Id":"1","MediaStreams":[
	  {"Index":34,"Type":"Subtitle","Language":"chi","Title":"BTM TRAD","IsExternal":false,"IsDefault":true}
	]}]}`
	var root map[string]any
	_ = json.Unmarshal([]byte(body), &root)
	ms := root["MediaSources"].([]any)[0].(map[string]any)
	if got := preferSimplifiedSubtitles(ms, "515", "1", "tok", ""); got != -1 {
		t.Fatalf("不该指定默认字幕，实际 %d", got)
	}
	if _, has := ms["DefaultSubtitleStreamIndex"]; has {
		t.Fatal("不该写入 DefaultSubtitleStreamIndex")
	}
	if subtitleStreams(t, ms)[34]["IsDefault"] != true {
		t.Fatal("原有的默认轨被改了")
	}
}

// 端到端：PlaybackInfo 反代后，客户端拿到的 JSON 里简体轨就是默认，
// 而且字幕地址是 Emby 真正认的那种（带 MediaSourceId 段）。
func TestPlaybackInfoSetsSimplifiedDefault(t *testing.T) {
	up, _ := fakeEmbyPlayback(t,
		strmFileShared(), testSubtitleJSON)
	g := testGatewayForPlayback(t, up.URL)
	req := httptest.NewRequest(http.MethodPost, "/emby/Items/515/PlaybackInfo", nil)
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
		t.Fatal(err)
	}
	ms := out.MediaSources[0]
	if ms["DefaultSubtitleStreamIndex"] != float64(36) {
		t.Fatalf("默认轨没写进响应：%v", ms["DefaultSubtitleStreamIndex"])
	}
	streams := subtitleStreams(t, ms)
	du, _ := streams[36]["DeliveryUrl"].(string)
	if !strings.Contains(du, "/emby/Videos/515/mediasource_515/Subtitles/36/Stream.srt") {
		t.Fatalf("字幕地址不对：%q", du)
	}
	if g.sourceIDFor("515") != "mediasource_515" {
		t.Fatal("MediaSource Id 没记住，简化字幕路径补不全")
	}
}

// 播放器按简化路径取字幕时，网关补上 MediaSourceId 段再反代。
// 之前这一路 Emby 直接 404，简体字幕根本加载不出来。
func TestSimpleSubtitlePathRewritten(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	g := testGatewayForPlayback(t, up.URL)
	g.rememberSourceID("437", "mediasource_437")

	for _, tc := range []struct {
		name, path, want string
	}{
		{"简化路径", "/emby/Videos/437/Subtitles/36/Stream.srt?api_key=tok",
			"proxied:/emby/Videos/437/mediasource_437/Subtitles/36/Stream.srt"},
		{"不带 emby 前缀", "/Videos/437/Subtitles/36/Stream.srt?api_key=tok",
			"proxied:/Videos/437/mediasource_437/Subtitles/36/Stream.srt"},
		{"已经带 sourceID 的完整路径", "/emby/Videos/437/mediasource_437/Subtitles/36/Stream.srt?api_key=tok",
			"proxied:/emby/Videos/437/mediasource_437/Subtitles/36/Stream.srt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			req.Header.Set("X-Emby-Token", "tok")
			rec := httptest.NewRecorder()
			g.ServeHTTP(rec, req)
			if !strings.Contains(rec.Body.String(), tc.want) {
				t.Fatalf("上游收到的路径不对：%q（期望含 %q）", rec.Body.String(), tc.want)
			}
		})
	}
}

// 没记住 sourceID 但请求里带了 MediaSourceId 参数，也应该能补全。
func TestSimpleSubtitlePathFromQuery(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	g := testGatewayForPlayback(t, up.URL)
	req := httptest.NewRequest(http.MethodGet,
		"/emby/Videos/437/Subtitles/36/Stream.srt?MediaSourceId=mediasource_437&api_key=tok", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "/mediasource_437/Subtitles/36/") {
		t.Fatalf("没从 query 补全：%q", rec.Body.String())
	}
}

// 两个都拿不到时维持原样反代，别把请求改坏。
func TestSimpleSubtitlePathNoSourceID(t *testing.T) {
	up, _ := fakeEmby(t, strmFile(t, "http://192.168.1.10:6086/d/q/04ac73.mkv?/F1.mkv"))
	g := testGatewayForPlayback(t, up.URL)
	req := httptest.NewRequest(http.MethodGet,
		"/emby/Videos/437/Subtitles/36/Stream.srt?api_key=tok", nil)
	req.Header.Set("X-Emby-Token", "tok")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "proxied:/emby/Videos/437/Subtitles/36/Stream.srt") {
		t.Fatalf("无 sourceID 时应该原样反代：%q", rec.Body.String())
	}
}

// fakeEmbyPlayback 的 itemPath 只用于 302 取流，这里不触发；给个合法占位即可。
func strmFileShared() string { return "http://192.168.1.10:6086/d/q/batman.mkv?/Batman.mkv" }
