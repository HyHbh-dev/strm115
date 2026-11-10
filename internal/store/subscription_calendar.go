package store

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// 订阅专有日历里每一集的状态。
//
// pending = 还没到 air_date；waiting = 到期了该搜（含「搜过但没搜到，等明天」）；
// done = 已提交离线；skipped = 起始集之前的集，明确不补。
// 状态只由轮询器和建订阅时的水位线推进，前端只读。
const (
	SubEpPending = "pending"
	SubEpWaiting = "waiting"
	SubEpDone    = "done"
	SubEpSkipped = "skipped"
)

// SubEpisode 是一条订阅里的某一集，air_date 是 TMDB 给的单集播出日（YYYY-MM-DD）。
//
// TMDB 有些老剧 / 未定档的季不给 air_date，这时 AirDate 为空：它进不了按日
// 的周历，但仍然要参与搜索，否则这些集永远不会被订阅抓到。
type SubEpisode struct {
	ID             int64  `json:"id"`
	SubscriptionID int64  `json:"subscription_id"`
	Season         int    `json:"season"`
	Episode        int    `json:"episode"`
	AirDate        string `json:"air_date"`
	Name           string `json:"name,omitempty"`
	Status         string `json:"status"`
	TryCount       int    `json:"try_count"`
	NextTryAt      string `json:"next_try_at,omitempty"`
	FoundAt        string `json:"found_at,omitempty"`
	LastResult     string `json:"last_result,omitempty"`

	// 下面这些是联 subscriptions 带出来的展示字段，不落在本表里。
	Title         string `json:"title,omitempty"`
	OriginalTitle string `json:"original_title,omitempty"`
	TMDbID        int    `json:"tmdb_id,omitempty"`
	Poster        string `json:"poster,omitempty"`
	State         string `json:"sub_state,omitempty"`
}

// Code 给人看的集号 S02E05。
func (e SubEpisode) Code() string {
	return fmt.Sprintf("S%02dE%02d", e.Season, e.Episode)
}

const subEpCols = `id, subscription_id, season, episode, air_date, name, status,
	try_count, next_try_at, found_at, last_result`

func scanSubEp(sc interface{ Scan(...any) error }) (SubEpisode, error) {
	var e SubEpisode
	err := sc.Scan(&e.ID, &e.SubscriptionID, &e.Season, &e.Episode, &e.AirDate,
		&e.Name, &e.Status, &e.TryCount, &e.NextTryAt, &e.FoundAt, &e.LastResult)
	return e, err
}

