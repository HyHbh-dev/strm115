package store

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"
)

// Media 是一条刮削结果：视频文件 + 匹配到的影视信息 + 本地附属文件位置。
//
// RelPath 与 files.rel_path 同义（相对媒体库根的斜杠路径），但两者不共享生命周期：
// 网盘改名后 rel_path 会变，Media 这边由刮削重新认领，旧行按不再出现来清理。
type Media struct {
	ID        int64  `json:"id"`
	LibraryID int64  `json:"library_id"`
	RelPath   string `json:"rel_path"`
	Kind      string `json:"kind"` // movie / series
	Title     string `json:"title"`
	Year      string `json:"year"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	IMDbID    string `json:"imdb_id"`
	// TMDbID 是刮削命中的 TMDB 条目 ID。海报墙按它直查 TMDB 详情；
	// IMDbID 可能为空（TMDB 有些条目没登记 external_ids），这时它才是主键。
	TMDbID       int    `json:"tmdb_id"`
	MetaTitle    string `json:"meta_title"`
	MetaYear     string `json:"meta_year"`
	Rating       string `json:"rating"`
	Runtime      string `json:"runtime"`
	Genres       string `json:"genres"`
	Cast         string `json:"cast"`
	Director     string `json:"director"`
	Overview     string `json:"overview"`
	PosterURL    string `json:"poster_url"`
	BackdropURL  string `json:"backdrop_url"`
	PosterFile   string `json:"poster_file"`
	BackdropFile string `json:"backdrop_file"`
	NFOFile      string `json:"nfo_file"`
	SubFile      string `json:"sub_file"`
	SubLang      string `json:"sub_lang"`
	SubState     string `json:"sub_state"`
	// SubOrigin 记这份中文字幕是怎么来的：
	//   zh         = 从字幕站下载的中文
	//   translated = 下载英文字幕再 AI 翻译
	// 以前只有 sub_lang="zh" 一个标记，机翻和官方中文在界面上一模一样，
	// 用户看到满屏机翻才知道不对，却查不到是哪一步给的。
	SubOrigin string    `json:"sub_origin"`
	Status    string    `json:"status"` // ok / unmatched / error
	Message   string    `json:"message"`
	UpdatedAt time.Time `json:"updated_at"`
}

const mediaCols = `id, library_id, rel_path, kind, title, year, season, episode,
	imdb_id, tmdb_id, meta_title, meta_year, rating, runtime, genres, actors, director, overview,
	poster_url, backdrop_url, poster_file, backdrop_file, nfo_file, sub_file, sub_lang, sub_state,
	sub_origin, status, message, updated_at`

func scanMedia(sc interface{ Scan(...any) error }) (Media, error) {
	var m Media
	var updated string
	err := sc.Scan(&m.ID, &m.LibraryID, &m.RelPath, &m.Kind, &m.Title, &m.Year,
		&m.Season, &m.Episode, &m.IMDbID, &m.TMDbID, &m.MetaTitle, &m.MetaYear, &m.Rating,
		&m.Runtime, &m.Genres, &m.Cast, &m.Director, &m.Overview,
		&m.PosterURL, &m.BackdropURL,
		&m.PosterFile, &m.BackdropFile, &m.NFOFile, &m.SubFile, &m.SubLang,
		&m.SubState, &m.SubOrigin, &m.Status, &m.Message, &updated)
	if err != nil {
		return m, err
	}
	m.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return m, nil
}

// UpsertMedia 写入或更新一条刮削结果。
//
// 保留原来的 poster_file / sub_file 这类「已经落盘的东西」不太好做，
// 所以调用方要传完整对象：读一次、改字段、写回，是这个模块的固定用法。
func (s *Store) UpsertMedia(ctx context.Context, m Media) error {
	now := m.UpdatedAt
	if now.IsZero() {
		now = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO media_meta(
			library_id, rel_path, kind, title, year, season, episode,
			imdb_id, tmdb_id, meta_title, meta_year, rating, runtime, genres, actors, director, overview,
			poster_url, backdrop_url, poster_file, backdrop_file, nfo_file, sub_file, sub_lang, sub_state,
			sub_origin, status, message, updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(library_id, rel_path) DO UPDATE SET
			kind=excluded.kind, title=excluded.title, year=excluded.year,
			season=excluded.season, episode=excluded.episode,
			imdb_id=excluded.imdb_id, tmdb_id=excluded.tmdb_id,
			meta_title=excluded.meta_title,
			meta_year=excluded.meta_year, rating=excluded.rating,
			runtime=excluded.runtime, genres=excluded.genres, actors=excluded.actors,
			director=excluded.director, overview=excluded.overview,
			poster_url=excluded.poster_url, backdrop_url=excluded.backdrop_url,
			poster_file=excluded.poster_file, backdrop_file=excluded.backdrop_file,
			nfo_file=excluded.nfo_file, sub_file=excluded.sub_file,
			sub_lang=excluded.sub_lang, sub_state=excluded.sub_state,
			sub_origin=excluded.sub_origin,
			status=excluded.status, message=excluded.message,
			updated_at=excluded.updated_at`,
		m.LibraryID, m.RelPath, m.Kind, m.Title, m.Year, m.Season, m.Episode,
		m.IMDbID, m.TMDbID, m.MetaTitle, m.MetaYear, m.Rating, m.Runtime, m.Genres,
		m.Cast, m.Director, m.Overview, m.PosterURL, m.BackdropURL,
		m.PosterFile, m.BackdropFile, m.NFOFile,
		m.SubFile, m.SubLang, m.SubState, m.SubOrigin, m.Status, m.Message,
		now.Format(time.RFC3339))
	return err
}

