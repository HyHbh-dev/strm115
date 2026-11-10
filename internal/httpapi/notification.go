package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"strm115/internal/notify"
	"strm115/internal/store"
)

const notificationMaxContent = 500

// buildNotification 把一条外推消息翻译成站内记录;ok=false 表示不该落库。
// 决策和写库分开,便于单测覆盖 level/link 映射与跳过规则。
func buildNotification(event string, m notify.Message) (store.Notification, bool) {
	// 测试推送(Force)和没标题的消息都不该进站内信箱。
	if m.Force || strings.TrimSpace(m.Title) == "" {
		return store.Notification{}, false
	}
	link := strings.TrimSpace(m.Link)
	if link == "" {
		link = defaultNotificationLink(event)
	}
	return store.Notification{
		Event:   event,
		Kind:    string(m.Kind),
		Title:   m.Title,
		Content: truncateRunes(m.Content, notificationMaxContent),
		Link:    link,
		Level:   notificationLevel(event),
	}, true
}

// recordNotification 落库一条站内消息。失败只告警,绝不把错误抛回主流程:
// 通知中心是旁路能力,不能让同步/下载因为一条站内信写失败而中断。
func (s *Server) recordNotification(event string, m notify.Message) {
	n, ok := buildNotification(event, m)
	if !ok {
		return
	}
	if err := s.st.InsertNotification(context.Background(), n); err != nil {
		s.log.Warn("站内通知落库失败", "err", err)
	}
}

// defaultNotificationLink 没带 link 时按事件给个能点进去的默认路由。
func defaultNotificationLink(event string) string {
	switch {
	case strings.HasPrefix(event, "offline_"):
		return "/media-transfer"
	case strings.HasPrefix(event, "sync_"):
		return "/library"
	}
	switch event {
	case "scrape_ok", "ingest", "subtitle_ok", "delete", "organize_ok":
		return "/library"
	default:
		return "/"
	}
}

// notificationLevel 决定列表里圆点的颜色。
func notificationLevel(event string) string {
	switch {
	case strings.Contains(event, "fail"), strings.Contains(event, "delete"):
		return "danger"
	case event == "offline_add", event == "waiting":
		return "warning"
	case strings.HasSuffix(event, "_ok"), event == "ingest":
		return "success"
	default:
		return "info"
	}
}

// truncateRunes 按 rune 截断,避免把中文切成半个字。
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// ==================== 接口 ====================

// handleListNotifications GET /api/notifications
func (s *Server) handleListNotifications(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	ctx := r.Context()
	items, err := s.st.ListNotifications(ctx, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	unread, err := s.st.CountUnreadNotifications(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if items == nil {
		items = []store.Notification{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "unread": unread})
}

// handleReadNotifications POST /api/notifications/read
func (s *Server) handleReadNotifications(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []int64 `json:"ids"`
		All bool    `json:"all"`
	}
	// 请求体可以为空(老前端/curl 都不带),解析失败就当「什么都没传」。
	_ = readJSON(r, &body)
	ctx := r.Context()
	if err := s.st.MarkNotificationsRead(ctx, body.IDs, body.All); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	unread, err := s.st.CountUnreadNotifications(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "unread": unread})
}