// UpsertSubscriptionEpisodes 把 TMDB 拉到的集批量写进日历。
//
// 刷新 TMDB 时会重复调用：只更新 air_date / name，绝不重置 status / try_count /
// found_at / next_try_at —— 否则每天早上刷新一次，昨天搜到的集又会变回「没搜过」。
// 新增的集按调用方给的状态插入。
func (s *Store) UpsertSubscriptionEpisodes(ctx context.Context, subID int64, eps []SubEpisode) error {
	if len(eps) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().Format(time.RFC3339)
	const q = `INSERT INTO subscription_episodes
		(subscription_id, season, episode, air_date, name, status, try_count,
		 next_try_at, found_at, last_result, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(subscription_id, season, episode) DO UPDATE SET
		  air_date = excluded.air_date,
		  name = excluded.name,
		  updated_at = excluded.updated_at`
	for _, e := range eps {
		if _, err := tx.ExecContext(ctx, q, subID, e.Season, e.Episode, e.AirDate,
			e.Name, e.Status, e.TryCount, e.NextTryAt, e.FoundAt, e.LastResult, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DeleteSubscriptionEpisodes 删订阅时把它的日历一起清掉，不留孤儿行。
func (s *Store) DeleteSubscriptionEpisodes(ctx context.Context, subID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscription_episodes WHERE subscription_id = ?`, subID)
	return err
}

// HasSubscriptionCalendar 判断这条订阅是不是走「专有日历驱动」。
//
// 老订阅（升级前建的）没有日历行，轮询器仍按旧的水位线逻辑跑，避免一升级
// 就把所有在追的剧停掉。
func (s *Store) HasSubscriptionCalendar(ctx context.Context, subID int64) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM subscription_episodes WHERE subscription_id = ?`, subID).Scan(&n)
	return n > 0, err
}

// SkipEpisodesBefore 把「起始集及之前」的集标成 skipped。
//
// 用户选「从 S02E05 开始订阅」时，前两季不该再被搜出来下载。只标 done 之外
// 的行：已经下载过的集保持 done，别把历史进度抹掉。
func (s *Store) SkipEpisodesBefore(ctx context.Context, subID int64, season, episode int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription_episodes SET status = ?, updated_at = ?
		 WHERE subscription_id = ? AND status != ?
		   AND (season < ? OR (season = ? AND episode <= ?))`,
		SubEpSkipped, time.Now().Format(time.RFC3339), subID, SubEpDone,
		season, season, episode)
	return err
}

// UnskipEpisodesAfter 把起播线之后、之前被跳过的集恢复成待搜。
//
// 用户把起播线从 S02E05 挪回 S01E01 时，前面那些 skipped 不该永远躺着：
// 恢复成「明天再排一次」的状态，让轮询器自然接手。已经 done 的集不动。
func (s *Store) UnskipEpisodesAfter(ctx context.Context, subID int64, season, episode int, today string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription_episodes
		 SET status = CASE WHEN air_date > ? THEN ? ELSE ? END,
		     next_try_at = '', updated_at = ?
		 WHERE subscription_id = ? AND status = ?
		   AND (season > ? OR (season = ? AND episode > ?))`,
		today, SubEpPending, SubEpWaiting, time.Now().Format(time.RFC3339),
		subID, SubEpSkipped, season, season, episode)
	return err
}

// ListSubscriptionEpisodes 按季集号排好，给单条订阅的详情用。
func (s *Store) ListSubscriptionEpisodes(ctx context.Context, subID int64) ([]SubEpisode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subEpCols+` FROM subscription_episodes
		 WHERE subscription_id = ? ORDER BY season, episode`, subID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubEpisode{}
	for rows.Next() {
		e, err := scanSubEp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListCalendarRange 取 [from, to] 之间播出的集，联订阅表带出片名 / TMDB ID。
// from/to 是 YYYY-MM-DD；AirDate 为空的「未定档」集不在这里，另由
// ListUndatedEpisodes 返回，免得周历里凭空冒出一行没有日期的卡片。
func (s *Store) ListCalendarRange(ctx context.Context, from, to string) ([]SubEpisode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.subscription_id, e.season, e.episode, e.air_date, e.name, e.status,
		        e.try_count, e.next_try_at, e.found_at, e.last_result,
		        s.title, s.original_title, s.tmdb_id, s.state
		 FROM subscription_episodes e
		 JOIN subscriptions s ON s.id = e.subscription_id
		 WHERE e.air_date >= ? AND e.air_date <= ? AND e.status != ?
		 ORDER BY e.air_date, s.title, e.season, e.episode`,
		from, to, SubEpSkipped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubEpisode{}
	for rows.Next() {
		var e SubEpisode
		if err := rows.Scan(&e.ID, &e.SubscriptionID, &e.Season, &e.Episode, &e.AirDate,
			&e.Name, &e.Status, &e.TryCount, &e.NextTryAt, &e.FoundAt, &e.LastResult,
			&e.Title, &e.OriginalTitle, &e.TMDbID, &e.State); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListUndatedEpisodes 返回订阅里 TMDB 没给播出日、且还没处理完的集。
func (s *Store) ListUndatedEpisodes(ctx context.Context) ([]SubEpisode, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.subscription_id, e.season, e.episode, e.air_date, e.name, e.status,
		        e.try_count, e.next_try_at, e.found_at, e.last_result,
		        s.title, s.original_title, s.tmdb_id, s.state
		 FROM subscription_episodes e
		 JOIN subscriptions s ON s.id = e.subscription_id
		 WHERE e.air_date = '' AND e.status NOT IN (?, ?)
		 ORDER BY s.title, e.season, e.episode`, SubEpDone, SubEpSkipped)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SubEpisode{}
	for rows.Next() {
		var e SubEpisode
		if err := rows.Scan(&e.ID, &e.SubscriptionID, &e.Season, &e.Episode, &e.AirDate,
			&e.Name, &e.Status, &e.TryCount, &e.NextTryAt, &e.FoundAt, &e.LastResult,
			&e.Title, &e.OriginalTitle, &e.TMDbID, &e.State); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// DueSubEpisode 是一条「今天该搜」的集，连同它所属订阅的搜索参数。
type DueSubEpisode struct {
	Ep  SubEpisode
	Sub Subscription
}

// DueCalendarEpisodes 取到期需要搜资源的集。
//
// 到期 = 状态还没结束、且（没播出日 或 播出日不晚于今天）、且下次尝试时间已过。
// 暂停 / 完结的订阅不搜。一次最多 limit 条，串行处理，和 115 离线对并发的
// 敏感度保持一致（见 subscribe.Service.tick 的注释）。
func (s *Store) DueCalendarEpisodes(ctx context.Context, now time.Time, limit int) ([]DueSubEpisode, error) {
	if limit <= 0 {
		limit = 10
	}
	today := now.Format("2006-01-02")
	rows, err := s.db.QueryContext(ctx,
		`SELECT e.id, e.subscription_id, e.season, e.episode, e.air_date, e.name, e.status,
		        e.try_count, e.next_try_at, e.found_at, e.last_result
		 FROM subscription_episodes e
		 JOIN subscriptions s ON s.id = e.subscription_id
		 WHERE e.status IN (?, ?)
		   AND (e.air_date = '' OR e.air_date <= ?)
		   AND (e.next_try_at = '' OR e.next_try_at <= ?)
		   AND s.state IN (?, ?)
		 ORDER BY e.air_date, e.subscription_id, e.season, e.episode LIMIT ?`,
		SubEpPending, SubEpWaiting, today, now.Format(time.RFC3339),
		SubWatching, SubWaiting, limit)
	if err != nil {
		return nil, err
	}
	var eps []SubEpisode
	for rows.Next() {
		e, err := scanSubEp(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		eps = append(eps, e)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	// 一轮里同一个订阅常常有多集到期，缓存一下别重复查订阅行。
	cache := map[int64]*Subscription{}
	out := make([]DueSubEpisode, 0, len(eps))
	for _, e := range eps {
		sub, ok := cache[e.SubscriptionID]
		if !ok {
			sub, err = s.GetSubscription(ctx, e.SubscriptionID)
			if err != nil {
				return nil, err
			}
			cache[e.SubscriptionID] = sub
		}
		if sub == nil {
			continue // 订阅刚被删掉，日历行还没来得及清理
		}
		out = append(out, DueSubEpisode{Ep: e, Sub: *sub})
	}
	return out, nil
}

// MarkSubEpisodeDone 搜到并提交离线成功。同时清掉重试排期。
func (s *Store) MarkSubEpisodeDone(ctx context.Context, id int64, result string) error {
	now := time.Now().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription_episodes
		 SET status = ?, found_at = ?, last_result = ?, next_try_at = '', updated_at = ?
		 WHERE id = ? AND status != ?`,
		SubEpDone, now, result, now, id, SubEpSkipped)
	return err
}

// MarkSubEpisodeMissed 这集没搜到：记一次尝试，把下次排到 next。
//
// 「没搜到」不是出错，所以不碰订阅的 fail_count、不走指数退避；调用方传
// 24 小时后的时间，就是用户要的「没搜到每日搜索一次」。
func (s *Store) MarkSubEpisodeMissed(ctx context.Context, id int64, next time.Time, result string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscription_episodes
		 SET status = ?, try_count = try_count + 1, next_try_at = ?,
		     last_result = ?, updated_at = ?
		 WHERE id = ? AND status != ?`,
		SubEpWaiting, next.Format(time.RFC3339), strings.TrimSpace(result),
		time.Now().Format(time.RFC3339), id, SubEpDone)
	return err
}
