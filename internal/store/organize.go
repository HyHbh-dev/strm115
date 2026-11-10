package store

import (
	"context"
	"strings"
	"time"
)

// OrganizeRecord 是自动整理的一条台账：一个网盘条目一行。
//
// 和 files 台账的分工：files 记「媒体库里有什么」（同步的产物），
// organize_records 记「我们把它从哪儿搬到哪儿、为什么搬、搬成功没有」。
// 后者要能回答「上周这个片怎么跑进剧集库了」这类问题，所以识别依据
// （reason / confidence）和识别结果（title / year / season）都得原样留下，
// 不能只存一个目标路径。
type OrganizeRecord struct {
	ID         int64     `json:"id"`
	AccountID  int64     `json:"account_id"`
	FileID     string    `json:"file_id"`
	Name       string    `json:"name"`
	IsDir      bool      `json:"is_dir"`
	Size       int64     `json:"size"`
	Kind       string    `json:"kind"` // movie / series / unknown
	Title      string    `json:"title"`
	Year       string    `json:"year"`
	Season     int       `json:"season"`
	Episode    int       `json:"episode"`
	Confidence int       `json:"confidence"`
	Reason     string    `json:"reason"`
	Target     string    `json:"target"`
	Status     string    `json:"status"` // moved / failed / skipped / ignored
	Message    string    `json:"message"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// 整理台账的状态取值。放在 store 里是因为「哪些状态算已处理」这件事
// 由读取方（扫描时过滤）和写入方（执行后落库）共同约定，两边不能各自写字符串。
const (
	OrganizeMoved   = "moved"   // 已移到目标目录
	OrganizeFailed  = "failed"  // 移动失败（115 报错 / 风控）
	OrganizeSkipped = "skipped" // 跳过：认不出、太小、非视频
	OrganizeIgnored = "ignored" // 用户点了忽略
)

// OrganizeDone 判断某个状态是否表示「不用再管了」。
// 扫描时按它把已处理和已忽略的条目过滤掉——否则每扫一次都要重新看一遍
// 已经整理好的片子，几千个条目滚不动。
func OrganizeDone(status string) bool {
	return status == OrganizeMoved || status == OrganizeIgnored
}

// UpsertOrganizeRecord 按 (account_id, file_id) 覆盖写一条。
//
// 用覆盖而不是追加：同一个文件被重试三次不该在界面上出现三行。
// created_at 只在首次插入时写，重试只改 updated_at——「这个文件第一次
// 是什么时候被整理的」比「最后一次」更难恢复。
func (s *Store) UpsertOrganizeRecord(ctx context.Context, r OrganizeRecord) error {
	now := time.Now().Format(time.RFC3339)
	created := r.CreatedAt.Format(time.RFC3339)
	if r.CreatedAt.IsZero() {
		created = now
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO organize_records(
		   account_id, file_id, name, is_dir, size, kind, title, year, season, episode,
		   confidence, reason, target, status, message, created_at, updated_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(account_id, file_id) DO UPDATE SET
		   name=excluded.name, is_dir=excluded.is_dir, size=excluded.size,
		   kind=excluded.kind, title=excluded.title, year=excluded.year,
		   season=excluded.season, episode=excluded.episode,
		   confidence=excluded.confidence, reason=excluded.reason,
		   target=excluded.target, status=excluded.status, message=excluded.message,
		   updated_at=excluded.updated_at`,
		r.AccountID, r.FileID, r.Name, boolInt(r.IsDir), r.Size, r.Kind, r.Title, r.Year,
		r.Season, r.Episode, r.Confidence, r.Reason, r.Target, r.Status, r.Message,
		created, now)
	return err
}

// ListOrganizeRecords 按 id 倒序分页。status 为空表示全部。
func (s *Store) ListOrganizeRecords(ctx context.Context, status string, limit, offset int) ([]OrganizeRecord, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if offset < 0 {
		offset = 0
	}
	where, args := "", []any{}
	if st := strings.TrimSpace(status); st != "" {
		where = " WHERE status=?"
		args = append(args, st)
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM organize_records`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT id, account_id, file_id, name, is_dir, size, kind, title, year, season, episode,
	             confidence, reason, target, status, message, created_at, updated_at
	      FROM organize_records` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []OrganizeRecord{}
	for rows.Next() {
		var r OrganizeRecord
		var isDir int
		var created, updated string
		if err := rows.Scan(&r.ID, &r.AccountID, &r.FileID, &r.Name, &isDir, &r.Size,
			&r.Kind, &r.Title, &r.Year, &r.Season, &r.Episode, &r.Confidence, &r.Reason,
			&r.Target, &r.Status, &r.Message, &created, &updated); err != nil {
			return nil, 0, err
		}
		r.IsDir = isDir == 1
		r.CreatedAt = parseRunTime(created)
		r.UpdatedAt = parseRunTime(updated)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// OrganizeRecord 取单条，重试用。
func (s *Store) OrganizeRecord(ctx context.Context, id int64) (OrganizeRecord, error) {
	var r OrganizeRecord
	var isDir int
	var created, updated string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, account_id, file_id, name, is_dir, size, kind, title, year, season, episode,
		        confidence, reason, target, status, message, created_at, updated_at
		 FROM organize_records WHERE id=?`, id).
		Scan(&r.ID, &r.AccountID, &r.FileID, &r.Name, &isDir, &r.Size, &r.Kind, &r.Title,
			&r.Year, &r.Season, &r.Episode, &r.Confidence, &r.Reason, &r.Target,
			&r.Status, &r.Message, &created, &updated)
	if err != nil {
		return OrganizeRecord{}, err
	}
	r.IsDir = isDir == 1
	r.CreatedAt = parseRunTime(created)
	r.UpdatedAt = parseRunTime(updated)
	return r, nil
}

// OrganizeStatuses 返回「文件 id → 台账状态」，扫描时用来跳过已处理的。
//
// 只回 moved / ignored 两种：failed 和 skipped 需要每次都重新出现在扫描
// 结果里，否则失败一次就再也没机会被重试。
func (s *Store) OrganizeStatuses(ctx context.Context, accountID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT file_id, status FROM organize_records
		 WHERE account_id=? AND status IN (?,?)`,
		accountID, OrganizeMoved, OrganizeIgnored)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, st string
		if err := rows.Scan(&id, &st); err != nil {
			return nil, err
		}
		out[id] = st
	}
	return out, rows.Err()
}

// SetOrganizeStatus 只改状态与说明（忽略 / 重试后回写用）。
func (s *Store) SetOrganizeStatus(ctx context.Context, id int64, status, message, target string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE organize_records SET status=?, message=?, target=?, updated_at=? WHERE id=?`,
		status, message, target, time.Now().Format(time.RFC3339), id)
	return err
}

// OrganizeStats 返回各状态计数。界面上「待处理 / 已整理 / 失败」三个数字靠它。
func (s *Store) OrganizeStats(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM organize_records GROUP BY status`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
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

// ClearOrganizeRecords 删台账。status 为空表示全清。
//
// 注意它删的是「整理记录」，不是网盘里的文件——界面上必须把这句话写出来，
// 否则「清空记录」很容易被当成「清空整理过的文件」。
func (s *Store) ClearOrganizeRecords(ctx context.Context, status string) (int, error) {
	var res interface {
		RowsAffected() (int64, error)
	}
	var err error
	if st := strings.TrimSpace(status); st != "" {
		res, err = s.db.ExecContext(ctx, `DELETE FROM organize_records WHERE status=?`, st)
	} else {
		res, err = s.db.ExecContext(ctx, `DELETE FROM organize_records`)
	}
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
