// Package store 是唯一的持久化层：一个 SQLite 文件，手写 SQL，没有 ORM。
//
// 为什么不用 ORM：这里只有 4 张表、十来个查询，ORM 带来的迁移魔力和反射开销
// 比它省下的 SQL 更贵。手写 SQL 也让「这个查询扫不扫全表」一眼可见。
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // 纯 Go 实现，不需要 CGO / gcc
)

type Store struct {
	db *sql.DB
}

// Open 打开（或创建）数据库并建表。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// 单写者模型：这个程序的并发量极低，串行化能彻底避免 SQLITE_BUSY。
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id          INTEGER PRIMARY KEY AUTOINCREMENT,
			kind        TEXT NOT NULL,
			name        TEXT NOT NULL DEFAULT '',
			credential  TEXT NOT NULL,
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS libraries (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id     INTEGER NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
			name           TEXT NOT NULL,
			remote_cid     TEXT NOT NULL,
			remote_path    TEXT NOT NULL DEFAULT '',
			local_path     TEXT NOT NULL,
			collection_type TEXT NOT NULL DEFAULT 'movies',
			keep_ext       INTEGER NOT NULL DEFAULT 1,
			server_name    TEXT NOT NULL DEFAULT '',
			server_lib_id  TEXT NOT NULL DEFAULT '',
			last_sync_at   TEXT NOT NULL DEFAULT '',
			last_sync_files INTEGER NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL
		)`,
		// files 是台账：一个网盘文件一行。它回答两个问题：
		// 「这个 .strm 是不是我们放的」「上次同步时它叫什么名字」。
		`CREATE TABLE IF NOT EXISTS files (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			library_id INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			account_id INTEGER NOT NULL,
			file_id    TEXT NOT NULL,
			pick_code  TEXT NOT NULL,
			rel_path   TEXT NOT NULL,
			name       TEXT NOT NULL,
			size       INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			UNIQUE(library_id, file_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_library ON files(library_id, rel_path)`,
		// library_sources 是「这个库从哪些网盘目录取片」。
		//
		// 一个库挂 N 个来源（115 的目录 + 夸克的目录）是 v0.2.7 的核心变化：
		// 本地目录只有一个、Emby 库只有一个，差别只在 .strm 里那行 302 地址。
		// libraries 上的 account_id/remote_cid 保留为「主来源」，老代码和
		// 老 .strm 继续能用；新代码一律读这张表。
		`CREATE TABLE IF NOT EXISTS library_sources (
			id             INTEGER PRIMARY KEY AUTOINCREMENT,
			library_id     INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			account_id     INTEGER NOT NULL,
			remote_cid     TEXT NOT NULL,
			remote_path    TEXT NOT NULL DEFAULT '',
			last_sync_at   TEXT NOT NULL DEFAULT '',
			last_sync_files INTEGER NOT NULL DEFAULT 0,
			created_at     TEXT NOT NULL,
			UNIQUE(library_id, account_id, remote_cid)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_lib_sources ON library_sources(library_id)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		// media_meta 是刮削结果：一条视频一行，存解析出的片名/季集、匹配到的
		// IMDb 元数据，以及本地 nfo / 图片 / 字幕的落盘位置。
		//
		// 它和 files 台账是两张表而不是一张：台账随每次同步被重写（改名会换
		// rel_path），刮削结果则要跨越同步保留——重新刮一遍 IMDb 是要付费的
		// （风控），不能因为改了个目录名就丢掉。
		`CREATE TABLE IF NOT EXISTS media_meta (
			id            INTEGER PRIMARY KEY AUTOINCREMENT,
			library_id    INTEGER NOT NULL REFERENCES libraries(id) ON DELETE CASCADE,
			rel_path      TEXT NOT NULL,
			kind          TEXT NOT NULL DEFAULT '',
			title         TEXT NOT NULL DEFAULT '',
			year          TEXT NOT NULL DEFAULT '',
			season        INTEGER NOT NULL DEFAULT 0,
			episode       INTEGER NOT NULL DEFAULT 0,
			imdb_id       TEXT NOT NULL DEFAULT '',
			tmdb_id       INTEGER NOT NULL DEFAULT 0,
			meta_title    TEXT NOT NULL DEFAULT '',
			meta_year     TEXT NOT NULL DEFAULT '',
			rating        TEXT NOT NULL DEFAULT '',
			runtime       TEXT NOT NULL DEFAULT '',
			genres        TEXT NOT NULL DEFAULT '',
			actors        TEXT NOT NULL DEFAULT '',
			director      TEXT NOT NULL DEFAULT '',
			overview      TEXT NOT NULL DEFAULT '',
			poster_url    TEXT NOT NULL DEFAULT '',
			backdrop_url  TEXT NOT NULL DEFAULT '',
			poster_file   TEXT NOT NULL DEFAULT '',
			backdrop_file TEXT NOT NULL DEFAULT '',
			nfo_file      TEXT NOT NULL DEFAULT '',
			sub_file      TEXT NOT NULL DEFAULT '',
			sub_lang      TEXT NOT NULL DEFAULT '',
			sub_state     TEXT NOT NULL DEFAULT '',
			sub_origin    TEXT NOT NULL DEFAULT '',
			status        TEXT NOT NULL DEFAULT '',
			message       TEXT NOT NULL DEFAULT '',
			updated_at    TEXT NOT NULL,
			UNIQUE(library_id, rel_path)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_media_library ON media_meta(library_id, status)`,
		// meta_cache 按「类型 + 归一化片名 + 年份」缓存一次完整的 IMDb 查询结果。
		//
		// 存在的意义只有一个：同一部剧的 20 集只该查一次 IMDb。没有这张表，
		// 刮 24 集的一季就是 24 次请求，等于主动把自己送进限流名单。
		`CREATE TABLE IF NOT EXISTS meta_cache (
			cache_key  TEXT PRIMARY KEY,
			imdb_id    TEXT NOT NULL DEFAULT '',
			payload    TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		// translation_cache 按「原文哈希 + 目标语言 + 配置指纹」缓存单条字幕译文。
		// 同一部片的不同版本字幕有大量重复台词，命中的部分不必再调一次模型。
		`CREATE TABLE IF NOT EXISTS translation_cache (
			key        TEXT PRIMARY KEY,
			value      TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		// sync_runs 是同步流水账：每跑一轮记一行，开跑时插入、结束时补齐统计。
		//
		// 为什么单独存而不是只留内存里的 Job：Job 只反映「当前这一轮」，
		// 而 Strm 管理页真正要回答的是「昨天 03:00 那轮到底写了几个文件、
		// 删了几个、失败在哪」——这些只能落库，重启也不能丢。
		`CREATE TABLE IF NOT EXISTS sync_runs (
			id           INTEGER PRIMARY KEY AUTOINCREMENT,
			library_id   INTEGER NOT NULL,
			library_name TEXT NOT NULL DEFAULT '',
			trigger      TEXT NOT NULL DEFAULT '',
			phase        TEXT NOT NULL DEFAULT 'running',
			files        INTEGER NOT NULL DEFAULT 0,
			written      INTEGER NOT NULL DEFAULT 0,
			removed      INTEGER NOT NULL DEFAULT 0,
			failed       INTEGER NOT NULL DEFAULT 0,
			skipped      INTEGER NOT NULL DEFAULT 0,
			message      TEXT NOT NULL DEFAULT '',
			started_at   TEXT NOT NULL,
			finished_at  TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_sync_runs_started ON sync_runs(started_at DESC)`,
		// organize_records 是自动整理台账：一个网盘条目一行。
		//
		// 为什么要单独一张表而不是复用 files：files 是「同步的产物」，
		// 随每次同步被重写；整理台账要跨同步保留识别依据（reason）和
		// 用户的选择（忽略）。同一个文件被重试多次也只保留一行，靠
		// (account_id, file_id) 唯一约束覆盖写。
		`CREATE TABLE IF NOT EXISTS organize_records (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL,
			file_id    TEXT NOT NULL,
			name       TEXT NOT NULL DEFAULT '',
			is_dir     INTEGER NOT NULL DEFAULT 0,
			size       INTEGER NOT NULL DEFAULT 0,
			kind       TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			year       TEXT NOT NULL DEFAULT '',
			season     INTEGER NOT NULL DEFAULT 0,
			episode    INTEGER NOT NULL DEFAULT 0,
			confidence INTEGER NOT NULL DEFAULT 0,
			reason     TEXT NOT NULL DEFAULT '',
			target     TEXT NOT NULL DEFAULT '',
			status     TEXT NOT NULL DEFAULT '',
			message    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE(account_id, file_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_organize_status ON organize_records(status, id DESC)`,
		// subscriptions 是「我要这部剧，出新集自动帮我下」的订阅。
		//
		// 核心是 watermark_season / watermark_episode 两个数字：没有它们，
		// 每轮检查都会从第一集重新搜一遍，结果是同一集被反复提交离线。
		// 它们记的是「已经提交成功的最高集」，不是「已经入库的最高集」——
		// 入库状态由台账（media / files）说了算，订阅这边不需要重复跟踪，
		// 否则提交成功但离线失败时会把水位线卡住，永远不再重试。
		`CREATE TABLE IF NOT EXISTS subscriptions (
			id               INTEGER PRIMARY KEY AUTOINCREMENT,
			tmdb_id          INTEGER NOT NULL,
			imdb_id          TEXT NOT NULL DEFAULT '',
			media_type       TEXT NOT NULL DEFAULT 'tv',
			title            TEXT NOT NULL DEFAULT '',
			original_title   TEXT NOT NULL DEFAULT '',
			year             INTEGER NOT NULL DEFAULT 0,
			library_id       INTEGER NOT NULL DEFAULT 0,
			save_cid         TEXT NOT NULL DEFAULT '',
			quality          TEXT NOT NULL DEFAULT '1080p',
			min_size         INTEGER NOT NULL DEFAULT 0,
			max_size         INTEGER NOT NULL DEFAULT 0,
			state            TEXT NOT NULL DEFAULT 'watching',
			watermark_season INTEGER NOT NULL DEFAULT 0,
			watermark_episode INTEGER NOT NULL DEFAULT 0,
			limit_season     INTEGER NOT NULL DEFAULT 0,
			limit_episode    INTEGER NOT NULL DEFAULT 0,
			total_episodes   INTEGER NOT NULL DEFAULT 0,
			last_check_at    TEXT NOT NULL DEFAULT '',
			next_check_at    TEXT NOT NULL DEFAULT '',
			last_result      TEXT NOT NULL DEFAULT '',
			fail_count       INTEGER NOT NULL DEFAULT 0,
			created_at       TEXT NOT NULL,
			updated_at       TEXT NOT NULL,
			UNIQUE(media_type, tmdb_id)
		)`,
		// 轮询器每轮只查「到期且没暂停」的行，这个索引就是给那条 SQL 用的。
		`CREATE INDEX IF NOT EXISTS idx_sub_due ON subscriptions(state, next_check_at)`,
		// api_calls 是「今天调了 115 多少次」的落库账。
		//
		// 为什么不在内存里数完就算：进程重启（换镜像、崩了）之后用户看到的数字
		// 会归零，而 115 的风控额度是按天算的 —— 数字对不上比没有更糟。
		// 一行一天一个账号，写入按分钟批量灌，不是每次请求都写。
		`CREATE TABLE IF NOT EXISTS api_calls (
			day        TEXT NOT NULL,
			account_id INTEGER NOT NULL,
			n          INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, account_id)
		)`,
		// notifications 是右上角铃铛里的站内消息。和「外推渠道」分开存：
		// 关掉 TG / 企微推送只是不往外发，站内仍要留痕供回看。
		`CREATE TABLE IF NOT EXISTS notifications (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			event      TEXT NOT NULL DEFAULT '',
			kind       TEXT NOT NULL DEFAULT '',
			title      TEXT NOT NULL DEFAULT '',
			content    TEXT NOT NULL DEFAULT '',
			link       TEXT NOT NULL DEFAULT '',
			level      TEXT NOT NULL DEFAULT 'info',
			read_at    TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_notifications_created ON notifications(created_at DESC)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("建表失败: %w", err)
		}
	}

	// 老库（v0.2.6 及之前）每个库只记了一个账号+目录。建表之后立刻回填一次，
	// 让它们在下一轮同步就走多来源逻辑；幂等，重启多少次都一样。
	if err := s.backfillSources(); err != nil {
		return fmt.Errorf("回填媒体库来源失败: %w", err)
	}

	// 老库的 media_meta 没有 tmdb_id。CREATE TABLE IF NOT EXISTS 不会补列，
	// 而 mediaCols 的 SELECT 会直接报「no such column」——必须在启动时补。
	// PRAGMA 查一次再 ALTER，重复启动是幂等的。
	if err := s.ensureColumn("media_meta", "tmdb_id",
		`ALTER TABLE media_meta ADD COLUMN tmdb_id INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}

	// 同理：sub_origin（zh / translated）是「字幕是怎么来的」这一列，
	// 老库没有它，SELECT 会直接报 no such column。
	if err := s.ensureColumn("media_meta", "sub_origin",
		`ALTER TABLE media_meta ADD COLUMN sub_origin TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	// 订阅的 limit_* 是旧版「限制线」的列。功能已合并进 watermark，
	// 这里保留列只为把老数据搬过去（见下面的 migrateSubscriptionLines）。
	if err := s.ensureColumn("subscriptions", "limit_season",
		`ALTER TABLE subscriptions ADD COLUMN limit_season INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.ensureColumn("subscriptions", "limit_episode",
		`ALTER TABLE subscriptions ADD COLUMN limit_episode INTEGER NOT NULL DEFAULT 0`); err != nil {
		return err
	}
	if err := s.migrateSubscriptionLines(); err != nil {
		return err
	}

	// 一次性数据迁移：新增内置种子站后，把「还停在旧默认值」的配置补齐。
	//
	// res_sites 是用户可勾的选项，一旦存过值，后端就永远按这份值搜，新增的
	// 站对老用户等于没加。这里只认精确等于「上一版默认值」的行，自己勾过
	// 站点的配置一个字都不动；version 标记保证每一步只跑一次 ——
	// 否则用户特意取消勾选的站，下次启动又会被塞回来。
	var v string
	err := s.db.QueryRow(`SELECT value FROM settings WHERE key='res_sites_version'`).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("读 res_sites_version 失败: %w", err)
	}
	// version 记的是「已经补到第几步」，历史上第一版补完写的就是 2，
	// 所以映射关系是 done = version-1：没有标记 = 一步都没补。
	marker, _ := strconv.Atoi(strings.TrimSpace(v))
	done := 0
	if marker > 0 {
		done = marker - 1
	}
	// 老库可能停在任意一版，从它停在的那一步往后依次补。
	for i := done; i < len(resSitesSteps); i++ {
		step := resSitesSteps[i]
		if _, e := s.db.Exec(
			`UPDATE settings SET value=? WHERE key='res_sites' AND value=?`,
			step.to, step.from); e != nil {
			return fmt.Errorf("补齐内置种子站失败: %w", e)
		}
	}
	if done >= len(resSitesSteps) {
		return nil
	}
	if _, e := s.db.Exec(
		`INSERT INTO settings(key,value) VALUES('res_sites_version',?) `+
			`ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
		strconv.Itoa(len(resSitesSteps)+1)); e != nil {
		return fmt.Errorf("写 res_sites_version 失败: %w", e)
	}
	return nil
}

// resSitesSteps 是 res_sites 默认值的历次追加，顺序即版本顺序。
// 每步只把「精确等于上一版默认值」的行改成新版 —— 用户自己勾过的配置不碰。
var resSitesSteps = []struct{ from, to string }{
	// v2：加 UIndex / TheRarBG
	{"tpb,nyaa,dmhy,mikan", "tpb,nyaa,dmhy,mikan,uindex,therarbg"},
	// v3：加磁力猫
	{"tpb,nyaa,dmhy,mikan,uindex,therarbg", "tpb,nyaa,dmhy,mikan,uindex,therarbg,clm"},
}

// migrateSubscriptionLines 把旧版的两条线合并成一条起播线。
//
// 规则：起播线取两者中更靠后的那个 —— 用户设限制线是为了「老集别下」，
// 水位线是程序推进的进度，合并后不能把任何一边的意图丢掉。
// 搬完把 limit_* 清零，函数就是幂等的：下次启动 WHERE 不命中，什么都不做。
func (s *Store) migrateSubscriptionLines() error {
	if _, err := s.db.Exec(`UPDATE subscriptions
		SET watermark_season = limit_season, watermark_episode = limit_episode
		WHERE limit_season > watermark_season
		   OR (limit_season = watermark_season AND limit_episode > watermark_episode)`); err != nil {
		return fmt.Errorf("合并订阅起播线失败: %w", err)
	}
	if _, err := s.db.Exec(`UPDATE subscriptions
		SET limit_season = 0, limit_episode = 0
		WHERE limit_season <> 0 OR limit_episode <> 0`); err != nil {
		return fmt.Errorf("清理旧限制线失败: %w", err)
	}
	return nil
}

// ensureColumn 给老库补一列。SQLite 的 ADD COLUMN 不支持 IF NOT EXISTS，
// 所以先查 PRAGMA；这是唯一一处允许在 migrate 里改表结构的地方。
func (s *Store) ensureColumn(table, column, alter string) error {
	rows, err := s.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return fmt.Errorf("读 %s 表结构失败: %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("读 %s 表结构失败: %w", table, err)
		}
		if name == column {
			return nil
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("读 %s 表结构失败: %w", table, err)
	}
	if _, err := s.db.Exec(alter); err != nil {
		return fmt.Errorf("给 %s 补 %s 列失败: %w", table, column, err)
	}
	return nil
}

// ==================== 账号 ====================

type Account struct {
	ID         int64     `json:"id"`
	Kind       string    `json:"kind"`
	Name       string    `json:"name"`
	Credential string    `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
}

func (s *Store) CreateAccount(ctx context.Context, kind, name, credential string) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO accounts(kind, name, credential, created_at, updated_at) VALUES(?,?,?,?,?)`,
		kind, name, credential, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) ListAccounts(ctx context.Context) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, name, created_at FROM accounts ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var created string
		if err := rows.Scan(&a.ID, &a.Kind, &a.Name, &created); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339, created)
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAccount 连凭证一起取（只给后端内部用，不要直接序列化给前端）。
func (s *Store) GetAccount(ctx context.Context, id int64) (Account, error) {
	var a Account
	var created string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, name, credential, created_at FROM accounts WHERE id=?`, id).
		Scan(&a.ID, &a.Kind, &a.Name, &a.Credential, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	if err != nil {
		return a, err
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return a, nil
}

// UpdateAccountCredential 换掉账号凭证。
//
// 夸克用得到：扫码登录写 refresh_token，后来补的网页 Cookie 也要写进同一份
// credential JSON。调用方必须传完整的新 JSON —— 这里是整段替换，不是合并。
func (s *Store) UpdateAccountCredential(ctx context.Context, id int64, credential string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET credential=?, updated_at=? WHERE id=?`,
		credential, time.Now().Format(time.RFC3339), id)
	return err
}

func (s *Store) UpdateAccountName(ctx context.Context, id int64, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE accounts SET name=?, updated_at=? WHERE id=?`,
		name, time.Now().Format(time.RFC3339), id)
	return err
}

func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM accounts WHERE id=?`, id)
	return err
}

// AccountByPickcode 反查某个 pickcode 属于哪个账号。播放代理用它决定用谁的凭证取直链。
func (s *Store) AccountByPickcode(ctx context.Context, pickCode string) (int64, bool) {
	var accountID int64
	err := s.db.QueryRowContext(ctx,
		`SELECT account_id FROM files WHERE pick_code=? LIMIT 1`, pickCode).Scan(&accountID)
	return accountID, err == nil
}

// FirstAccountID 返回第一个账号（播放兜底用：台账里查不到时只能猜一个）。
func (s *Store) FirstAccountID(ctx context.Context) int64 {
	var id int64
	_ = s.db.QueryRowContext(ctx, `SELECT id FROM accounts ORDER BY id LIMIT 1`).Scan(&id)
	return id
}

// ==================== 媒体库 ====================

type Library struct {
	ID             int64     `json:"id"`
	AccountID      int64     `json:"account_id"`
	Name           string    `json:"name"`
	RemoteCID      string    `json:"remote_cid"`
	RemotePath     string    `json:"remote_path"`
	LocalPath      string    `json:"local_path"`
	CollectionType string    `json:"collection_type"`
	KeepExt        bool      `json:"keep_ext"`
	ServerName     string    `json:"server_name"`
	ServerLibID    string    `json:"server_lib_id"`
	LastSyncAt     time.Time `json:"last_sync_at"`
	LastSyncFiles  int       `json:"last_sync_files"`
	CreatedAt      time.Time `json:"created_at"`

	// Sources 是这个库挂的全部网盘目录（115 一份、夸克一份…）。
	// AccountID / RemoteCID 仍然保留，等于 Sources[0]，历史调用方继续可用。
	Sources []LibrarySource `json:"sources,omitempty"`
}

const libCols = `id, account_id, name, remote_cid, remote_path, local_path, collection_type,
	keep_ext, server_name, server_lib_id, last_sync_at, last_sync_files, created_at`

func scanLibrary(sc interface{ Scan(...any) error }) (Library, error) {
	var l Library
	var keepExt int
	var lastSync, created string
	err := sc.Scan(&l.ID, &l.AccountID, &l.Name, &l.RemoteCID, &l.RemotePath, &l.LocalPath,
		&l.CollectionType, &keepExt, &l.ServerName, &l.ServerLibID, &lastSync, &l.LastSyncFiles, &created)
	if err != nil {
		return l, err
	}
	l.KeepExt = keepExt != 0
	l.LastSyncAt, _ = time.Parse(time.RFC3339, lastSync)
	l.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return l, nil
}

func (s *Store) CreateLibrary(ctx context.Context, l Library) (int64, error) {
	now := time.Now().Format(time.RFC3339)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO libraries(account_id, name, remote_cid, remote_path, local_path, collection_type,
			keep_ext, server_name, server_lib_id, created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		l.AccountID, l.Name, l.RemoteCID, l.RemotePath, l.LocalPath, l.CollectionType,
		boolInt(l.KeepExt), l.ServerName, l.ServerLibID, now)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// 主来源和库一起落库：少了这一行的库在同步/播放时都算「没有来源」，
	// 那是一个只会在半小时后暴露的空库，不如现在就失败。
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO library_sources(library_id, account_id, remote_cid, remote_path, created_at)
		 VALUES(?,?,?,?,?)`, id, l.AccountID, l.RemoteCID, l.RemotePath, now); err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (s *Store) ListLibraries(ctx context.Context) ([]Library, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+libCols+` FROM libraries ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Library
	for rows.Next() {
		l, err := scanLibrary(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 一次把来源全取回来：库的数量是个位数，逐个查也只是多几次 IO，
	// 但列表接口每 5 秒被轮询一次，攒成 N+1 就不划算了。
	srcs, err := s.SourcesByLibrary(ctx)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Sources = srcs[out[i].ID]
	}
	return out, nil
}

func (s *Store) GetLibrary(ctx context.Context, id int64) (Library, error) {
	l, err := scanLibrary(s.db.QueryRowContext(ctx, `SELECT `+libCols+` FROM libraries WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return l, ErrNotFound
	}
	if err != nil {
		return l, err
	}
	if l.Sources, err = s.ListSources(ctx, id); err != nil {
		return l, err
	}
	return l, nil
}

