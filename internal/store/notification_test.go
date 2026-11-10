package store

import (
	"context"
	"fmt"
	"testing"
)

// openNotificationTest 复用项目自带的临时库,再补建 notifications 表。
// 主 agent 会把同样的 DDL 放进 migrate() 的 stmts;测试库是独立 Open 的,
// 这里显式建一次,保证测试不依赖接线进度。IF NOT EXISTS 幂等,重复无害。
func openNotificationTest(t *testing.T) *Store {
	t.Helper()
	s := openTest(t)
	if _, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS notifications (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		event      TEXT NOT NULL DEFAULT '',
		kind       TEXT NOT NULL DEFAULT '',
		title      TEXT NOT NULL DEFAULT '',
		content    TEXT NOT NULL DEFAULT '',
		link       TEXT NOT NULL DEFAULT '',
		level      TEXT NOT NULL DEFAULT 'info',
		read_at    TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at DESC)`); err != nil {
		t.Fatal(err)
	}
	return s
}

// 插入 → 倒序列出 → 未读数 → 单条/全部标已读 → 幂等 → 空 ids,一条链路走完。
func TestNotificationCRUD(t *testing.T) {
	s := openNotificationTest(t)
	ctx := context.Background()

	for i := 1; i <= 3; i++ {
		if err := s.InsertNotification(ctx, Notification{
			Event: "sync_ok", Kind: "system", Title: fmt.Sprintf("第 %d 条", i),
			Content: "正文", Link: "/library", Level: "success",
		}); err != nil {
			t.Fatalf("插入失败: %v", err)
		}
	}

	list, err := s.ListNotifications(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 3 {
		t.Fatalf("应有 3 条,实际 %d", len(list))
	}
	// 按 id 倒序:最后插的排最前
	if list[0].Title != "第 3 条" || list[2].Title != "第 1 条" {
		t.Fatalf("列表没按 id 倒序: %+v", list)
	}
	if list[0].CreatedAt == "" {
		t.Fatal("created_at 没自动填")
	}
	if list[0].ReadAt != "" || list[0].Level != "success" || list[0].Link != "/library" {
		t.Fatalf("字段读回不对: %+v", list[0])
	}

	unread, err := s.CountUnreadNotifications(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unread != 3 {
		t.Fatalf("未读应为 3,实际 %d", unread)
	}

	// 单条标已读
	firstID := list[0].ID
	if err := s.MarkNotificationsRead(ctx, []int64{firstID}, false); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.CountUnreadNotifications(ctx)
	if unread != 2 {
		t.Fatalf("标一条后未读应为 2,实际 %d", unread)
	}
	after, _ := s.ListNotifications(ctx, 0)
	var readAt string
	for _, n := range after {
		if n.ID == firstID {
			readAt = n.ReadAt
		}
	}
	if readAt == "" {
		t.Fatal("标已读后 read_at 仍为空")
	}

	// 幂等:重复标同一条,read_at 不该被新时间覆盖
	if err := s.MarkNotificationsRead(ctx, []int64{firstID}, false); err != nil {
		t.Fatal(err)
	}
	after2, _ := s.ListNotifications(ctx, 0)
	for _, n := range after2 {
		if n.ID == firstID && n.ReadAt != readAt {
			t.Fatalf("重复标已读把时间冲掉了: %q -> %q", readAt, n.ReadAt)
		}
	}

	// 空 ids 直接返回,不改数据
	if err := s.MarkNotificationsRead(ctx, nil, false); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.CountUnreadNotifications(ctx)
	if unread != 2 {
		t.Fatalf("空 ids 不该改动数据,实际未读 %d", unread)
	}

	// 全部标已读
	if err := s.MarkNotificationsRead(ctx, nil, true); err != nil {
		t.Fatal(err)
	}
	unread, _ = s.CountUnreadNotifications(ctx)
	if unread != 0 {
		t.Fatalf("全部标已读后未读应为 0,实际 %d", unread)
	}
}

func TestNotificationListLimit(t *testing.T) {
	s := openNotificationTest(t)
	ctx := context.Background()
	for i := 0; i < 260; i++ {
		if err := s.InsertNotification(ctx, Notification{Title: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.ListNotifications(ctx, 999)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 200 {
		t.Fatalf("limit>200 应夹到 200,实际 %d", len(list))
	}
	list, _ = s.ListNotifications(ctx, -1)
	if len(list) != 50 {
		t.Fatalf("limit<=0 应用 50,实际 %d", len(list))
	}
}

// 容量裁剪只能删最老的,最新一条都不许误删。
func TestNotificationTrimKeepsNewest(t *testing.T) {
	s := openNotificationTest(t)
	ctx := context.Background()
	for i := 0; i < 305; i++ {
		if err := s.InsertNotification(ctx, Notification{Title: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	var total, minID, maxID int
	if err := s.db.QueryRow(`SELECT COUNT(*), COALESCE(MIN(id),0), COALESCE(MAX(id),0) FROM notifications`).
		Scan(&total, &minID, &maxID); err != nil {
		t.Fatal(err)
	}
	if total != 300 {
		t.Fatalf("裁剪后应剩 300 条,实际 %d", total)
	}
	// 共 305 条(id 1..305),删掉最老的 5 条,保留 id 6..305
	if minID != 6 || maxID != 305 {
		t.Fatalf("裁剪范围不对: min=%d max=%d", minID, maxID)
	}
	list, err := s.ListNotifications(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if list[0].Title != "304" {
		t.Fatalf("最新一条应还在且排最前,实际 %q", list[0].Title)
	}
}
