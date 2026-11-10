package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// 订阅状态。四个值不是同一种东西：
//   - watching 是「还会出新集，我在追」
//   - waiting  是「已经追平了，等下一集」
//   - done     是「追完了」（剧完结且追平，或电影已入库）
//   - paused   是「用户手动停的」，轮询器完全不碰
//
// 区分 waiting 和 done 是为了让用户看得出「这剧还在更，只是暂时没新集」和
// 「这剧彻底结束了」。两者都不再产生下载，但前者还会被轮询。
const (
	SubWatching = "watching"
	SubWaiting  = "waiting"
	SubDone     = "done"
	SubPaused   = "paused"
)

// Subscription 是一条订阅。
//
// Watermark 记的是「已经提交成功的最高集」，不是「已经入库的最高集」。
// 理由：提交离线到真正落盘之间有几十分钟到几小时，如果等入库才推水位线，
// 这段时间里每一轮检查都会重复提交同一集。入库状态本来就由台账负责，
// 订阅这边不需要再当第二份真相。
type Subscription struct {
	ID            int64  `json:"id"`
	TMDbID        int    `json:"tmdb_id"`
	IMDbID        string `json:"imdb_id,omitempty"`
	MediaType     string `json:"media_type"` // tv / movie
	Title         string `json:"title"`
	OriginalTitle string `json:"original_title,omitempty"`
	Year          int    `json:"year,omitempty"`
	LibraryID     int64  `json:"library_id"`
	SaveCID       string `json:"save_cid,omitempty"`
	Quality       string `json:"quality"`            // 2160p / 1080p / 720p
	MinSize       int64  `json:"min_size,omitempty"` // GB
	MaxSize       int64  `json:"max_size,omitempty"` // GB，0 = 不限
	State         string `json:"state"`
	// Watermark 是「起播线」：从这一集开始追（含这一集），之前的集不补。
	//
	// 以前这里和水位线是两条线（程序自动推进的水位线 + 用户设的限制线），
	// 界面上两个概念、两处文案，用户经常设错。现在合并成一条：
	// 用户用滑杆改它，程序提交成功后也推进它。专有日历靠每集的 done 标记
	// 防重复下载，不靠这条线，所以推进后不会把刚下的集重新排队。
	// 0/0 = 从头追（没有集被跳过）。
	WatermarkSeason  int       `json:"watermark_season"`
	WatermarkEpisode int       `json:"watermark_episode"`
	TotalEpisodes    int       `json:"total_episodes,omitempty"`
	LastCheckAt      time.Time `json:"last_check_at"`
	NextCheckAt      time.Time `json:"next_check_at"`
	// 不用 omitempty：字段消失和空串在前端是两条渲染路径（undefined / 空白），
	// 统一成空串，前端只需判断非空。
	LastResult string    `json:"last_result"`
	FailCount  int       `json:"fail_count"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// HasWatermark 表示这条订阅已经有过成功提交。
func (s Subscription) HasWatermark() bool {
	return s.WatermarkEpisode > 0
}

// Progress 返回给人看的进度串：S02E05 / 已入库 / 等待新集 …
func (s Subscription) Progress() string {
	switch {
	case s.MediaType == "movie":
		if s.State == SubDone {
			return "已入库"
		}
		return "等待资源"
	case s.WatermarkEpisode > 0:
		return fmt.Sprintf("S%02dE%02d", s.WatermarkSeason, s.WatermarkEpisode)
	case s.TotalEpisodes > 0:
		return fmt.Sprintf("0 / %d 集", s.TotalEpisodes)
	default:
		return "还没开始"
	}
}

const subCols = `id, tmdb_id, imdb_id, media_type, title, original_title, year,
	library_id, save_cid, quality, min_size, max_size, state,
	watermark_season, watermark_episode, total_episodes,
	last_check_at, next_check_at, last_result, fail_count, created_at, updated_at`

func scanSub(sc interface{ Scan(...any) error }) (Subscription, error) {
	var s Subscription
	var lastCheck, nextCheck, created, updated string
	err := sc.Scan(&s.ID, &s.TMDbID, &s.IMDbID, &s.MediaType, &s.Title, &s.OriginalTitle,
		&s.Year, &s.LibraryID, &s.SaveCID, &s.Quality, &s.MinSize, &s.MaxSize, &s.State,
		&s.WatermarkSeason, &s.WatermarkEpisode, &s.TotalEpisodes,
		&lastCheck, &nextCheck, &s.LastResult, &s.FailCount, &created, &updated)
	if err != nil {
		return s, err
	}
	s.LastCheckAt = parseRunTime(lastCheck)
	s.NextCheckAt = parseRunTime(nextCheck)
	s.CreatedAt = parseRunTime(created)
	s.UpdatedAt = parseRunTime(updated)
	return s, nil
}

// ListSubscriptions 按状态筛选；state 留空表示全部（含 paused）。
func (s *Store) ListSubscriptions(ctx context.Context, state string) ([]Subscription, error) {
	q := `SELECT ` + subCols + ` FROM subscriptions`
	args := []any{}
	if st := strings.TrimSpace(state); st != "" && st != "all" {
		q += ` WHERE state = ?`
		args = append(args, st)
	}
	q += ` ORDER BY updated_at DESC, id DESC`

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// GetSubscription 按 id 取一条；不存在返回 (nil, nil)。
func (s *Store) GetSubscription(ctx context.Context, id int64) (*Subscription, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+subCols+` FROM subscriptions WHERE id = ?`, id)
	sub, err := scanSub(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// ErrSubExists 表示同一部作品已经订阅过。调用方据此回 409 而不是 500。
var ErrSubExists = errors.New("这部作品已经在订阅里了")

// CreateSubscription 新增一条订阅。
//
// (media_type, tmdb_id) 唯一：同一部剧重复订阅会让两个任务抢同一个水位线，
// 结果就是同一集被提交两次。这里把 SQLite 的唯一约束错误翻译成人话。
func (s *Store) CreateSubscription(ctx context.Context, sub Subscription) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	if sub.MediaType == "" {
		sub.MediaType = "tv"
	}
	if sub.State == "" {
		sub.State = SubWatching
	}
	if sub.Quality == "" {
		sub.Quality = "1080p"
	}
	// 新订阅立刻到期一次：用户刚点完「订阅」，不该等一轮 ticker 才去查。
	next := sub.NextCheckAt
	if next.IsZero() {
		next = time.Now()
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions(tmdb_id, imdb_id, media_type, title, original_title, year,
		   library_id, save_cid, quality, min_size, max_size, state,
		   watermark_season, watermark_episode, total_episodes,
		   last_check_at, next_check_at, last_result, fail_count, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'',?,'',0,?,?)`,
		sub.TMDbID, sub.IMDbID, sub.MediaType, sub.Title, sub.OriginalTitle, sub.Year,
		sub.LibraryID, sub.SaveCID, sub.Quality, sub.MinSize, sub.MaxSize, sub.State,
		sub.WatermarkSeason, sub.WatermarkEpisode, sub.TotalEpisodes,
		next.Format(time.RFC3339), now, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, ErrSubExists
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSubscription 只改用户能改的字段（画质、体积、库、目标目录、状态）。
//
// 水位线和 last_result 故意不在这里：它们是轮询器的产物，能被前端随手改掉
// 就等于把「已经下到第几集」交给表单，改错一次就是重复下载好几十集。
func (s *Store) UpdateSubscription(ctx context.Context, id int64, quality string,
	minSize, maxSize int64, libraryID int64, saveCID, state string) error {

	sets := []string{"updated_at = ?"}
	args := []any{time.Now().Format(time.RFC3339)}

	if q := strings.TrimSpace(quality); q != "" {
		sets = append(sets, "quality = ?")
		args = append(args, q)
	}
	if minSize >= 0 {
		sets = append(sets, "min_size = ?")
		args = append(args, minSize)
	}
	if maxSize >= 0 {
		sets = append(sets, "max_size = ?")
		args = append(args, maxSize)
	}
	if libraryID > 0 {
		sets = append(sets, "library_id = ?")
		args = append(args, libraryID)
	}
	if saveCID != "" {
		sets = append(sets, "save_cid = ?")
		args = append(args, saveCID)
	}
	if st := strings.TrimSpace(state); st != "" {
		sets = append(sets, "state = ?")
		args = append(args, st)
		// 从暂停恢复时立刻到期，否则用户点完「继续」还要等下一个周期。
		if st == SubWatching || st == SubWaiting {
			sets = append(sets, "next_check_at = ?")
			args = append(args, time.Now().Format(time.RFC3339))
			sets = append(sets, "fail_count = 0")
		}
	}
	args = append(args, id)

	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	return err
}

// DeleteSubscription 删掉一条订阅。已经提交出去的离线任务不受影响。
func (s *Store) DeleteSubscription(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DueSubscriptions 取「到期且需要检查」的订阅，供轮询器消费。
//
// paused 排除在外：暂停的意思是「别再看这部剧了」，不是「降低频率」。
// done 也排除：已经追完的再查也不会产生下载，只是白打 TMDB 的接口。
func (s *Store) DueSubscriptions(ctx context.Context, now time.Time, limit int) ([]Subscription, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+subCols+` FROM subscriptions
		 WHERE state IN (?, ?) AND (next_check_at = '' OR next_check_at <= ?)
		 ORDER BY next_check_at, id LIMIT ?`,
		SubWatching, SubWaiting, now.Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []Subscription{}
	for rows.Next() {
		sub, err := scanSub(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// MarkSubscriptionChecked 写回一轮检查的结果：下次什么时候再查、状态、一句话结论。
//
// 这三个字段总是一起变，所以合成一个方法 —— 分三次 UPDATE 会出现「状态改了
// 但下次检查时间没改」的中间态，而轮询器是每分钟扫一次的，完全可能读到它。
func (s *Store) MarkSubscriptionChecked(ctx context.Context, id int64,
	next time.Time, state, result string, failCount int) error {

	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET next_check_at = ?, state = ?, last_result = ?,
		   fail_count = ?, last_check_at = ?, updated_at = ? WHERE id = ?`,
		next.Format(time.RFC3339), state, result, failCount,
		time.Now().Format(time.RFC3339), time.Now().Format(time.RFC3339), id)
	return err
}

// FindSubscriptionByTMDB 按 (类型, tmdb_id) 反查订阅，没订过返回 (nil, nil)。
//
// 详情抽屉要回答「这部剧我订了没有、追到第几集」，而入口页面手里只有 tmdb_id。
// 表上本来就有 UNIQUE(media_type, tmdb_id)，这里走的是同一个唯一键。
func (s *Store) FindSubscriptionByTMDB(ctx context.Context, mediaType string, tmdbID int) (*Subscription, error) {
	if tmdbID <= 0 {
		return nil, nil
	}
	t := strings.TrimSpace(mediaType)
	if t != "movie" {
		t = "tv"
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+subCols+` FROM subscriptions WHERE media_type = ? AND tmdb_id = ? LIMIT 1`, t, tmdbID)
	sub, err := scanSub(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &sub, nil
}

// ForceSubscriptionWatermark 把水位线直接设成指定值，可前进也可后退。
//
// 和 SetSubscriptionWatermark 的区别是「谁在决定」：
//   - SetSubscriptionWatermark 是程序提交成功后的自动推进，只许前进；
//   - 这个函数是用户在本子里手改，意思是「前面那些集我不补了」。
//
// 允许后退是必须的：补不到的老集会把水位线永久卡住（S01E03 搜不到资源，
// 后面十几集就永远轮不到），用户唯一能自救的办法就是把它挪到最新一集。
// 后退之后那几集会被重新搜索、重新提交，这是用户的明确选择，不做拦截。
func (s *Store) ForceSubscriptionWatermark(ctx context.Context, id int64, season, episode int) error {
	now := time.Now().Format(time.RFC3339)
	// next_check_at 一起推到「现在」：用户改完就去倒水了，不该再等一个周期。
	// 轮询器下一轮 tick 会立刻取到它，行为和点「立即检查」接近但不阻塞请求。
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		 SET watermark_season = ?, watermark_episode = ?, next_check_at = ?, updated_at = ?
		 WHERE id = ?`,
		season, episode, now, now, id)
	return err
}

// SetSubscriptionWatermark 推进「已提交到第几集」。
//
// 只允许往前走：并发下（手动点「立即检查」正好撞上轮询）两次提交可能乱序回来，
// 后写的旧值会把水位线拉回去，那一集就会被重复下载。SQL 里加条件比调用方
// 加锁省事，也挡住了以后新增调用点忘记判断的情况。
func (s *Store) SetSubscriptionWatermark(ctx context.Context, id int64, season, episode int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions
		 SET watermark_season = ?, watermark_episode = ?, updated_at = ?
		 WHERE id = ?
		   AND (watermark_season < ? OR (watermark_season = ? AND watermark_episode < ?))`,
		season, episode, time.Now().Format(time.RFC3339), id,
		season, season, episode)
	return err
}

// SetSubscriptionTotal 记录 TMDB 报的总集数，用于前端显示「3 / 10 集」。
func (s *Store) SetSubscriptionTotal(ctx context.Context, id int64, total int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE subscriptions SET total_episodes = ? WHERE id = ?`, total, id)
	return err
}

// CountSubscriptions 返回各状态的条数，用于页面顶部的分类角标。
func (s *Store) CountSubscriptions(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM subscriptions GROUP BY state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := map[string]int{SubWatching: 0, SubWaiting: 0, SubDone: 0, SubPaused: 0}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
