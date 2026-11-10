package httpapi

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	"strm115/internal/notify"
	"strm115/internal/store"
)

// pushEvent 是「外推」的入口，站内消息不能因为没配渠道、或用户关掉了这个
// 事件的外推订阅就消失 —— 关的是推送，不是记录。这条测试钉死这个接线。
func TestPushEventRecordsInbox(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	s := &Server{st: st, log: slog.Default(), notify: notify.New(notify.Config{})}
	// 只订阅 sync_ok：offline_ok 的外推是关的。
	if err := st.SetSetting(ctx, settingNotifyEvents, "sync_ok"); err != nil {
		t.Fatal(err)
	}

	s.pushEvent("offline_ok", notify.KindTransfer, "离线下载完成", "本轮下了 3 条")
	items, err := st.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("关掉外推的事件也该留一条站内记录，拿到 %d 条", len(items))
	}
	if items[0].Event != "offline_ok" || items[0].Title != "离线下载完成" {
		t.Fatalf("记录内容不对: %+v", items[0])
	}
	if items[0].Link != "/media-transfer" || items[0].Level != "success" {
		t.Fatalf("默认跳转/级别不对: %+v", items[0])
	}
	unread, err := st.CountUnreadNotifications(ctx)
	if err != nil || unread != 1 {
		t.Fatalf("未读数应为 1，拿到 %d (err=%v)", unread, err)
	}

	// 全部已读后未读归零，但记录还在。
	if err := st.MarkNotificationsRead(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	unread, _ = st.CountUnreadNotifications(ctx)
	if unread != 0 {
		t.Fatalf("标完已读还有 %d 条未读", unread)
	}
	items, _ = st.ListNotifications(ctx, 10)
	if len(items) != 1 || items[0].ReadAt == "" {
		t.Fatalf("已读状态没落库: %+v", items)
	}
}
