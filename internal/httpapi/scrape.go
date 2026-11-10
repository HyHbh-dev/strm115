package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"strm115/internal/scrape"
	"strm115/internal/store"
)

const (
	settingScrapeConfig   = "scrape_config"
	settingSubtitleConfig = "subtitle_config"
)

// scrapeConfig 是刮削的偏好设置。单独存一行 JSON 而不是拆成一堆键：
// 这些参数总是一起改，拆开会让「恢复默认」变成十几条删除。
type scrapeConfig struct {
	WriteNFO    bool `json:"write_nfo"`
	FetchImages bool `json:"fetch_images"`
	MinSizeMB   int  `json:"min_size_mb"`
	RateMS      int  `json:"rate_ms"`
	// Auto 表示同步完成后自动刮削。
	//
	// 默认开着：元数据本身有本地缓存，同一部片只会真正请求 IMDb 一次，
	// 之后每次同步都是纯本地命中。关掉它等于让用户每次同步完手动点一下。
	Auto bool `json:"auto"`
	// Language 元数据语言："zh"（默认，中文优先）或 "original"（保持英文）。
	Language string `json:"language"`
	// Translate 表示把英文简介/标题翻成中文。
	Translate bool `json:"translate"`
	// RefreshServer 表示刮削完成后让 Emby 重新读一遍 nfo。
	//
	// 必须要做这一步：Emby 只在「文件有变化」时才重读元数据，
	// 而我们改的是同目录下的 .nfo，不加这一步，界面上永远是旧信息。
	RefreshServer bool `json:"refresh_server"`
}

func defaultScrapeConfig() scrapeConfig {
	return scrapeConfig{
		WriteNFO:      true,
		FetchImages:   true,
		MinSizeMB:     50,
		RateMS:        1200,
		Auto:          true,
		Language:      "zh",
		Translate:     true,
		RefreshServer: true,
	}
}

// subtitleConfig 是字幕下载与翻译的偏好。
type subtitleConfig struct {
	// Auto 表示刮削完成后自动补中文字幕。
	Auto bool `json:"auto"`
	// Translate 表示没有中文字幕时，把英文字幕翻译成中文。
	Translate bool `json:"translate"`
	// Bilingual 输出双语字幕。
	Bilingual bool `json:"bilingual"`
	// APIBase / Model 是翻译服务地址与模型。
	APIBase string `json:"api_base"`
	Model   string `json:"model"`
	// Workers 翻译并发。
	Workers int `json:"workers"`
	// RefreshServer 表示字幕写盘后通知 Emby 重读。
	//
	// 默认开：Emby 只在建库那次把外挂字幕登记进 MediaStreams，之后覆盖
	// .srt 不会自己重探。不通知的话界面上永远还是第一次那份字幕。
	RefreshServer bool `json:"refresh_server"`
}

func defaultSubtitleConfig() subtitleConfig {
	return subtitleConfig{
		Auto:          true,
		Translate:     true,
		APIBase:       "https://index-translate.bilibili.com/v1",
		Model:         "Index-Translate-35B-A3B",
		Workers:       2,
		RefreshServer: true,
	}
}

func (s *Server) loadScrapeConfig(ctx context.Context) scrapeConfig {
	cfg := defaultScrapeConfig()
	raw, ok := s.st.GetSetting(ctx, settingScrapeConfig)
	if !ok || strings.TrimSpace(raw) == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		s.log.Warn("刮削设置解析失败，用默认值", "err", err)
		return defaultScrapeConfig()
	}
	return cfg
}

func (s *Server) loadSubtitleConfig(ctx context.Context) subtitleConfig {
	cfg := defaultSubtitleConfig()
	raw, ok := s.st.GetSetting(ctx, settingSubtitleConfig)
	if !ok || strings.TrimSpace(raw) == "" {
		return cfg
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		s.log.Warn("字幕设置解析失败，用默认值", "err", err)
		return defaultSubtitleConfig()
	}
	if cfg.Workers <= 0 || cfg.Workers > 8 {
		cfg.Workers = 2
	}
	if strings.TrimSpace(cfg.APIBase) == "" {
		cfg.APIBase = defaultSubtitleConfig().APIBase
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = defaultSubtitleConfig().Model
	}
	return cfg
}

func (s *Server) scrapeOptions(ctx context.Context, overwrite, refreshCache bool) scrape.Options {
	cfg := s.loadScrapeConfig(ctx)
	sb := s.loadSubtitleConfig(ctx)
	opt := scrape.DefaultOptions()
	opt.WriteNFO = cfg.WriteNFO
	opt.FetchImages = cfg.FetchImages
	opt.Overwrite = overwrite
	opt.RefreshCache = refreshCache
	opt.Language = strings.TrimSpace(cfg.Language)
	if opt.Language == "" {
		opt.Language = "zh"
	}
	// 翻译与字幕共用一套接口配置：都是同一个 Index-Translate 服务，
	// 分成两处配置只会出现「字幕能翻、简介不能」的诡异状态。
	opt.Translate = cfg.Translate
	opt.APIBase = sb.APIBase
	opt.Model = sb.Model
	opt.Workers = sb.Workers
	if cfg.MinSizeMB > 0 {
		opt.MinSize = int64(cfg.MinSizeMB) << 20
	} else {
		opt.MinSize = 0
	}
	if cfg.RateMS > 0 {
		opt.Rate = time.Duration(cfg.RateMS) * time.Millisecond
	}
	return opt
}