// GetMedia 按库 + 相对路径取一条（没有则 found=false）。
func (s *Store) GetMedia(ctx context.Context, libraryID int64, relPath string) (Media, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+mediaCols+` FROM media_meta WHERE library_id=? AND rel_path=?`,
		libraryID, relPath)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, false, nil
	}
	if err != nil {
		return Media{}, false, err
	}
	return m, true, nil
}

// ListMedia 分页列刮削结果，status 为空表示不筛选。
func (s *Store) ListMedia(ctx context.Context, libraryID int64, status string, limit, offset int) ([]Media, int, error) {
	if limit <= 0 {
		limit = 100
	}
	where := `library_id=?`
	args := []any{libraryID}
	if status != "" {
		where += ` AND status=?`
		args = append(args, status)
	}

	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM media_meta WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT ` + mediaCols + ` FROM media_meta WHERE ` + where + ` ORDER BY rel_path LIMIT ? OFFSET ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

// ListMediaAll 拉全量（刮削/补字幕时用，按 rel_path 稳定排序）。
func (s *Store) ListMediaAll(ctx context.Context, libraryID int64) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mediaCols+` FROM media_meta WHERE library_id=? ORDER BY rel_path`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// KnownIMDbIDs 返回这批 IMDb ID 里，哪些已经刮削入库过。
//
// 电影墙用它给卡片打「已入库」标记：用户扫一眼就知道哪些片子在网盘里已经有，
// 不用点进去挨个确认。查的是刮削台账而不是文件表 —— 台账里才有 imdb_id。
func (s *Store) KnownIMDbIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	out := make(map[string]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const chunk = 400
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		args := make([]any, 0, len(batch))
		for _, id := range batch {
			args = append(args, id)
		}
		q := `SELECT DISTINCT imdb_id FROM media_meta WHERE imdb_id IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if id != "" {
				out[id] = true
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// MediaStats 返回总数与各状态的条数，给概览页用。
func (s *Store) MediaStats(ctx context.Context, libraryID int64) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT status, COUNT(*) FROM media_meta WHERE library_id=? GROUP BY status`, libraryID)
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

// MediaRelPaths 返回该库已知的所有 rel_path，供刮削比对「哪些还在」。
func (s *Store) MediaRelPaths(ctx context.Context, libraryID int64) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rel_path FROM media_meta WHERE library_id=?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out[p] = struct{}{}
	}
	return out, rows.Err()
}

