package store

import (
	"context"
	"strings"
	"time"
)

// SyncRun 是一轮同步的流水账。
//
// 和内存里的 sync.Job 有重叠字段，但职责不同：Job 是「现在跑到哪了」，
// SyncRun 是「这轮最后怎么了」。前端两个都读——进度看 Job，历史看 SyncRun。
type SyncRun struct {
	ID          int64     `json:"id"`
	LibraryID   int64     `json:"library_id"`
	LibraryName string    `json:"library_name"`
	Trigger     string    `json:"trigger"` // full / incremental / auto / manual
	Phase       string    `json:"phase"`   // running / done / error / canceled
	Files       int       `json:"files"`
	Written     int       `json:"written"`
	Removed     int       `json:"removed"`
	Failed      int       `json:"failed"`
	Skipped     int       `json:"skipped"`
	Message     string    `json:"message"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at"`
}

// StartSyncRun 在开跑前插一行，返回行 id。
//
// 调用方拿到 id 后就算进程被人拔电，这行也停在 running——重启后前端看到
// 「有一轮 running 但服务其实已经重启过」会困惑，所以读取时把超期的
// running 行当失败处理（见 ListSyncRuns 里的 staleRunning）。
func (s *Store) StartSyncRun(ctx context.Context, libraryID int64, libraryName, trigger string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO sync_runs(library_id, library_name, trigger, phase, started_at)
		 VALUES(?,?,?,?,?)`,
		libraryID, libraryName, trigger, "running", time.Now().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// FinishSyncRun 补齐一轮的统计与结果。
func (s *Store) FinishSyncRun(ctx context.Context, id int64, phase string,
	files, written, removed, failed, skipped int, message string) error {

	_, err := s.db.ExecContext(ctx,
		`UPDATE sync_runs SET phase=?, files=?, written=?, removed=?, failed=?, skipped=?,
		   message=?, finished_at=? WHERE id=?`,
		phase, files, written, removed, failed, skipped, message,
		time.Now().Format(time.RFC3339), id)
	return err
}

// ListSyncRuns 按时间倒序列最近 limit 轮。
func (s *Store) ListSyncRuns(ctx context.Context, limit int) ([]SyncRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 30
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, library_id, library_name, trigger, phase, files, written, removed,
		        failed, skipped, message, started_at, finished_at
		 FROM sync_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []SyncRun{}
	for rows.Next() {
		var r SyncRun
		var started, finished string
		if err := rows.Scan(&r.ID, &r.LibraryID, &r.LibraryName, &r.Trigger, &r.Phase,
			&r.Files, &r.Written, &r.Removed, &r.Failed, &r.Skipped, &r.Message,
			&started, &finished); err != nil {
			return nil, err
		}
		r.StartedAt = parseRunTime(started)
		r.FinishedAt = parseRunTime(finished)
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastFullSyncAt 返回最近一次成功全量的完成时间（没有则零值）。
func (s *Store) LastFullSyncAt(ctx context.Context) time.Time {
	var v string
	err := s.db.QueryRowContext(ctx,
		`SELECT finished_at FROM sync_runs
		 WHERE trigger='full' AND phase='done' ORDER BY id DESC LIMIT 1`).Scan(&v)
	if err != nil {
		return time.Time{}
	}
	return parseRunTime(v)
}

// PruneSyncRuns 只保留最近 keep 条，避免流水账无限长。
func (s *Store) PruneSyncRuns(ctx context.Context, keep int) error {
	if keep < 50 {
		keep = 50
	}
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM sync_runs WHERE id NOT IN (SELECT id FROM sync_runs ORDER BY id DESC LIMIT ?)`, keep)
	return err
}

// ClearSyncRuns 删掉已经结束的流水账，正在跑的那行保留。
//
// 不做「全清」：tasks 页的清空按钮如果连正在跑的那轮一起删掉，用户会看到
// 「正在同步」突然消失，而同步其实还在写盘 —— 那比不删更让人困惑。
func (s *Store) ClearSyncRuns(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sync_runs WHERE phase <> 'running'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// CountSyncRuns 返回流水账总条数（用于「历史 N 条」这类展示）。
func (s *Store) CountSyncRuns(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_runs`).Scan(&n)
	return n, err
}

func parseRunTime(s string) time.Time {
	if strings.TrimSpace(s) == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t
}

// ==================== 台账反查 / 库配置 ====================

// FileRef 是「这个网盘文件在我们台账里对应哪条 .strm」。
type FileRef struct {
	FileID    string `json:"file_id"`
	RelPath   string `json:"rel_path"`
	LibraryID int64  `json:"library_id"`
}

// FilesByFileIDs 按网盘文件 id 批量反查台账。
//
// 网盘文件页每次列一页都会调一次：几十个 id 走索引，比把整张 files 表
// 拉进内存再比对便宜得多——库大了以后后者就是几百毫秒的卡顿。
func (s *Store) FilesByFileIDs(ctx context.Context, accountID int64, ids []string) (map[string]FileRef, error) {
	out := make(map[string]FileRef, len(ids))
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, accountID)
		for _, id := range batch {
			args = append(args, id)
		}
		q := `SELECT file_id, rel_path, library_id FROM files
		      WHERE account_id=? AND file_id IN (?` + strings.Repeat(",?", len(batch)-1) + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var r FileRef
			if err := rows.Scan(&r.FileID, &r.RelPath, &r.LibraryID); err != nil {
				rows.Close()
				return nil, err
			}
			out[r.FileID] = r
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// SetLibraryKeepExt 改某个库的「直链带扩展名」开关，下一轮同步生效。
//
// 只改这一个字段而不做通用的 UpdateLibrary：它能改的其它字段
// （local_path / remote_cid）都会让已生成的 .strm 与台账对不上，
// 那属于「重建库」的范畴，不该混在一个开关里。
func (s *Store) SetLibraryKeepExt(ctx context.Context, id int64, keep bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE libraries SET keep_ext=? WHERE id=?`, boolInt(keep), id)
	return err
}
