package store

import (
	"context"
	"strings"
	"time"
)

// Notification 是通知中心里的一条站内消息。
//
// 和外推消息(notify.Message)分开存:关掉某个渠道或某个事件的外推订阅,
// 只是「不往外发」,站内仍要留下记录供用户回看。
type Notification struct {
	ID        int64  `json:"id"`
	Event     string `json:"event"`
	Kind      string `json:"kind"`
	Title     string `json:"title"`
	Content   string `json:"content"`
	Link      string `json:"link"`
	Level     string `json:"level"`
	ReadAt    string `json:"read_at"`
	CreatedAt string `json:"created_at"`
}

const notificationCols = `id, event, kind, title, content, link, level, read_at, created_at`

func scanNotification(sc interface{ Scan(...any) error }) (Notification, error) {
	var n Notification
	err := sc.Scan(&n.ID, &n.Event, &n.Kind, &n.Title, &n.Content,
		&n.Link, &n.Level, &n.ReadAt, &n.CreatedAt)
	return n, err
}

// InsertNotification 落一条站内消息,顺手把总量裁到最近 300 条。
//
// 裁剪用 id 而不是 created_at:自增 id 天然是写入顺序,不受机器时钟回拨影响。
func (s *Store) InsertNotification(ctx context.Context, n Notification) error {
	if n.CreatedAt == "" {
		n.CreatedAt = time.Now().Format(time.RFC3339)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO notifications(event, kind, title, content, link, level, read_at, created_at)
		 VALUES(?,?,?,?,?,?,?,?)`,
		n.Event, n.Kind, n.Title, n.Content, n.Link, n.Level, n.ReadAt, n.CreatedAt)
	if err != nil {
		return err
	}
	// 通知是流水账,不设上限会无限涨;只保留最近 300 条。
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM notifications WHERE id NOT IN
		 (SELECT id FROM notifications ORDER BY id DESC LIMIT 300)`)
	return err
}

// ListNotifications 按写入倒序列最近 limit 条。
func (s *Store) ListNotifications(ctx context.Context, limit int) ([]Notification, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+notificationCols+` FROM notifications ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Notification{}
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// CountUnreadNotifications 只数还没标已读的。
func (s *Store) CountUnreadNotifications(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM notifications WHERE read_at=''`).Scan(&n)
	return n, err
}

// MarkNotificationsRead 标已读:all=true 全标,否则按 ids 批量标。
// 只碰 read_at=” 的行 —— 重复点「全部已读」不会把首次已读时间冲掉(幂等)。
func (s *Store) MarkNotificationsRead(ctx context.Context, ids []int64, all bool) error {
	now := time.Now().Format(time.RFC3339)
	if all {
		_, err := s.db.ExecContext(ctx,
			`UPDATE notifications SET read_at=? WHERE read_at=''`, now)
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+1)
	args = append(args, now)
	for _, id := range ids {
		args = append(args, id)
	}
	q := `UPDATE notifications SET read_at=? WHERE read_at='' AND id IN (?` +
		strings.Repeat(",?", len(ids)-1) + `)`
	_, err := s.db.ExecContext(ctx, q, args...)
	return err
}
