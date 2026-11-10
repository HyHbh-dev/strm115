package resource

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 机器人（@torrenthuntbot 这类）没有 /s/ 预览页：t.me 会 302 到落地页，
// 页面里 0 条消息。必须报出「不是公开频道」，否则界面只显示 0 条，
// 用户会一直以为是搜索词写错了。
func TestSearchTelegramRejectsBot(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/s/") {
			http.Redirect(w, r, "/torrenthuntbot", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<html><head><title>Telegram: Launch @torrenthuntbot</title></head>` +
			`<body><div class="tgme_page"><div class="tgme_page_title">Torrent Hunt</div></div></body></html>`))
	}))
	defer srv.Close()

	old := tgBase
	tgBase = srv.URL
	defer func() { tgBase = old }()

	_, err := searchTelegram(context.Background(), srv.Client(), "torrenthuntbot", "F1", 5)
	if err == nil {
		t.Fatal("机器人应该报错，实际返回了空结果")
	}
	if !strings.Contains(err.Error(), "不是公开频道") {
		t.Fatalf("错误文案应说明不是公开频道，实际 %q", err)
	}
}

// 正常的公开频道：/s/<name> 留在原地，照常解析消息。
func TestSearchTelegramKeepsPublicChannel(t *testing.T) {
	page := `<html><body><main>`
	page += `<div class="tgme_widget_message_wrap"><div class="tgme_widget_message" data-post="QukanMovie/12">` +
		`<div class="tgme_widget_message_text js-message_text" dir="auto">F1：狂飙飞车 4K 原盘 https://115.com/s/abcd#pass</div>` +
		`<time datetime="2026-11-08T10:00:00+00:00"></time></div></div>`
	page += `</main></body></html>`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/s/QukanMovie" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(page))
	}))
	defer srv.Close()

	old := tgBase
	tgBase = srv.URL
	defer func() { tgBase = old }()

	items, err := searchTelegram(context.Background(), srv.Client(), "QukanMovie", "F1", 5)
	if err != nil {
		t.Fatalf("公开频道不该报错: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("应该解析出 1 条，实际 %d 条", len(items))
	}
	if items[0].SourceKey != "tg:QukanMovie" {
		t.Fatalf("来源标记 = %q", items[0].SourceKey)
	}
}

// 频道页偶尔会慢，别让一条坏频道把整轮搜索拖死 —— 这里只验证超时能正常返回错误。
func TestSearchTelegramTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	}))
	defer srv.Close()

	old := tgBase
	tgBase = srv.URL
	defer func() { tgBase = old }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := searchTelegram(ctx, srv.Client(), "QukanMovie", "F1", 5); err == nil {
		t.Fatal("超时应该返回错误")
	}
}