// ==================== 刮削 ====================

// refreshServerAfterScrape 等刮削跑完，让媒体服务器重新读一遍本地 nfo。
//
// 单独一个 goroutine 是刻意的：Emby 的条目刷新是「一次 HTTP 一次解析」，
// 几百部要几十秒，不该让刮削任务本身停在 done 之前。
func (s *Server) refreshServerAfterScrape(job *scrape.Job, lib store.Library) {
	<-job.Done()
	if job.Snapshot().Phase != "done" {
		return
	}
	if !s.loadScrapeConfig(context.Background()).RefreshServer {
		return
	}
	s.refreshServerMetadata(lib)
}

// refreshServerMetadata 先整库扫描、再逐条刷新。
//
// 两步缺一不可：扫描把新同步进来的 .strm 变成条目（这一步只认新增/删除），
// 条目刷新才会重读改动过的 nfo。反过来只做扫描的话，老的条目永远是
// 「文件名当标题、简介空白」——这就是「Emby 里没有影视信息」的成因。
func (s *Server) refreshServerMetadata(lib store.Library) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	srv, err := s.mediaServer(ctx)
	if err != nil {
		return // 没配媒体服务器，不是错误
	}

	if err := srv.Refresh(ctx, lib.ServerLibID); err != nil {
		s.log.Warn("触发媒体库扫描失败", "lib", lib.Name, "err", err)
	}
	// 扫描是异步的：立刻刷条目会打在扫描前的旧条目上，白跑一趟。
	// 4 秒是实测值——23 个文件的库扫描在 1~2 秒内结束。
	select {
	case <-ctx.Done():
		return
	case <-time.After(4 * time.Second):
	}

	items, err := s.st.ListMediaAll(ctx, lib.ID)
	if err != nil || len(items) == 0 {
		return
	}
	root := s.libRoot(lib)
	seen := make(map[string]bool, len(items))
	paths := make([]string, 0, len(items))
	add := func(dir string) {
		sp := s.mapServerPath(ctx, filepath.FromSlash(dir))
		if sp == "" || seen[sp] {
			return
		}
		seen[sp] = true
		paths = append(paths, sp)
	}
	for _, it := range items {
		add(path.Dir(filepath.ToSlash(filepath.Join(root, filepath.FromSlash(it.RelPath)))))
		// 剧集还要刷新「剧根目录」。
		//
		// 剧的标题 / 海报 / 演员都在 tvshow.nfo 里，而 tvshow.nfo 位于剧根；
		// 只刷「Season 1」的话，Emby 刷的是每一集，Series 条目本身不动 ——
		// 表现就是：单集有中文演员名，剧的详情页还是旧的英文名和灰头像。
		if root := scrape.ShowRoot(it.RelPath); root != "" {
			add(filepath.ToSlash(filepath.Join(s.libRoot(lib), filepath.FromSlash(root))))
		}
	}

	n, err := srv.RefreshPaths(ctx, paths)
	if err != nil {
		s.log.Warn("刷新媒体条目失败", "lib", lib.Name, "err", err)
		return
	}
	s.log.Info("已让媒体服务器重读元数据", "lib", lib.Name, "条目", n, "目录", len(paths))
}

func (s *Server) handleStartScrape(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	lib, err := s.st.GetLibrary(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	// 必须先同步过才有台账可刮：刮削只读台账，不碰网盘。
	if n, _ := s.st.CountFiles(ctx, lib.ID); n == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("这个库还没有同步记录，请先同步一次再刮削"))
		return
	}

	var req struct {
		Overwrite    bool `json:"overwrite"`
		RefreshCache bool `json:"refresh_cache"`
	}
	_ = readJSON(r, &req)

	job, err := s.startScrape(lib, s.scrapeOptions(ctx, req.Overwrite, req.RefreshCache))
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	go s.refreshServerAfterScrape(job, lib)
	s.chainSubtitlesAfterScrape(lib, job)
	writeJSON(w, http.StatusAccepted, job.Snapshot())
}

// chainSubtitlesAfterScrape 等整库刮削跑完，按设置自动补一轮中文字幕。
//
// 串在刮削之后而不是并行：字幕查询要 IMDb ID，而 ID 是刮削的产物。
// 没开启自动补字幕时立刻返回，不占任何资源。
func (s *Server) chainSubtitlesAfterScrape(lib store.Library, job *scrape.Job) {
	if !s.loadSubtitleConfig(context.Background()).Auto {
		return
	}
	go func() {
		<-job.Done()
		if job.Snapshot().Phase != "done" {
			return
		}
		items, err := s.st.ListMediaForSubtitle(context.Background(), lib.ID, false)
		if err != nil || len(items) == 0 {
			return
		}
		if _, err := s.startSubtitleJob(lib, items, false); err != nil {
			s.log.Debug("自动补字幕没启动", "lib", lib.Name, "err", err)
		}
	}()
}

