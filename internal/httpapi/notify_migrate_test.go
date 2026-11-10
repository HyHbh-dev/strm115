package httpapi

// 通知事件的升级迁移。
//
// 这组测试盯的是一个「不报错但用户会困惑」的场景：老用户存过订阅清单，
// 新增事件如果不动它，升级后界面上新事件全是关的，看起来像「加了但没生效」。

import (
	"context"
	"strings"
	"testing"
)

func hasEvent(raw, id string) bool {
	for _, part := range strings.Split(raw, ",") {
		if strings.TrimSpace(part) == id {
			return true
		}
	}
	return false
}

func TestMigrateNotifyEventsAddsNew(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()

	// 上一版存下的清单：只有四个老事件
	if err := st.SetSetting(ctx, settingNotifyEvents, "sync_ok, sync_fail, offline_ok"); err != nil {
		t.Fatal(err)
	}
	s.migrateNotifyEvents(ctx)

	raw, _ := st.GetSetting(ctx, settingNotifyEvents)
	for _, id := range []string{"sync_ok", "sync_fail", "offline_ok", "ingest", "delete", "scrape_ok", "offline_add"} {
		if !hasEvent(raw, id) {
			t.Fatalf("迁移后应包含 %s: %q", id, raw)
		}
	}
	if strings.Count(raw, "sync_ok") != 1 {
		t.Fatalf("不该写出重复项: %q", raw)
	}
	if v, _ := st.GetSetting(ctx, settingNotifyEventsVersion); v != notifyEventsVersion {
		t.Fatalf("应打上版本标记，实际 %q", v)
	}
}

func TestMigrateNotifyEventsIdempotent(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	_ = st.SetSetting(ctx, settingNotifyEvents, "sync_ok")
	s.migrateNotifyEvents(ctx)

	// 用户随后手动关掉 ingest：再跑迁移不该把它加回来
	_ = st.SetSetting(ctx, settingNotifyEvents, "sync_ok")
	s.migrateNotifyEvents(ctx)
	raw, _ := st.GetSetting(ctx, settingNotifyEvents)
	if hasEvent(raw, "ingest") {
		t.Fatalf("打上版本标记后不该再补事件: %q", raw)
	}
}

func TestMigrateNotifyEventsRespectsAllOff(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	// 空字符串 = 用户明确全关。这个语义不能被迁移破坏。
	_ = st.SetSetting(ctx, settingNotifyEvents, "")
	s.migrateNotifyEvents(ctx)
	raw, _ := st.GetSetting(ctx, settingNotifyEvents)
	if strings.TrimSpace(raw) != "" {
		t.Fatalf("全关的配置不该被补上事件: %q", raw)
	}
}

func TestMigrateNotifyEventsNoSetting(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	s.migrateNotifyEvents(ctx)
	if _, ok := st.GetSetting(ctx, settingNotifyEvents); ok {
		t.Fatal("从没配过就不该凭空写一行（那会让「全开」变成「部分开」）")
	}
}