func (s *Store) DeleteLibrary(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM libraries WHERE id=?`, id)
	return err
}

func (s *Store) MarkLibrarySynced(ctx context.Context, id int64, at time.Time, files int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE libraries SET last_sync_at=?, last_sync_files=? WHERE id=?`,
		at.Format(time.RFC3339), files, id)
	return err
}

// SetLibraryServer 记住「这个库对应 Emby 里的哪个库」，下次加库就能幂等。
func (s *Store) SetLibraryServer(ctx context.Context, id int64, serverName, libID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE libraries SET server_name=?, server_lib_id=? WHERE id=?`, serverName, libID, id)
	return err
}

// ==================== 台账 ====================

type File struct {
	ID        int64  `json:"id"`
	LibraryID int64  `json:"library_id"`
	AccountID int64  `json:"account_id"`
	FileID    string `json:"file_id"`
	PickCode  string `json:"pick_code"`
	RelPath   string `json:"rel_path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
}

// UpsertFiles 批量写台账（一个事务）。
// ON CONFLICT 更新 rel_path：文件在网盘被改名/移动后，本地也要跟着改。
//
// accountID 是「默认归属」：一个库挂多个网盘之后，同一批行可能来自不同账号，
// 每行自己带的 AccountID 优先。播放代理靠这一列决定用谁的凭证换直链，
// 写错了表现就是「片子看得见、点开 502」。
func (s *Store) UpsertFiles(ctx context.Context, accountID int64, fs []File) error {
	if len(fs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO files(library_id, account_id, file_id, pick_code, rel_path, name, size, updated_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(library_id, file_id) DO UPDATE SET
		   account_id=excluded.account_id,
		   pick_code=excluded.pick_code, rel_path=excluded.rel_path,
		   name=excluded.name, size=excluded.size, updated_at=excluded.updated_at`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	now := time.Now().Format(time.RFC3339)
	for _, f := range fs {
		owner := f.AccountID
		if owner == 0 {
			owner = accountID
		}
		if _, err := stmt.ExecContext(ctx, f.LibraryID, owner, f.FileID, f.PickCode,
			f.RelPath, f.Name, f.Size, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListFiles 分页列台账。
func (s *Store) ListFiles(ctx context.Context, libraryID int64, limit, offset int) ([]File, int, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE library_id=?`, libraryID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, library_id, account_id, file_id, pick_code, rel_path, name, size
		 FROM files WHERE library_id=? ORDER BY rel_path LIMIT ? OFFSET ?`, libraryID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.LibraryID, &f.AccountID, &f.FileID, &f.PickCode, &f.RelPath, &f.Name, &f.Size); err != nil {
			return nil, 0, err
		}
		out = append(out, f)
	}
	return out, total, rows.Err()
}

// FileRelPaths 返回 file_id → rel_path 的全量映射。
//
// 同步开始前读一次，用途有二：判断 .strm 是否需要重写；文件在网盘被改名后，
// 知道旧路径好把残留的旧 .strm 删掉（不删的话 Emby 会看到两个同名条目）。
func (s *Store) FileRelPaths(ctx context.Context, libraryID int64) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT file_id, rel_path FROM files WHERE library_id=?`, libraryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, 1024)
	for rows.Next() {
		var id, rel string
		if err := rows.Scan(&id, &rel); err != nil {
			return nil, err
		}
		out[id] = rel
	}
	return out, rows.Err()
}

// IterStale 遍历台账里「这次同步没见到」的行——它们在网盘侧已被删除或移出了本库。
//
// 用 keyset 分页（id > lastID）而不是一次读出全表：一个几万文件的库，
// 全表扫一遍再比对会把几十 MB 的字符串压在内存里，而这个程序跑在 NAS 上。
func (s *Store) IterStale(ctx context.Context, libraryID int64, seen map[string]struct{}, fn func(File) error) error {
	return s.IterStaleByAccount(ctx, libraryID, 0, seen, fn)
}

// IterStaleByAccount 只清理某个来源（账号）名下的失效行。
//
// 一个库挂两个网盘时这条过滤是必须的：扫完 115 那一半就用「这次见到的 file_id」
// 去清全库，会把夸克那半边的台账行连同 .strm 一起删掉——下一轮同步又写回来，
// Emby 于是每轮都看到一批条目消失又出现。accountID=0 表示不按账号过滤。
func (s *Store) IterStaleByAccount(ctx context.Context, libraryID, accountID int64,
	seen map[string]struct{}, fn func(File) error) error {
	const page = 1000
	// 占位符顺序必须和 SQL 里的顺序一致：library_id → account_id（可选）→ id>。
	where := `library_id=?`
	args := []any{libraryID}
	if accountID > 0 {
		where += ` AND account_id=?`
		args = append(args, accountID)
	}
	where += ` AND id>?`
	var lastID int64
	for {
		q := `SELECT id, library_id, account_id, file_id, pick_code, rel_path, name, size
			 FROM files WHERE ` + where + ` ORDER BY id LIMIT ?`
		rows, err := s.db.QueryContext(ctx, q, append(args, lastID, page)...)
		if err != nil {
			return err
		}
		n := 0
		for rows.Next() {
			var f File
			if err := rows.Scan(&f.ID, &f.LibraryID, &f.AccountID, &f.FileID, &f.PickCode, &f.RelPath, &f.Name, &f.Size); err != nil {
				rows.Close()
				return err
			}
			n++
			lastID = f.ID
			if _, ok := seen[f.FileID]; !ok {
				if err := fn(f); err != nil {
					rows.Close()
					return err
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if n < page {
			return nil
		}
	}
}

func (s *Store) DeleteFileIDs(ctx context.Context, ids []int64) error {
	const chunk = 500
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		args := make([]any, len(batch))
		for j, id := range batch {
			args[j] = id
		}
		q := `DELETE FROM files WHERE id IN (?` + strings.Repeat(",?", len(batch)-1) + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

// DeleteFilesByFileIDs 按网盘文件 ID 删台账行。
//
// 同步去重后要把「多余副本」的行清掉，但那时手里只有网盘的 file_id，
// 没有自增主键；专门查一遍 id 只为拼 IN 子句不值当。
func (s *Store) DeleteFilesByFileIDs(ctx context.Context, libraryID int64, fileIDs []string) error {
	const chunk = 500
	for i := 0; i < len(fileIDs); i += chunk {
		end := i + chunk
		if end > len(fileIDs) {
			end = len(fileIDs)
		}
		batch := fileIDs[i:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, libraryID)
		for _, id := range batch {
			args = append(args, id)
		}
		q := `DELETE FROM files WHERE library_id=? AND file_id IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) CountFiles(ctx context.Context, libraryID int64) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files WHERE library_id=?`, libraryID).Scan(&n)
	return n, err
}

// TotalFileBytes 返回台账里所有视频的体积之和。
//
// 总览面板的「已入库体积」用它。刻意不去问 115 剩余空间：115 的空间接口
// 需要额外的登录态与权限，而「我们到底管着多少片子」这个数字台账本来就有，
// 一次 SUM 就够，不必为了一行展示去多调一个可能失败的外部接口。
func (s *Store) TotalFileBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM files`).Scan(&n)
	return n, err
}

// ==================== 配置 ====================

func (s *Store) GetSetting(ctx context.Context, key string) (string, bool) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	return v, err == nil
}

func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO settings(key, value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ==================== 杂项 ====================

var ErrNotFound = errors.New("记录不存在")

// ErrDuplicate 表示唯一约束冲突（同一个库重复挂同一个账号的同一个目录）。
// 单独一个哨兵，是为了让接口层能回 409 + 一句人话，而不是把 SQLite 的
// "UNIQUE constraint failed" 直接甩到界面上。
var ErrDuplicate = errors.New("记录已存在")

func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "constraint failed")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// SanitizeDSNPath 只用于日志/显示，避免把带反斜杠的 Windows 路径拼进 SQLite DSN。
func SanitizeDSNPath(p string) string { return url.PathEscape(strings.TrimSpace(p)) }

// GetFileByRelPath 按 .strm 相对路径反查网盘文件（删条目时要拿 file_id）。
func (s *Store) GetFileByRelPath(ctx context.Context, libraryID int64, rel string) (File, bool, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, library_id, account_id, file_id, pick_code, rel_path, name, size
		 FROM files WHERE library_id=? AND rel_path=?`, libraryID, rel)
	var f File
	err := row.Scan(&f.ID, &f.LibraryID, &f.AccountID, &f.FileID, &f.PickCode, &f.RelPath, &f.Name, &f.Size)
	if errors.Is(err, sql.ErrNoRows) {
		return File{}, false, nil
	}
	return f, err == nil, err
}

// ListFilesByPrefix 列某目录下所有网盘文件台账（整部剧一起删时用）。
func (s *Store) ListFilesByPrefix(ctx context.Context, libraryID int64, prefix string) ([]File, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, library_id, account_id, file_id, pick_code, rel_path, name, size
		 FROM files WHERE library_id=? AND (rel_path=? OR rel_path LIKE ? ESCAPE '\')
		 ORDER BY rel_path`,
		libraryID, prefix, escapeLike(prefix)+"/%")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.ID, &f.LibraryID, &f.AccountID, &f.FileID, &f.PickCode, &f.RelPath, &f.Name, &f.Size); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteFilesByRelPaths 删一批文件台账行。
func (s *Store) DeleteFilesByRelPaths(ctx context.Context, libraryID int64, rels []string) error {
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
		q := `DELETE FROM files WHERE library_id=? AND rel_path IN (?` +
			strings.Repeat(",?", len(batch)-1) + `)`
		if _, err := s.db.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	return nil
}