// handleRescrapeMedia 用一条明确的 IMDb ID 重刮单个片目。
//
// 这是「自动匹配错了」的兜底：用户填 ID，后端按 ID 直查一次 meta，
// 就地改写 .strm 旁边的 nfo / 海报，并可选顺手补一条中文字幕。
func (s *Server) handleRescrapeMedia(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	m, ok, err := s.st.GetMediaByID(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("还没有刮削记录，先整库刮一次"))
		return
	}
	lib, err := s.st.GetLibrary(ctx, m.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}

	var req struct {
		IMDbID string `json:"imdb_id"`
		// 指针是为了区分「没传」（默认覆盖）和「显式 false」。
		Overwrite *bool `json:"overwrite"`
		Subtitles *bool `json:"subtitles"`
	}
	_ = readJSON(r, &req)

	imdbID := scrape.NormalizeIMDbID(req.IMDbID)
	if imdbID == "" {
		imdbID = scrape.NormalizeIMDbID(m.IMDbID)
	}
	if imdbID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请填一个 IMDb ID，形如 tt15239678"))
		return
	}

	overwrite := true
	if req.Overwrite != nil {
		overwrite = *req.Overwrite
	}
	// 覆盖模式下连本地元数据缓存一起刷新：用户点「重新刮削」就是想要最新的。
	opt := s.scrapeOptions(ctx, overwrite, overwrite)

	out, err := s.scrape.RescrapeOne(ctx, lib, m, imdbID, opt)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	resp := map[string]any{"ok": true, "media": out}
	var subWritten bool
	var subSaved string

	wantSubs := true
	if req.Subtitles != nil {
		wantSubs = *req.Subtitles
	}
	if wantSubs {
		mm := out
		if res, err := s.fetchSubtitleFor(ctx, lib, &mm, overwrite); err != nil {
			// 字幕失败不该把「元数据已写好」这个结果一起吞掉。
			resp["subtitle"] = map[string]any{"ok": false, "error": err.Error()}
		} else {
			resp["subtitle"] = map[string]any{
				"ok": true, "saved": res.Saved, "origin": res.Origin,
				"translated": res.Translated, "skipped": res.Skipped,
			}
			out = mm
			resp["media"] = out
			subWritten = !res.Skipped && strings.TrimSpace(res.Saved) != ""
			subSaved = res.Saved
		}
	}

	// 让 Emby 立刻重读这个目录，不然要等下一次整库扫描才看得到新信息。
	// 字幕也写盘了就走字幕那条：它除了条目刷新，还会把「文件被改过」
	// 直接告诉 Emby，并 bump .strm 的 mtime，两者一起才能真正换掉旧字幕。
	switch {
	case subWritten:
		go s.refreshSubtitleServer(lib,
			[]subtitleChange{{RelPath: out.RelPath, SubFile: subSaved}})
	default:
		go s.refreshMediaDir(lib, out.RelPath)
	}
	writeJSON(w, http.StatusOK, resp)
}

// refreshMediaDir 只刷新一条记录所在目录。
func (s *Server) refreshMediaDir(lib store.Library, relPath string) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	srv, err := s.mediaServer(ctx)
	if err != nil {
		return // 没配媒体服务器不是错误
	}
	dir := filepath.Dir(filepath.Join(s.libRoot(lib), filepath.FromSlash(relPath)))
	// 给 Emby 一点时间落盘：刚写完 nfo 立刻刷，它可能还读到旧内容。
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	if _, err := srv.RefreshPaths(ctx, []string{s.mapServerPath(ctx, dir)}); err != nil {
		s.log.Debug("刷新媒体条目失败", "dir", dir, "err", err)
	}
}

func (s *Server) handleScrapeJob(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	j := s.scrape.Job(id)
	if j == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "phase": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, j.Snapshot())
}

func (s *Server) handleCancelScrape(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if j := s.scrape.Job(id); j != nil {
		j.Cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleListMedia 列刮削结果。
func (s *Server) handleListMedia(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	offset, _ := strconv.Atoi(q.Get("offset"))
	status := strings.TrimSpace(q.Get("status"))
	// `all` 是前端的「不筛选」，不能直接当状态值传给 SQL
	if status == "all" {
		status = ""
	}

	items, total, err := s.st.ListMedia(ctx, id, status, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats, err := s.st.MediaStats(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	cached, _ := s.st.CountMetaCache(ctx)
	if items == nil {
		items = []store.Media{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"total":       total,
		"stats":       stats,
		"meta_cached": cached,
	})
}
