package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// LibrarySource 是「这个媒体库从哪个网盘目录取片」。
//
// 一个库可以挂多个来源：115 的电影目录 + 夸克的电影目录，写进同一个本地
// 目录、由同一个 Emby 库扫描。这样「一部分片在 115、一部分在夸克」对用户
// 就是透明的——区别只剩 .strm 里那行 302 地址（115 是 /d/{pickcode}，
// 夸克是 /d/q/{fid}）。
//
// 为什么单独一张表而不是在 libraries 上再加两列：来源是「1..N」，
// 加列只能表达 2 个，而且第三个网盘接进来时又要改表。表结构的成本是一次性的。
type LibrarySource struct {
	ID            int64     `json:"id"`
	LibraryID     int64     `json:"library_id"`
	AccountID     int64     `json:"account_id"`
	AccountKind   string    `json:"account_kind"`
	AccountName   string    `json:"account_name"`
	RemoteCID     string    `json:"remote_cid"`
	RemotePath    string    `json:"remote_path"`
	LastSyncAt    time.Time `json:"last_sync_at"`
	LastSyncFiles int       `json:"last_sync_files"`
}

const srcCols = `s.id, s.library_id, s.account_id, COALESCE(a.kind,''), COALESCE(a.name,''),
	s.remote_cid, s.remote_path, s.last_sync_at, s.last_sync_files`

func scanSource(sc interface{ Scan(...any) error }) (LibrarySource, error) {
	var s LibrarySource
	var lastSync string
	err := sc.Scan(&s.ID, &s.LibraryID, &s.AccountID, &s.AccountKind, &s.AccountName,
		&s.RemoteCID, &s.RemotePath, &lastSync, &s.LastSyncFiles)
	if err != nil {
		return s, err
	}
	s.LastSyncAt, _ = time.Parse(time.RFC3339, lastSync)
	return s, nil
}

// ListSources 返回某个库的全部来源，按添加顺序（顺序决定「同一部片两边都有时留谁」）。
func (s *Store) ListSources(ctx context.Context, libraryID int64) ([]LibrarySource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+srcCols+` FROM library_sources s
		 LEFT JOIN accounts a ON a.id = s.account_id
		 WHERE s.library_id=? ORDER BY s.id`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LibrarySource{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, src)
	}
	return out, rows.Err()
}

// AddSource 挂一个新来源。同一个库重复挂同一账号的同一目录会被唯一约束挡下，
// 这里翻译成一句人话，而不是把 SQLite 的报错原样甩给前端。
func (s *Store) AddSource(ctx context.Context, src LibrarySource) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO library_sources(library_id, account_id, remote_cid, remote_path, created_at)
		 VALUES(?,?,?,?,?)`,
		src.LibraryID, src.AccountID, src.RemoteCID, src.RemotePath, now)
	if err != nil {
		if isUniqueErr(err) {
			return 0, ErrDuplicate
		}
		return 0, err
	}
	return res.LastInsertId()
}

// UpdateSource 改一个来源指向的目录（换目录比删了重建更省事，台账行还能留住）。
func (s *Store) UpdateSource(ctx context.Context, libraryID, sourceID int64, remoteCID, remotePath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE library_sources SET remote_cid=?, remote_path=? WHERE id=? AND library_id=?`,
		remoteCID, remotePath, sourceID, libraryID)
	return err
}

// DeleteSource 摘掉一个来源。允许摘到 0 个：库本身、Emby 侧、已生成的 .strm 都不动，
// 只是下一轮同步不再扫它——「先摘掉，观察一阵再删库」是合理操作。
func (s *Store) DeleteSource(ctx context.Context, libraryID, sourceID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM library_sources WHERE id=? AND library_id=?`, sourceID, libraryID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// MarkSourceSynced 记下「这个来源上一次同步是什么时候、扫出多少条」。
func (s *Store) MarkSourceSynced(ctx context.Context, sourceID int64, at time.Time, files int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE library_sources SET last_sync_at=?, last_sync_files=? WHERE id=?`,
		at.Format(time.RFC3339), files, sourceID)
	return err
}

// SourcesByLibrary 一次拿多个库的来源，供列表接口避免 N+1 查询。
func (s *Store) SourcesByLibrary(ctx context.Context) (map[int64][]LibrarySource, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+srcCols+` FROM library_sources s
		 LEFT JOIN accounts a ON a.id = s.account_id
		 ORDER BY s.library_id, s.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]LibrarySource{}
	for rows.Next() {
		src, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out[src.LibraryID] = append(out[src.LibraryID], src)
	}
	return out, rows.Err()
}

// AccountByKind 取某个网盘类型下的第一个账号。
//
// 播放代理用它兜底：.strm 里写了 q/{fid} 但台账里查不到（比如旧文件、
// 台账被清过），至少还能按「夸克账号」把直链换出来，而不是拿 115 的
// 凭证去问夸克的 fid——那样只会得到一个语焉不详的 502。
func (s *Store) AccountByKind(ctx context.Context, kind string) (int64, bool) {
	var id int64
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE kind=? ORDER BY id LIMIT 1`, kind).Scan(&id)
	return id, err == nil
}

// AccountKind 返回账号类型（115 / quarktv）。
func (s *Store) AccountKind(ctx context.Context, accountID int64) (string, error) {
	var kind string
	err := s.db.QueryRowContext(ctx, `SELECT kind FROM accounts WHERE id=?`, accountID).Scan(&kind)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return kind, err
}

// backfillSources 把「一个库一个账号」的老数据搬进 library_sources。
//
// 每次启动都跑，靠 UNIQUE(library_id, account_id, remote_cid) + INSERT OR IGNORE
// 保证幂等。老库升级后第一轮同步就能直接按「多来源」逻辑走，不需要用户
// 重新建库。
func (s *Store) backfillSources() error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO library_sources
		(library_id, account_id, remote_cid, remote_path, created_at)
		SELECT id, account_id, remote_cid, remote_path, created_at FROM libraries`)
	return err
}
