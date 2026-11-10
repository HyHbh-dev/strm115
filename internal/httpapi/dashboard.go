package httpapi

// 总览面板的数据源。
//
// 这一页以前整页都是 mock：数字写死、按钮 setTimeout 假装在跑、海报是随机
// 渐变色块。现在每个字段都必须能回答「它从哪来」：
//
//	库 / 台账 / 刮削统计 → SQLite 的 libraries / files / media_meta
//	正在跑的任务        → 进程内存里的 sync.Job / scrape.Job / subtitleJob
//	上次全量            → sync_runs 流水账（重启不丢）
//	115 通道            → panpool 的实时计数（请求 / 重试 / 风控 / 冷却）
//	最新入库            → media_meta 按 updated_at 倒序，海报走本地图片接口
//
// 一个刻意的边界：这里不探 Emby 的连通性。那是一次真正的外部 HTTP 请求，
// 超时上限是秒级的，塞进首屏会让「打开总览」在最坏情况下卡几秒。前端并行
// 调 /api/settings/server-status，两边的结果在页面上合并，谁慢都不拖累谁。

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"time"

	"strm115/internal/scrape"
	"strm115/internal/store"
)

type dashServerInfo struct {
	Version   string `json:"version"`
	UptimeSec int64  `json:"uptime_sec"`
	User      string `json:"user"`
	MediaRoot string `json:"media_root"`
	Domain    string `json:"play_domain"`
}

// dashService 是顶部那一排状态徽章。
type dashService struct {
	Key    string `json:"key"`   // 115 / imdb / tmdb
	Label  string `json:"label"` // 显示名
	State  string `json:"state"` // ok / warn / off
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"` // 有问题时告诉用户去哪修
}

type dashTotals struct {
	Libraries int   `json:"libraries"`
	Files     int   `json:"files"`     // 台账条数 = 应有的 .strm 数
	Bytes     int64 `json:"bytes"`     // 台账里的视频体积之和
	Scraped   int   `json:"scraped"`   // 刮削成功（status=ok）
	Unmatched int   `json:"unmatched"` // 没匹配上
	Failed    int   `json:"failed"`    // 刮削出错
	Subtitled int   `json:"subtitled"` // 已有字幕
	Running   int   `json:"running"`   // 正在跑的任务数
}

