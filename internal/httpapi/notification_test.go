package httpapi

import (
	"strings"
	"testing"
	"unicode/utf8"

	"strm115/internal/notify"
)

// level/link 的映射是面板上唯一「一眼看懂发生了什么」的依据,逐事件钉死。
func TestBuildNotificationLevelAndLink(t *testing.T) {
	cases := []struct {
		event string
		level string
		link  string
	}{
		{"offline_add", "warning", "/media-transfer"},
		{"offline_ok", "success", "/media-transfer"},
		{"offline_fail", "danger", "/media-transfer"},
		{"sync_ok", "success", "/library"},
		{"sync_fail", "danger", "/library"},
		{"scrape_ok", "success", "/library"},
		{"ingest", "success", "/library"},
		{"subtitle_ok", "success", "/library"},
		{"delete", "danger", "/library"},
		{"organize_ok", "success", "/library"},
		{"waiting", "warning", "/"},
		{"unknown_event", "info", "/"},
		{"", "info", "/"},
	}
	for _, c := range cases {
		n, ok := buildNotification(c.event, notify.Message{
			Kind: notify.KindSystem, Title: "标题", Content: "正文",
		})
		if !ok {
			t.Fatalf("%s 不该被跳过", c.event)
		}
		if n.Level != c.level {
			t.Errorf("%s 的 level = %q,期望 %q", c.event, n.Level, c.level)
		}
		if n.Link != c.link {
			t.Errorf("%s 的 link = %q,期望 %q", c.event, n.Link, c.link)
		}
		if n.Event != c.event || n.Kind != "system" || n.Title != "标题" {
			t.Errorf("%s 的字段没带上: %+v", c.event, n)
		}
	}

	// 自带 link 优先于默认路由
	n, _ := buildNotification("sync_ok", notify.Message{Title: "t", Link: "  /custom  "})
	if n.Link != "/custom" {
		t.Fatalf("自带 link 应优先,实际 %q", n.Link)
	}
}

// 空标题、纯空白标题、测试推送(Force)都不该污染站内信箱。
func TestBuildNotificationSkip(t *testing.T) {
	for _, m := range []notify.Message{
		{Title: ""},
		{Title: "   "},
		{Title: "测试推送", Force: true},
	} {
		if _, ok := buildNotification("sync_ok", m); ok {
			t.Errorf("%+v 不该落库", m)
		}
	}
}

// 正文截断按 rune,中文不能被切坏。
func TestBuildNotificationTruncateContent(t *testing.T) {
	long := strings.Repeat("中", 600)
	n, ok := buildNotification("ingest", notify.Message{Title: "标题", Content: long})
	if !ok {
		t.Fatal("不该被跳过")
	}
	if got := utf8.RuneCountInString(n.Content); got != notificationMaxContent {
		t.Fatalf("内容应截到 %d 字,实际 %d", notificationMaxContent, got)
	}
	if !utf8.ValidString(n.Content) {
		t.Fatal("按 rune 截断不该产生非法 UTF-8")
	}
	short := "短正文"
	n2, _ := buildNotification("ingest", notify.Message{Title: "标题", Content: short})
	if n2.Content != short {
		t.Fatalf("短内容不该被改: %q", n2.Content)
	}
}

// recordNotification 即使写库失败(测试库可能还没建表)也只告警、不 panic、不抛错。
func TestRecordNotificationNeverBreaks(t *testing.T) {
	s, _ := newServerWithStore(t)
	s.recordNotification("sync_ok", notify.Message{Title: "同步完成", Content: "正文"})
	s.recordNotification("", notify.Message{Title: "测试推送", Force: true})
	s.recordNotification("offline_fail", notify.Message{Title: "   "})
}