// DeleteMediaNotIn 删掉本次没再出现的条目（视频从网盘删了/改名了）。
func (s *Store) DeleteMediaNotIn(ctx context.Context, libraryID int64, keep map[string]struct{}) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, rel_path FROM media_meta WHERE library_id=?`, libraryID)
	if err != nil {
		return 0, err
	}
	var stale []int64
	for rows.Next() {
		var id int64
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			rows.Close()
			return 0, err
		}
		if _, ok := keep[p]; !ok {
			stale = append(stale, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, id := range stale {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM media_meta WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

// ClearMedia 清空一个库的刮削结果（重新刮之前用）。
func (s *Store) ClearMedia(ctx context.Context, libraryID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM media_meta WHERE library_id=?`, libraryID)
	return err
}

// ==================== IMDb 元数据缓存 ====================

// MetaCacheKey 归一化缓存键：类型 + 片名 + 年份。
//
// 刻意不含 rel_path：同一部剧的所有集共享一条缓存，这是「减少 IMDb 请求」
// 最有效的一招，比调低并发管用得多。
func MetaCacheKey(kind, title, year string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(title), " "))
	return kind + "|" + norm + "|" + year
}

// GetMetaCache 取一条缓存；不存在时 found=false。
func (s *Store) GetMetaCache(ctx context.Context, key string) (imdbID, payload string, found bool, err error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT imdb_id, payload FROM meta_cache WHERE cache_key=?`, key)
	err = row.Scan(&imdbID, &payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return imdbID, payload, true, nil
}

// PutMetaCache 写入缓存。payload 是完整元数据的 JSON，将来加字段不用改表。
func (s *Store) PutMetaCache(ctx context.Context, key, imdbID, payload string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO meta_cache(cache_key, imdb_id, payload, updated_at) VALUES(?,?,?,?)
		ON CONFLICT(cache_key) DO UPDATE SET
			imdb_id=excluded.imdb_id, payload=excluded.payload, updated_at=excluded.updated_at`,
		key, imdbID, payload, time.Now().Format(time.RFC3339))
	return err
}

// DeleteMetaCache 清空元数据缓存（用户手动「重新刮」时用）。
func (s *Store) DeleteMetaCache(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM meta_cache`)
	return err
}

// CountMetaCache 返回缓存条数。
func (s *Store) CountMetaCache(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM meta_cache`).Scan(&n)
	return n, err
}

// ==================== 翻译缓存 ====================

