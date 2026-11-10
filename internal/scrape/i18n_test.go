package scrape

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHasChinese(t *testing.T) {
	cases := map[string]bool{
		"空枪":                 true,
		"Kong Qiang":         false,
		"碟中谍8：最终清算":          true,
		"Spider-Man: No Way": false,
		"":                   false,
		"3 Body Problem 三体":  true,
	}
	for in, want := range cases {
		if got := hasChinese(in); got != want {
			t.Errorf("hasChinese(%q) = %v, 期望 %v", in, got, want)
		}
	}
}

func TestZhGenres(t *testing.T) {
	got := zhGenres([]string{"Action", "Sci-Fi", "Drama", "action", "UnknownGenre"})
	want := []string{"动作", "科幻", "剧情", "UnknownGenre"}
	if len(got) != len(want) {
		t.Fatalf("zhGenres = %v, 期望 %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("zhGenres = %v, 期望 %v", got, want)
		}
	}
}

// TestLocalizePrefersFilenameTitle 中文片名必须来自文件名，而不是机翻。
// 「空枪」这种发行方译名机翻是翻不出来的（会变成「Kong Qiang」或「空枪炮」）。
func TestLocalizePrefersFilenameTitle(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	opt := DefaultOptions()
	opt.Translate = false

	m := svc.localize(context.Background(), Meta{
		Title:  "Kong Qiang",
		Genres: []string{"Action", "Crime"},
	}, "空枪", opt)

	if m.TitleCN != "空枪" {
		t.Errorf("TitleCN = %q, 期望空枪", m.TitleCN)
	}
	if m.displayTitle() != "空枪" {
		t.Errorf("displayTitle = %q, 期望空枪", m.displayTitle())
	}
	if strings.Join(m.Genres, ",") != "动作,犯罪" {
		t.Errorf("Genres = %v, 期望中文", m.Genres)
	}
}

// TestLocalizeTranslatesOverview 英文简介要变成中文，且结果写进缓存：
// 第二次调用即使翻译接口已经关掉，也应该直接命中缓存。
func TestLocalizeTranslatesOverview(t *testing.T) {
	var hits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]any{"content": "这是一段中文简介。"}},
			},
		})
	}))

	svc, _, _, _ := newTestService(t)
	opt := DefaultOptions()
	opt.APIBase = server.URL
	opt.Workers = 1

	m := svc.localize(context.Background(), Meta{
		Title:    "The Test Movie",
		TitleCN:  "", // 文件名里没有中文名，只能翻
		Overview: "A movie used by tests.",
	}, "The Test Movie", opt)

	if m.Overview != "这是一段中文简介。" {
		t.Fatalf("Overview = %q, 期望译文", m.Overview)
	}
	if hits == 0 {
		t.Fatal("应该请求了一次翻译接口")
	}

	// 关掉接口后再来一次：必须靠缓存，不能发请求（发了就会失败）
	server.Close()
	before := hits
	again := svc.localize(context.Background(), Meta{
		Title:    "The Test Movie",
		Overview: "A movie used by tests.",
	}, "The Test Movie", opt)
	if again.Overview != "这是一段中文简介。" {
		t.Errorf("第二次 Overview = %q, 期望命中缓存", again.Overview)
	}
	if hits != before {
		t.Errorf("第二次不该再发请求，实际多发了 %d 次", hits-before)
	}
}

// TestLocalizeSkipsWhenOriginal 关掉中文模式时不该动任何字段。
func TestLocalizeSkipsWhenOriginal(t *testing.T) {
	svc, _, _, _ := newTestService(t)
	opt := DefaultOptions()
	opt.Language = "original"
	opt.Translate = true

	m := svc.localize(context.Background(), Meta{
		Title:    "Kong Qiang",
		Overview: "English only.",
		Genres:   []string{"Action"},
	}, "空枪", opt)

	if m.TitleCN != "" {
		t.Errorf("original 模式下不该写 TitleCN，实际 %q", m.TitleCN)
	}
	if m.Overview != "English only." || m.Genres[0] != "Action" {
		t.Errorf("original 模式下不该改内容: %+v", m)
	}
}
