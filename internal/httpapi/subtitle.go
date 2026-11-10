package httpapi

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/notify"
	"strm115/internal/scrape"
	"strm115/internal/store"
	"strm115/internal/subtitle"
)

// SubtitleProgress 是「整库补字幕」任务的进度。
type SubtitleProgress struct {
	LibraryID  int64  `json:"library_id"`
	Phase      string `json:"phase"` // run / done / error / canceled
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Saved      int    `json:"saved"`
	Translated int    `json:"translated"`
	Missing    int    `json:"missing"`
	// Mismatch 是「有字幕但跟这部片对不上」的条数。和 failed 分开：
	// 前者是字幕站上只有同名另一部片的字幕，后者才是我们真的出错了。
	Mismatch   int        `json:"mismatch"`
	Failed     int        `json:"failed"`
	Current    string     `json:"current"`
	Running    bool       `json:"running"`
	Message    string     `json:"message"`
	Err        string     `json:"err"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

type subtitleJob struct {
	mu   sync.Mutex
	p    SubtitleProgress
	cnl  context.CancelFunc
	done chan struct{}
}

func (j *subtitleJob) update(fn func(*SubtitleProgress)) {
	j.mu.Lock()
	fn(&j.p)
	j.mu.Unlock()
}

func (j *subtitleJob) snapshot() SubtitleProgress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p
}

func (j *subtitleJob) Cancel() { j.cnl() }

// handleLibrarySubtitles 给整个库补中文字幕（后台任务）。
func (s *Server) handleLibrarySubtitles(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	lib, err := s.st.GetLibrary(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}

	var req struct {
		// Redo 为 true 时连已经有字幕的也重做一遍。
		Redo bool `json:"redo"`
	}
	_ = readJSON(r, &req)

	items, err := s.st.ListMediaForSubtitle(ctx, lib.ID, req.Redo)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if len(items) == 0 {
		writeErr(w, http.StatusBadRequest,
			errors.New("没有需要补字幕的条目（先刮削一次，或勾选「重做全部」）"))
		return
	}

	job, err := s.startSubtitleJob(lib, items, req.Redo)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusAccepted, job.snapshot())
}

// startSubtitleJob 起一个后台任务。已经有一个在跑时返回错误。
func (s *Server) startSubtitleJob(lib store.Library, items []store.Media, redo bool) (*subtitleJob, error) {
	s.subMu.Lock()
	if old, ok := s.subJobs[lib.ID]; ok {
		if p := old.snapshot(); p.Running {
			s.subMu.Unlock()
			return nil, errors.New("这个库正在补字幕")
		}
	}
	jobCtx, cancel := context.WithCancel(context.Background())
	job := &subtitleJob{cnl: cancel, done: make(chan struct{})}
	job.p = SubtitleProgress{
		LibraryID: lib.ID,
		Phase:     "run",
		Total:     len(items),
		Running:   true,
		StartedAt: time.Now(),
		Message:   "准备补字幕",
	}
	s.subJobs[lib.ID] = job
	s.subMu.Unlock()

	go func() {
		defer close(job.done)
		defer cancel()
		s.runSubtitleJob(jobCtx, job, lib, items, redo)
	}()
	return job, nil
}

func (s *Server) handleSubtitleJob(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	s.subMu.Lock()
	job := s.subJobs[id]
	s.subMu.Unlock()
	if job == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "phase": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, job.snapshot())
}

func (s *Server) runSubtitleJob(ctx context.Context, job *subtitleJob, lib store.Library,
	items []store.Media, redo bool) {
	cfg := s.loadSubtitleConfig(ctx)
	fetcher := subtitle.NewFetcher(s.st, s.log)
	// 只有真的写盘了的条目才要通知 Emby。跳过 / 失败 / 没字幕的一律不刷。
	var changed []subtitleChange

	var lastErr error
	for idx, m := range items {
		if err := ctx.Err(); err != nil {
			job.update(func(p *SubtitleProgress) {
				p.Running = false
				p.Phase = "canceled"
				p.Message = "已取消"
				now := time.Now()
				p.FinishedAt = &now
			})
			return
		}

		// 条目之间留一点间隔。字幕列表来自公共接口，整库几十上百条
		// 连着打过去，轻则被限流、重则整段 403；多花几十秒换稳定。
		if idx > 0 {
			select {
			case <-ctx.Done():
			case <-time.After(350 * time.Millisecond):
			}
		}

		job.update(func(p *SubtitleProgress) { p.Current = m.RelPath })

		res, err := fetcher.Fetch(ctx, s.libRoot(lib), subtitle.Target{
			RelPath:   m.RelPath,
			VideoName: path.Base(m.RelPath),
			IMDbID:    m.IMDbID,
			Kind:      m.Kind,
			Season:    m.Season,
			Episode:   m.Episode,
		}, subtitle.Options{
			Translate: cfg.Translate,
			Bilingual: cfg.Bilingual,
			// 「重做全部字幕」必须真的覆盖磁盘上那份。以前这里写死 false，
			// redo 只放宽了筛选，结果每条都被 findExistingChinese 跳过，
			// 进度条显示「新增 N」而文件一个字节都没变。
			Overwrite:  redo,
			APIBase:    cfg.APIBase,
			Model:      cfg.Model,
			Workers:    cfg.Workers,
			SaveSource: false,
			// 片长用来挡住「同名另一部片」的字幕：字幕站会把它们挂在同一个
			// IMDb 下，光看片源名分不出来（见 subtitle.fitsRuntime 的注释）。
			RuntimeMin: runtimeMinutes(m.Runtime),
		})

		switch {
		case errors.Is(err, subtitle.ErrNoSubtitle):
			m.SubState = "missing"
			job.update(func(p *SubtitleProgress) { p.Missing++ })
		case errors.Is(err, subtitle.ErrMismatch):
			// 有字幕，但都是别的片的：单独报出来，别混进「失败」里 ——
			// 这类条目用户还能自己去别处找字幕，不是我们挂了。
			m.SubState = "mismatch"
			job.update(func(p *SubtitleProgress) { p.Mismatch++ })
			s.log.Warn("字幕与片源对不上", "rel", m.RelPath, "err", err)
		case err != nil:
			m.SubState = "failed"
			lastErr = err
			job.update(func(p *SubtitleProgress) { p.Failed++ })
			s.log.Warn("补字幕失败", "rel", m.RelPath, "err", err)
		default:
			m.SubFile = res.Saved
			m.SubLang = "zh"
			m.SubOrigin = res.Origin
			m.SubState = "ok"
			if !res.Skipped && strings.TrimSpace(res.Saved) != "" {
				changed = append(changed, subtitleChange{RelPath: m.RelPath, SubFile: res.Saved})
			}
			job.update(func(p *SubtitleProgress) {
				p.Saved++
				if res.Translated {
					p.Translated++
				}
			})
		}
		// 字幕状态落在 media_meta 上，重进页面能看到每条的结果。
		// UpdatedAt 清零让 UpsertMedia 写当前时间，否则「最近入库」里
		// 永远看不到刚补完字幕的片。
		m.UpdatedAt = time.Time{}
		if err := s.st.UpsertMedia(context.Background(), m); err != nil {
			s.log.Warn("更新字幕状态失败", "rel", m.RelPath, "err", err)
		}

		job.update(func(p *SubtitleProgress) { p.Done++ })
	}

	job.update(func(p *SubtitleProgress) {
		p.Running = false
		p.Current = ""
		now := time.Now()
		p.FinishedAt = &now
		if lastErr != nil {
			p.Phase = "done"
			p.Message = "部分完成"
		} else {
			p.Phase = "done"
			p.Message = "字幕补齐完成"
		}
	})

	// 写盘 ≠ Emby 看得到。整库任务收尾时统一通知一次媒体服务器，
	// 逐条刷会把 Emby 打疼（一轮几十条，每条都要列条目 + FullRefresh）。
	if len(changed) > 0 {
		go s.refreshSubtitleServer(lib, changed)
	}

	// 一条汇总就够了：整库补字幕是几十条的事，逐条推会把聊天窗口刷穿。
	if p := job.snapshot(); p.Saved+p.Missing+p.Failed+p.Mismatch > 0 {
		line := fmt.Sprintf("新增 %d · 无源 %d · 失效 %d · 失败 %d",
			p.Saved, p.Missing, p.Mismatch, p.Failed)
		if p.Translated > 0 {
			line += fmt.Sprintf(" · 其中翻译 %d", p.Translated)
		}
		s.pushEvent("subtitle_ok", notify.KindIngest, "💬 字幕补齐 · "+lib.Name, line)
	}
}

// subtitleChange 是一条「确实写盘了」的字幕。
type subtitleChange struct {
	RelPath string // .strm 相对库根的路径
	SubFile string // .zh.srt 相对库根的路径
}

// refreshSubtitleServer 让 Emby 重读刚改过的字幕。
//
// 这是「字幕重下了但 Emby 没更新」的关键一步：字幕链路以前只写盘、
// 不通知，而 Emby 对已入库条目的外挂字幕流只认首次扫描建立的索引，
// 覆盖 .srt 并不会让它自己重探。刮削链路早就有这套刷新，字幕侧漏了。
//
// 发两种通知，缺一不可：
//   - MediaUpdated 传真实文件路径，Emby 自己决定扫哪个库；
//   - RefreshPaths 做条目级 FullRefresh，重读同目录的 nfo / 图片 / 字幕流。
//
// 没配媒体服务器、或用户在字幕配置里关掉刷新时直接跳过，不算错误。
func (s *Server) refreshSubtitleServer(lib store.Library, changed []subtitleChange) {
	if len(changed) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if !s.loadSubtitleConfig(ctx).RefreshServer {
		return
	}
	srv, err := s.mediaServer(ctx)
	if err != nil {
		s.log.Info("字幕已写盘；没配媒体服务器，跳过刷新", "lib", lib.Name)
		return
	}
	// 刚写完就刷，Emby 可能还读到旧字节；2 秒是刮削链路用过的实测值。
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}

	root := s.libRoot(lib)
	files := make([]string, 0, len(changed)*2)
	dirs := make([]string, 0, len(changed))
	seenFile := map[string]bool{}
	seenDir := map[string]bool{}
	for _, c := range changed {
		video := filepath.Join(root, filepath.FromSlash(c.RelPath))
		sub := filepath.Join(root, filepath.FromSlash(c.SubFile))
		for _, p := range []string{video, sub} {
			sp := s.mapServerPath(ctx, p)
			if sp != "" && !seenFile[sp] {
				seenFile[sp] = true
				files = append(files, sp)
			}
		}
		sd := s.mapServerPath(ctx, filepath.Dir(video))
		if sd != "" && !seenDir[sd] {
			seenDir[sd] = true
			dirs = append(dirs, sd)
		}
	}
	if err := srv.MediaUpdated(ctx, files); err != nil {
		// 老版本 Emby 可能没有这个接口；后面的条目刷新仍然要试。
		s.log.Warn("通知媒体服务器文件变更失败", "lib", lib.Name, "err", err)
	}
	n, err := srv.RefreshPaths(ctx, dirs)
	if err != nil {
		s.log.Warn("刷新字幕对应的媒体条目失败", "lib", lib.Name, "err", err)
		return
	}
	s.log.Info("已让媒体服务器重读字幕", "lib", lib.Name, "文件", len(files), "目录", len(dirs), "条目", n)
}

// handleFetchOneSubtitle 给单条视频补字幕（同步执行，用户点了等几秒）。
func (s *Server) handleFetchOneSubtitle(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	m, ok, err := s.st.GetMediaByID(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("还没有刮削记录"))
		return
	}
	if m.IMDbID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("这条没有 IMDb ID，无法查字幕"))
		return
	}
	lib, err := s.st.GetLibrary(ctx, m.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}

	var req struct {
		Overwrite bool `json:"overwrite"`
	}
	_ = readJSON(r, &req)

	res, err := s.fetchSubtitleFor(ctx, lib, &m, req.Overwrite)
	if err != nil {
		writeErr(w, statusOfSubtitle(err), err)
		return
	}
	written := !res.Skipped && strings.TrimSpace(res.Saved) != ""
	if written {
		go s.refreshSubtitleServer(lib,
			[]subtitleChange{{RelPath: m.RelPath, SubFile: res.Saved}})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"saved":          res.Saved,
		"origin":         res.Origin,
		"translated":     res.Translated,
		"cues":           res.Cues,
		"note":           res.Note,
		"skipped":        res.Skipped,
		"overwritten":    req.Overwrite && written,
		"server_refresh": written && s.loadSubtitleConfig(ctx).RefreshServer,
	})
}

// fetchSubtitleFor 给一条媒体补中文字幕，并把状态写回台账。
//
// 「单条补字幕」和「按 ID 重刮后顺手补字幕」共用这一份：sub_file /
// sub_state 只有一处写入逻辑，界面上的「有中字 / 无中字 / 失败」才不会前后矛盾。
func (s *Server) fetchSubtitleFor(ctx context.Context, lib store.Library,
	m *store.Media, overwrite bool) (*subtitle.Result, error) {

	cfg := s.loadSubtitleConfig(ctx)
	fetcher := subtitle.NewFetcher(s.st, s.log)
	res, err := fetcher.Fetch(ctx, s.libRoot(lib), subtitle.Target{
		RelPath:   m.RelPath,
		VideoName: path.Base(m.RelPath),
		IMDbID:    m.IMDbID,
		Kind:      m.Kind,
		Season:    m.Season,
		Episode:   m.Episode,
	}, subtitle.Options{
		Translate:  cfg.Translate,
		Bilingual:  cfg.Bilingual,
		Overwrite:  overwrite,
		APIBase:    cfg.APIBase,
		Model:      cfg.Model,
		Workers:    cfg.Workers,
		SaveSource: false,
		RuntimeMin: runtimeMinutes(m.Runtime),
	})
	if err != nil {
		m.SubState = "failed"
		if errors.Is(err, subtitle.ErrNoSubtitle) {
			m.SubState = "missing"
		}
		if errors.Is(err, subtitle.ErrMismatch) {
			m.SubState = "mismatch"
		}
		m.UpdatedAt = time.Time{}
		if upErr := s.st.UpsertMedia(ctx, *m); upErr != nil {
			s.log.Warn("更新字幕状态失败", "rel", m.RelPath, "err", upErr)
		}
		return nil, err
	}

	m.SubFile = res.Saved
	m.SubLang = "zh"
	m.SubOrigin = res.Origin
	m.SubState = "ok"
	m.UpdatedAt = time.Time{} // 让台账的 updated_at 真的跟着字幕变
	if err := s.st.UpsertMedia(ctx, *m); err != nil {
		// 字幕已经落盘了，台账写不进去只是状态显示的问题，不该报成「补字幕失败」。
		s.log.Warn("更新字幕状态失败", "rel", m.RelPath, "err", err)
	}
	return res, nil
}

func statusOfSubtitle(err error) int {
	if errors.Is(err, subtitle.ErrNoSubtitle) || errors.Is(err, subtitle.ErrMismatch) {
		return http.StatusNotFound
	}
	msg := err.Error()
	if strings.Contains(msg, "未开启自动翻译") {
		return http.StatusUnprocessableEntity
	}
	return http.StatusBadGateway
}

// handleMediaImage 把本地已下载的海报返给管理后台预览。
//
// 为什么不直接挂一个静态目录：媒体库根目录里有用户的真实文件，
// 挂出去等于把整个目录暴露在登录态之下。这里按 media id 反查路径，
// 只可能读到我们自己写的那两个图片文件。
func (s *Server) handleMediaImage(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	m, ok, err := s.st.GetMediaByID(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		http.NotFound(w, r)
		return
	}
	rel := m.PosterFile
	if strings.EqualFold(r.URL.Query().Get("kind"), "backdrop") {
		rel = m.BackdropFile
	}
	// 剧集的海报写在「剧根目录」，而台账里每一行是一集，poster_file 只在
	// 写图那一刻的那个对象上有值，落库的行里是空的。这里按剧根兜一次底，
	// 否则剧集在墙上一律是灰块。只认我们自己写的那两个固定文件名。
	if strings.TrimSpace(rel) == "" && m.Kind == "series" {
		show := scrape.ShowRoot(m.RelPath)
		if show != "" {
			name := "poster.jpg"
			if strings.EqualFold(r.URL.Query().Get("kind"), "backdrop") {
				name = "fanart.jpg"
			}
			rel = path.Join(show, name)
		}
	}
	if strings.TrimSpace(rel) == "" {
		http.NotFound(w, r)
		return
	}
	lib, err := s.st.GetLibrary(ctx, m.LibraryID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	root := filepath.Clean(s.libRoot(lib))
	full := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	// Join 会吃掉 ".."，所以前缀检查足以挡住路径穿越
	if !strings.HasPrefix(full, root+string(filepath.Separator)) {
		writeErr(w, http.StatusForbidden, errors.New("非法路径"))
		return
	}
	if st, err := os.Stat(full); err != nil || st.IsDir() {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=86400")
	http.ServeFile(w, r, full)
}

// libRoot 返回媒体库在磁盘上的真实根目录。
//
// 库的 local_path 可能是从容器里带出来的 `/media/movies`，在 Windows 上
// 那不是绝对路径，直接拼会落到 D://media//movies。统一走 scrape.ResolveRoot，
// 保证「同步写 .strm 的地方」和「刮削写字幕/海报的地方」是同一个目录。
func (s *Server) libRoot(lib store.Library) string {
	return scrape.ResolveRoot(lib.LocalPath, s.cfg.MediaRoot)
}

// runtimeMinutes 把台账里的片长字符串解析成分钟。
//
// TMDB 给的是 "173 min"，但老库/手工改过的行可能是 "2h 53m"、"173" 或空。
// 解析不出来就返回 0 —— 字幕那边看到 0 会跳过时长校验，不会因此补不上字幕。
//
// 不用 fmt.Sscanf：`Sscanf(s, "%dh %dm", ...)` 在 "173 min" 上会「部分匹配」
// 成功并先把 173 写进小时位，于是 173 分钟变成 173 小时（10380 分钟），
// 校验就永远不通过、一集字幕都补不上。
func runtimeMinutes(s string) int {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return 0
	}
	// "2h 53m" / "1h"：只要出现 h，前半段就是小时
	if i := strings.IndexByte(s, 'h'); i > 0 {
		if h, err := strconv.Atoi(strings.TrimSpace(s[:i])); err == nil {
			total := h * 60
			rest := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s[i+1:]), "m"))
			if rest != "" {
				if m, err := strconv.Atoi(rest); err == nil {
					total += m
				}
			}
			return total
		}
	}
	// "173 min" / "173"
	if f := strings.Fields(s); len(f) > 0 {
		if n, err := strconv.Atoi(f[0]); err == nil {
			return n
		}
	}
	return 0
}