// GetTranslations 批量取译文，返回「原文哈希 → 译文」。
func (s *Store) GetTranslations(ctx context.Context, keys []string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	// SQLite 的变量上限是 999，分批查。
	for start := 0; start < len(keys); start += 500 {
		end := start + 500
		if end > len(keys) {
			end = len(keys)
		}
		chunk := keys[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		args := make([]any, len(chunk))
		for i, k := range chunk {
			args[i] = k
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT key, value FROM translation_cache WHERE key IN (`+ph+`)`, args...)
		if err != nil {
			return out, err
		}
		for rows.Next() {
			var k, v string
			if err := rows.Scan(&k, &v); err != nil {
				rows.Close()
				return out, err
			}
			out[k] = v
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return out, err
		}
	}
	return out, nil
}

// PutTranslations 批量写译文（一个事务）。
func (s *Store) PutTranslations(ctx context.Context, entries map[string]string) error {
	if len(entries) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().Format(time.RFC3339)
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO translation_cache(key, value, created_at) VALUES(?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, v := range entries {
		if _, err := stmt.ExecContext(ctx, k, v, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListFilesAll 拉一个库的全部视频台账（刮削用，不分页）。
//
// 刮削是「一次性把整个库过一遍」的操作，分页反而要写循环；
// 十万条以内一次性读进内存是几十 MB 的事，比反复查询简单。
func (s *Store) ListFilesAll(ctx context.Context, libraryID int64) ([]File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, library_id, file_id, pick_code, rel_path, name, size
		 FROM files WHERE library_id=? ORDER BY rel_path`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.LibraryID, &f.FileID, &f.PickCode, &f.RelPath, &f.Name, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetMediaByID 按主键取一条刮削结果。
func (s *Store) GetMediaByID(ctx context.Context, id int64) (Media, bool, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+mediaCols+` FROM media_meta WHERE id=?`, id)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, false, nil
	}
	if err != nil {
		return Media{}, false, err
	}
	return m, true, nil
}

// FindMediaByTMDBID 按 TMDB id 反查本地台账。
//
// 详情抽屉有两个入口：海报墙（有本地行）和订阅列表 / 播出日历（只有 tmdb_id）。
// 后者点开一部剧时得知道「我到底下没下、下到哪一集」，只能靠 tmdb_id 反查。
//
// kind 接受 movie / series，空字符串表示不限。剧集在台账里按**分集**存，
// 同一个 tmdb_id 会有几十上百行，这里只回更新最近的那一行当代表 ——
// 调用方要集数明细就再按 rel_path 前缀查，不要指望这一个函数给全集。
func (s *Store) FindMediaByTMDBID(ctx context.Context, kind string, tmdbID int) (Media, bool, error) {
	if tmdbID <= 0 {
		return Media{}, false, nil
	}
	q := `SELECT ` + mediaCols + ` FROM media_meta WHERE tmdb_id = ?`
	args := []any{tmdbID}
	if k := strings.TrimSpace(kind); k != "" {
		q += ` AND kind = ?`
		args = append(args, k)
	}
	// 剧集按 (季, 集) 倒序取「最新那一集」，比按 updated_at 稳：
	// 整季重刮会让所有行的时间戳一样，updated_at 排不出先后。
	q += ` ORDER BY season DESC, episode DESC, id DESC LIMIT 1`

	row := s.db.QueryRowContext(ctx, q, args...)
	m, err := scanMedia(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Media{}, false, nil
	}
	if err != nil {
		return Media{}, false, err
	}
	return m, true, nil
}

// CountMediaByTMDBID 数一部剧在台账里落了多少集。
//
// 详情抽屉要显示「已入库 8 / 共 20 集」，集总数来自 TMDB，已入库只能数台账。
// 分集行的 tmdb_id 都指向剧本身（刮削时写的是剧集 ID），所以直接 count。
func (s *Store) CountMediaByTMDBID(ctx context.Context, kind string, tmdbID int) (int, error) {
	if tmdbID <= 0 {
		return 0, nil
	}
	q := `SELECT COUNT(*) FROM media_meta WHERE tmdb_id = ?`
	args := []any{tmdbID}
	if k := strings.TrimSpace(kind); k != "" {
		q += ` AND kind = ?`
		args = append(args, k)
	}
	var n int
	err := s.db.QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// ListMediaForSubtitle 只挑还需要补字幕的条目：
// 有 IMDb ID、刮削成功、且还没有中文字幕。
func (s *Store) ListMediaForSubtitle(ctx context.Context, libraryID int64, redo bool) ([]Media, error) {
	q := `SELECT ` + mediaCols + ` FROM media_meta
		WHERE library_id=? AND status='ok' AND imdb_id<>''`
	if !redo {
		q += ` AND (sub_file='' OR sub_state='failed')`
	}
	q += ` ORDER BY rel_path`
	rows, err := s.db.QueryContext(ctx, q, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Media
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ListMediaByRelPaths 一次取回这批路径的刮削记录，返回 rel_path → Media。
//
// 海报墙一页 30~60 张卡片，逐条 GetMedia 就是几十次查询；这里合成一条
// IN 查询，同时把结果直接做成 map，调用方不必再自己建索引。
func (s *Store) ListMediaByRelPaths(ctx context.Context, libraryID int64, rels []string) (map[string]Media, error) {
	out := make(map[string]Media, len(rels))
	// SQLite 默认最多 999 个绑定变量，分批才不会在大库上炸。
	const chunk = 400
	for i := 0; i < len(rels); i += chunk {
		end := i + chunk
		if end > len(rels) {
			end = len(rels)
		}
		batch := rels[i:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, libraryID)
		for _, r := range batch {
			args = append(args, r)
		}
		q := `SELECT ` + mediaCols + ` FROM media_meta WHERE library_id=? AND rel_path IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		rows, err := s.db.QueryContext(ctx, q, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			m, err := scanMedia(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			out[m.RelPath] = m
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// DeleteMediaByRelPaths 删一批刮削记录（删条目时同步清理）。
func (s *Store) DeleteMediaByRelPaths(ctx context.Context, libraryID int64, rels []string) error {
	const chunk = 400
	for i := 0; i < len(rels); i += chunk {
		end := i + chunk
		if end > len(rels) {
			end = len(rels)
		}
		batch := rels[i:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, libraryID)
		for _, r := range batch {
			args = append(args, r)
		}
		q := `DELETE FROM media_meta WHERE library_id=? AND rel_path IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// DeleteMediaByPrefix 按目录前缀删刮削记录（删整部剧时用）。
func (s *Store) DeleteMediaByPrefix(ctx context.Context, libraryID int64, prefix string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM media_meta WHERE library_id=? AND (rel_path=? OR rel_path LIKE ? ESCAPE '\')`,
		libraryID, prefix, escapeLike(prefix)+"/%")
	return err
}

// escapeLike 把 LIKE 通配符转成字面量；网盘目录名里带 % 或 _ 并不罕见。
func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

// MediaGenres 返回库里所有刮削记录出现过的类型（去重、按名称排序）。
// 海报墙的类型筛选条用它，只读 genres 一列，不必把整张表拉进内存。
func (s *Store) MediaGenres(ctx context.Context, libraryID int64) ([]string, error) {
	q := `SELECT genres FROM media_meta WHERE library_id=? AND genres<>''`
	args := []any{libraryID}
	if libraryID == 0 {
		q = `SELECT genres FROM media_meta WHERE genres<>''`
		args = nil
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[string]struct{}{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		for _, g := range strings.Split(raw, ",") {
			if g = strings.TrimSpace(g); g != "" {
				seen[g] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for g := range seen {
		out = append(out, g)
	}
	sort.Strings(out)
	return out, nil
}

// MediaLibraryStat 是一个库的刮削统计（一条 SQL 聚合出来）。
type MediaLibraryStat struct {
	LibraryID int64
	Total     int
	OK        int
	Unmatched int
	Failed    int
	Subtitled int
}

// MediaStatsByLibrary 一次拿到所有库的刮削统计。
//
// 总览面板要同时显示十几个库的「已刮削 / 未匹配 / 有字幕」，逐库调
// MediaStats 就是十几次查询；GROUP BY 一次扫完，库多了也只有一条 SQL。
func (s *Store) MediaStatsByLibrary(ctx context.Context) (map[int64]MediaLibraryStat, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT library_id,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status='ok' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='unmatched' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status='error' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN sub_file<>'' THEN 1 ELSE 0 END), 0)
		FROM media_meta GROUP BY library_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]MediaLibraryStat{}
	for rows.Next() {
		var st MediaLibraryStat
		if err := rows.Scan(&st.LibraryID, &st.Total, &st.OK, &st.Unmatched, &st.Failed, &st.Subtitled); err != nil {
			return nil, err
		}
		out[st.LibraryID] = st
	}
	return out, rows.Err()
}

// RecentMedia 按最近写入顺序取一批刮削记录，给总览的「最新入库」用。
//
// 排序键是 updated_at：重新刮一条老片也会把它顶到前面，这符合「最近
// 有变动」的直觉，而且比 files 表的 updated_at 更贴近用户看到的信息
// （海报/简介是刮削写进去的）。
func (s *Store) RecentMedia(ctx context.Context, limit int) ([]Media, error) {
	if limit <= 0 || limit > 500 {
		limit = 60
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mediaCols+` FROM media_meta WHERE status='ok'
		 ORDER BY updated_at DESC, id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CountMedia 返回刮削台账条数（localTotal 用）。
func (s *Store) CountMedia(ctx context.Context, libraryID int64) (int, error) {
	var n int
	q := `SELECT COUNT(*) FROM media_meta`
	var err error
	if libraryID > 0 {
		err = s.db.QueryRowContext(ctx, q+` WHERE library_id=?`, libraryID).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx, q).Scan(&n)
	}
	return n, err
}

// ListMediaByPrefix 列某目录下所有刮削记录。
//
// Emby 的剧集条目 Path 指向剧目录，而台账里记的是一集一个 .strm；
// 列表卡片与详情抽屉要靠这个前缀查询把整部剧的本地信息补回来。
func (s *Store) ListMediaByPrefix(ctx context.Context, libraryID int64, prefix string) ([]Media, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+mediaCols+` FROM media_meta
		 WHERE library_id=? AND (rel_path=? OR rel_path LIKE ? ESCAPE '\')
		 ORDER BY rel_path`,
		libraryID, prefix, escapeLike(prefix)+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Media{}
	for rows.Next() {
		m, err := scanMedia(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