type dashJob struct {
	Kind      string `json:"kind"` // sync / scrape / subtitle
	LibraryID int64  `json:"library_id"`
	Library   string `json:"library"`
	Phase     string `json:"phase"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Percent   int    `json:"percent"` // -1 表示总量未知（同步要边走边发现）
	Message   string `json:"message"`
	StartedAt string `json:"started_at"`
}

type dashLibrary struct {
	ID             int64    `json:"id"`
	Name           string   `json:"name"`
	AccountName    string   `json:"account_name"`
	RemotePath     string   `json:"remote_path"`
	LocalRoot      string   `json:"local_root"`
	CollectionType string   `json:"collection_type"`
	Files          int      `json:"files"`
	Scraped        int      `json:"scraped"`
	Unmatched      int      `json:"unmatched"`
	Subtitled      int      `json:"subtitled"`
	LastSyncAt     string   `json:"last_sync_at"`
	LastSyncFiles  int      `json:"last_sync_files"`
	Status         string   `json:"status"` // ok / syncing / pending / error
	Job            *dashJob `json:"job,omitempty"`
}

type dashRecentItem struct {
	ID        int64  `json:"id"`
	LibraryID int64  `json:"library_id"`
	Library   string `json:"library"`
	Name      string `json:"name"`
	Year      string `json:"year"`
	Type      string `json:"type"` // movie / series
	Rating    string `json:"rating"`
	Poster    string `json:"poster"` // 本地已下载的海报；没有则为空
	Added     string `json:"added"`
	RelPath   string `json:"rel_path"`
	Episodes  int    `json:"episodes,omitempty"`
}

// handleDashboard 一次性返回总览面板首屏需要的东西。
func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	accts, err := s.st.ListAccounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	mediaStats, err := s.st.MediaStatsByLibrary(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	bytes, _ := s.st.TotalFileBytes(ctx)

	acctName := make(map[int64]string, len(accts))
	for _, a := range accts {
		acctName[a.ID] = a.Name
	}

	libName := make(map[int64]string, len(libs))
	jobs := make([]dashJob, 0, 4)
	totals := dashTotals{Libraries: len(libs), Bytes: bytes}
	libOut := make([]dashLibrary, 0, len(libs))
	var lastSync time.Time

	for _, l := range libs {
		n, _ := s.st.CountFiles(ctx, l.ID)
		libName[l.ID] = l.Name
		totals.Files += n

		st := mediaStats[l.ID]
		totals.Scraped += st.OK
		totals.Unmatched += st.Unmatched
		totals.Failed += st.Failed
		totals.Subtitled += st.Subtitled

		out := dashLibrary{
			ID: l.ID, Name: l.Name, AccountName: acctName[l.AccountID],
			RemotePath: l.RemotePath, LocalRoot: s.libRoot(l),
			CollectionType: l.CollectionType,
			Files:          n, Scraped: st.OK, Unmatched: st.Unmatched, Subtitled: st.Subtitled,
			LastSyncAt: formatTime(l.LastSyncAt), LastSyncFiles: l.LastSyncFiles,
		}
		if job := s.libraryJob(l.ID, l.Name); job != nil {
			out.Job = job
			jobs = append(jobs, *job)
			if job.Phase == "error" {
				out.Status = "error"
			} else {
				out.Status = "syncing"
			}
		} else if l.LastSyncAt.IsZero() {
			out.Status = "pending"
		} else {
			out.Status = "ok"
		}
		if l.LastSyncAt.After(lastSync) {
			lastSync = l.LastSyncAt
		}
		libOut = append(libOut, out)
	}
	totals.Running = len(jobs)

	// 整库补字幕的任务挂在 Server 上而不是按库的 map 里，单独收一遍。
	s.subMu.Lock()
	for id, j := range s.subJobs {
		p := j.snapshot()
		if !p.Running {
			continue
		}
		jobs = append(jobs, dashJob{
			Kind: "subtitle", LibraryID: id, Library: libName[id],
			Phase: p.Phase, Done: p.Done, Total: p.Total, Percent: ratio(p.Done, p.Total),
			Message: p.Message, StartedAt: formatTime(p.StartedAt),
		})
	}
	s.subMu.Unlock()
	totals.Running = len(jobs)

	runs, err := s.st.ListSyncRuns(ctx, 8)
	if err != nil || runs == nil {
		runs = []store.SyncRun{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"server": dashServerInfo{
			Version:   Version,
			UptimeSec: int64(time.Since(s.start).Seconds()),
			User:      s.currentUser(),
			MediaRoot: s.cfg.MediaRoot,
			Domain:    s.setting(ctx, "play_domain"),
		},
		"services":       s.dashServices(ctx, accts),
		"emby":           s.dashEmby(ctx),
		"totals":         totals,
		"libraries":      libOut,
		"recent":         s.dashRecent(ctx, libs, 12),
		"jobs":           jobs,
		"runs":           runs,
		"last_sync_at":   formatTime(lastSync),
		"last_full_sync": formatTime(s.st.LastFullSyncAt(ctx)),
		"generated_at":   formatTime(time.Now()),
	})
}

// libraryJob 挑出这个库当前「正在跑」的任务（同步优先，其次刮削）。
//
// 已经跑完但 Job 还在内存里的那一轮不返回：总览要回答「现在忙不忙」，
// 历史结果有 sync_runs，不该和进行中的任务混在一起。
func (s *Server) libraryJob(id int64, name string) *dashJob {
	if j := s.sync.Job(id); j != nil {
		p := j.Snapshot()
		if p.Running {
			return &dashJob{
				Kind: "sync", LibraryID: id, Library: name,
				Phase: p.Phase, Done: p.Written, Percent: -1,
				Message: p.Message, StartedAt: formatTime(p.StartedAt),
			}
		}
	}
	if j := s.scrape.Job(id); j != nil {
		p := j.Snapshot()
		if p.Running {
			return &dashJob{
				Kind: "scrape", LibraryID: id, Library: name,
				Phase: p.Phase, Done: p.Done, Total: p.Total, Percent: ratio(p.Done, p.Total),
				Message: p.Message, StartedAt: formatTime(p.StartedAt),
			}
		}
	}
	return nil
}

func ratio(done, total int) int {
	if total <= 0 {
		return -1
	}
	n := done * 100 / total
	if n > 100 {
		n = 100
	}
	if n < 0 {
		n = 0
	}
	return n
}

// dashServices 组装顶部徽章：115 通道、IMDb、TMDB。
//
// 三个都是「能自己判断的」：115 看连接池快照，两个 Key 看设置里有没有值。
// Emby 不在这里，原因见文件头。
func (s *Server) dashServices(ctx context.Context, accts []store.Account) []dashService {
	out := make([]dashService, 0, 3)

	live, cooling := 0, 0
	var lastErr string
	for _, a := range accts {
		snap, up := s.pool.Snapshot(a.ID)
		if up {
			live++
		}
		if !snap.CooldownUntil.IsZero() && snap.CooldownUntil.After(time.Now()) {
			cooling++
		}
		if snap.LastError != "" {
			lastErr = snap.LastError
		}
	}
	svc115 := dashService{Key: "115", Label: "115 网盘"}
	switch {
	case len(accts) == 0:
		svc115.State, svc115.Detail, svc115.Hint = "off", "还没有登录账号", "去「账号与媒体库」扫码或填 Cookie"
	case live == 0:
		// 连接池是懒加载的：进程刚起来时即使凭据完全正常，这里也是 0。
		// 所以说「本次启动后还没连过」而不是「连不上」——后者会让人白去重登。
		svc115.State = "warn"
		svc115.Detail = fmt.Sprintf("%d 个账号 · 本次启动后还没连过", len(accts))
		svc115.Hint = "跑一次同步或打开「网盘文件」就会自动建立连接"
	case cooling > 0:
		svc115.State = "warn"
		svc115.Detail = fmt.Sprintf("%d 个账号 · %d 个冷却中", len(accts), cooling)
	default:
		svc115.State = "ok"
		svc115.Detail = fmt.Sprintf("%d 个账号 · %d 个在线", len(accts), live)
	}
	if lastErr != "" && svc115.State == "ok" {
		svc115.Detail += " · 最近有报错"
	}
	out = append(out, svc115)

	imdb := dashService{Key: "imdb", Label: "IMDb"}
	if s.setting(ctx, "imdb_api_key") != "" {
		imdb.State, imdb.Detail = "ok", "已配置官方 Key"
	} else {
		imdb.State, imdb.Detail, imdb.Hint = "warn", "免费接口（易限流）", "到「系统配置」填 IMDb Key 可显著提速"
	}
	out = append(out, imdb)

	tmdb := dashService{Key: "tmdb", Label: "TMDB"}
	if s.setting(ctx, "tmdb_api_key") != "" {
		tmdb.State, tmdb.Detail = "ok", "已配置"
	} else {
		tmdb.State, tmdb.Detail, tmdb.Hint = "warn", "未配置 Key", "「发现与订阅」的榜单需要 TMDB Key"
	}
	out = append(out, tmdb)

	return out
}

// dashEmby 只回配置状态，连通性由前端另调 /api/settings/server-status 合并。
func (s *Server) dashEmby(ctx context.Context) map[string]any {
	kind := s.setting(ctx, "server_kind")
	if kind == "" {
		kind = "emby"
	}
	url := s.setting(ctx, "server_url")
	key := s.setting(ctx, "server_key")
	return map[string]any{
		"kind":       kind,
		"url":        url,
		"configured": url != "" && key != "",
	}
}

// dashRecent 取「最新入库」。
//
// 剧集在 media_meta 里是一集一行，直接按行展示会把同一部剧的几十集刷满整排，
// 所以按「库 + 剧根目录」合并成一张卡（跨季也合并），Episodes 记集数。
func (s *Server) dashRecent(ctx context.Context, libs []store.Library, limit int) []dashRecentItem {
	rows, err := s.st.RecentMedia(ctx, limit*4)
	if err != nil {
		return []dashRecentItem{}
	}
	libName := make(map[int64]string, len(libs))
	libRoot := make(map[int64]string, len(libs))
	for _, l := range libs {
		libName[l.ID] = l.Name
		libRoot[l.ID] = s.libRoot(l)
	}

	out := make([]dashRecentItem, 0, limit)
	seen := make(map[string]int, len(rows))
	for _, m := range rows {
		key := m.RelPath
		if m.Kind == "series" {
			key = scrape.ShowRoot(m.RelPath)
		}
		key = strconv.FormatInt(m.LibraryID, 10) + ":" + key
		if i, ok := seen[key]; ok {
			// 只有剧集需要累计集数；电影一行就是一部，不加。
			if m.Kind == "series" {
				out[i].Episodes++
			}
			continue
		}
		if len(out) >= limit {
			continue
		}
		name := m.MetaTitle
		if name == "" {
			name = m.Title
		}
		year := m.MetaYear
		if year == "" {
			year = m.Year
		}
		seen[key] = len(out)
		episodes := 0
		if m.Kind == "series" {
			episodes = 1
		}
		out = append(out, dashRecentItem{
			ID: m.ID, LibraryID: m.LibraryID, Library: libName[m.LibraryID],
			Name: name, Year: year, Type: m.Kind, Rating: m.Rating,
			Poster: posterURL(m, libRoot[m.LibraryID]), Added: formatTime(m.UpdatedAt),
			RelPath: m.RelPath, Episodes: episodes,
		})
	}
	return out
}

// posterURL 返回这张卡片该用的海报地址。
//
// 只有「磁盘上真有那张图」时才给地址：台账里的 poster_file 可能是上一轮
// 写下的、后来被人删了，直接拼 URL 会让墙上一排碎图。剧集还要按剧根兜底
// （见 handleMediaImage 里的同名逻辑），因为落库的是单集行，图却在剧根。
func posterURL(m store.Media, root string) string {
	if root == "" {
		return ""
	}
	rel := m.PosterFile
	if rel == "" && m.Kind == "series" {
		if show := scrape.ShowRoot(m.RelPath); show != "" {
			rel = path.Join(show, "poster.jpg")
		}
	}
	if rel == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
		return ""
	}
	return fmt.Sprintf("/api/media/%d/image", m.ID)
}
