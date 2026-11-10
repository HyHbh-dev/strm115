package httpapi

// 任务中心的数据源。
//
// 这一页以前整页是 mock：任务列表写死在 mock.ts 里、进度条是随机数、
// 「暂停 / 取消」只弹一个 toast、「Emby 探测」的两行失败记录纯属虚构。
// 现在每一行都必须对应一个真的在跑的东西：
//
//	同步   → sync.Job（每个库一个）
//	刮削   → scrape.Job
//	字幕   → subtitleJob（挂在 Server 上的 map）
//	历史   → sync_runs 流水账（重启不丢）
//
// 为什么只有同步有「历史」：sync_runs 是唯一真的落库的任务流水。刮削和
// 字幕跑完就只剩结果（media_meta / 字幕文件），没有一条「这轮跑了多久、
// 成功几个」的记录。为了这一页去新造一张表，等于给三种任务各写一遍写库
// 逻辑；先让历史只覆盖同步，是诚实的做法 —— 页面上会明说这一点。
//
// 取消是按 (kind, library_id) 定位的：任务 id 在内存里本来就以库为键，
// 前端拿到的是同一个键，不必再维护一套 id 映射。

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
)

// taskRow 是「进行中」列表的一行。
type taskRow struct {
	// ID 是 kind:libraryID，前端拿它做 key，也拿它回来取消。
	ID        string `json:"id"`
	Kind      string `json:"kind"`  // sync / scrape / subtitle
	Label     string `json:"label"` // 同步 / 刮削 / 补字幕
	LibraryID int64  `json:"library_id"`
	Library   string `json:"library"`
	Phase     string `json:"phase"`
	Done      int    `json:"done"`
	Total     int    `json:"total"`
	Percent   int    `json:"percent"` // -1 表示总量未知
	Message   string `json:"message"`
	StartedAt string `json:"started_at"`
	// Detail 是各类任务各自的细项，前端按 kind 挑着显示。
	Detail map[string]int `json:"detail,omitempty"`
	// Cancelable 为 false 时前端不该显示「取消」——
	// 一个点了没反应的按钮比没有按钮更糟。
	Cancelable bool `json:"cancelable"`
}

type taskSummary struct {
	Running int `json:"running"`
	// OfflineWatching 是还在轮询的离线任务数（下完会自动同步那批）。
	OfflineWatching int    `json:"offline_watching"`
	HistoryTotal    int    `json:"history_total"`
	Failed24h       int    `json:"failed_24h"`
	LastError       string `json:"last_error,omitempty"`
}

// handleTasks 返回「进行中」+ 汇总 + 可选的库清单。
func (s *Server) handleTasks(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	names := make(map[int64]string, len(libs))
	libOut := make([]map[string]any, 0, len(libs))
	for _, l := range libs {
		names[l.ID] = l.Name
		libOut = append(libOut, map[string]any{
			"id": l.ID, "name": l.Name, "collection_type": l.CollectionType,
			"last_sync_at": formatTime(l.LastSyncAt),
		})
	}

	running := s.runningTasks(libs, names)
	total, _ := s.st.CountSyncRuns(ctx)
	summary := taskSummary{
		Running:         len(running),
		OfflineWatching: s.offline.Watching(),
		HistoryTotal:    total,
	}
	// 最近 24 小时的失败轮次：这一项的回答是「有没有东西一直在坏」，
	// 比历史里翻十行更有用。
	recent, _ := s.st.ListSyncRuns(ctx, 200)
	cut := time.Now().Add(-24 * time.Hour)
	for _, run := range recent {
		if run.Phase == "error" && run.StartedAt.After(cut) {
			summary.Failed24h++
			if summary.LastError == "" {
				summary.LastError = run.LibraryName + "：" + run.Message
			}
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"running":      running,
		"summary":      summary,
		"libraries":    libOut,
		"generated_at": formatTime(time.Now()),
	})
}

// runningTasks 把三类内存里的任务摊平成同一个列表。
func (s *Server) runningTasks(libs []store.Library, names map[int64]string) []taskRow {

	out := make([]taskRow, 0, 4)
	// 进度百分比留给前端算：同步的总数要边走边发现，这里给 -1 表示未知。
	seen := map[string]bool{}

	for _, l := range libs {
		if j := s.sync.Job(l.ID); j != nil {
			p := j.Snapshot()
			if !p.Running {
				continue
			}
			id := taskID("sync", l.ID)
			seen[id] = true
			out = append(out, taskRow{
				ID: id, Kind: "sync", Label: "同步", LibraryID: l.ID, Library: l.Name,
				Phase: p.Phase, Done: p.Written, Total: p.Files, Percent: -1,
				Message: p.Message, StartedAt: formatTime(p.StartedAt), Cancelable: true,
				Detail: map[string]int{
					"dirs": p.Dirs, "files": p.Files, "written": p.Written,
					"removed": p.Removed, "failed": p.Failed,
				},
			})
		}
		if j := s.scrape.Job(l.ID); j != nil {
			p := j.Snapshot()
			if !p.Running {
				continue
			}
			id := taskID("scrape", l.ID)
			seen[id] = true
			out = append(out, taskRow{
				ID: id, Kind: "scrape", Label: "刮削", LibraryID: l.ID, Library: l.Name,
				Phase: p.Phase, Done: p.Done, Total: p.Total, Percent: ratio(p.Done, p.Total),
				Message: p.Message, StartedAt: formatTime(p.StartedAt), Cancelable: true,
				Detail: map[string]int{
					"matched": p.Matched, "unmatched": p.Unmatched, "failed": p.Failed,
					"fetched": p.Fetched, "cached": p.Cached,
				},
			})
		}
	}

	s.subMu.Lock()
	for id, j := range s.subJobs {
		p := j.snapshot()
		if !p.Running {
			continue
		}
		out = append(out, taskRow{
			ID: taskID("subtitle", id), Kind: "subtitle", Label: "补字幕",
			LibraryID: id, Library: names[id],
			Phase: p.Phase, Done: p.Done, Total: p.Total, Percent: ratio(p.Done, p.Total),
			Message: p.Message, StartedAt: formatTime(p.StartedAt), Cancelable: true,
			Detail: map[string]int{"saved": p.Saved, "failed": p.Failed, "missing": p.Missing},
		})
	}
	s.subMu.Unlock()

	return out
}

func taskID(kind string, libraryID int64) string {
	return kind + ":" + strconv.FormatInt(libraryID, 10)
}

// handleTaskHistory 返回同步流水账。
func (s *Server) handleTaskHistory(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if v, err := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("limit"))); err == nil && v > 0 {
		limit = v
	}
	runs, err := s.st.ListSyncRuns(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	total, _ := s.st.CountSyncRuns(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"items": runs, "total": total})
}

// handleTaskCancel 停掉一个正在跑的任务。
func (s *Server) handleTaskCancel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string `json:"kind"`
		LibraryID int64  `json:"library_id"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	if body.LibraryID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 library_id"))
		return
	}

	switch strings.TrimSpace(body.Kind) {
	case "sync":
		j := s.sync.Job(body.LibraryID)
		if j == nil || !j.Running() {
			writeErr(w, http.StatusConflict, errors.New("这个库没有在同步"))
			return
		}
		j.Cancel()
	case "scrape":
		j := s.scrape.Job(body.LibraryID)
		if j == nil || !j.Running() {
			writeErr(w, http.StatusConflict, errors.New("这个库没有在刮削"))
			return
		}
		j.Cancel()
	case "subtitle":
		s.subMu.Lock()
		j := s.subJobs[body.LibraryID]
		s.subMu.Unlock()
		if j == nil || !j.snapshot().Running {
			writeErr(w, http.StatusConflict, errors.New("这个库没有在补字幕"))
			return
		}
		j.Cancel()
	default:
		writeErr(w, http.StatusBadRequest, errors.New("未知的任务类型"))
		return
	}
	s.log.Info("任务已取消", "kind", body.Kind, "lib", body.LibraryID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleTaskRun 手动起一轮同步或刮削。
//
// 历史里的「重跑」和进行中页的「跑一轮」走同一个接口：它们要的东西一样，
// 分成两个接口只会让「为什么那边不能重跑」变成一个新问题。
func (s *Server) handleTaskRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind      string `json:"kind"` // sync / scrape
		LibraryID int64  `json:"library_id"`
		// Full 为真时同步走全量（刷新所有 .strm 的 mtime，让 Emby 整库重扫）。
		Full bool `json:"full"`
		// Refresh 为真时刮削忽略本地缓存重查（换了数据源之后要用）。
		Refresh bool `json:"refresh"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	kind := strings.TrimSpace(body.Kind)
	// 先判类型再看库：参数写错（kind=subtitle）时应该直接说「不支持」，
	// 而不是先报一个「库不存在」把真正的问题盖住。
	if kind != "sync" && kind != "scrape" {
		writeErr(w, http.StatusBadRequest, errors.New("只支持 sync 和 scrape"))
		return
	}
	ctx := r.Context()
	lib, err := s.st.GetLibrary(ctx, body.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}

	switch kind {
	case "sync":
		opt, err := s.syncOptions(ctx, lib, syncReq{Force: body.Full})
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		opt.Trigger = "manual"
		job, err := s.sync.Start(lib, opt)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		s.watchSyncAndNotify(job, lib)
		writeJSON(w, http.StatusAccepted, job.Snapshot())
	case "scrape":
		opt := s.scrapeOptions(ctx, body.Refresh, body.Refresh)
		job, err := s.startScrape(lib, opt)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		go func() {
			<-job.Done()
			if job.Snapshot().Phase == "done" {
				go s.refreshServerAfterScrape(job, lib)
				s.chainSubtitlesAfterScrape(lib, job)
			}
		}()
		writeJSON(w, http.StatusAccepted, job.Snapshot())
	}
}

// handleTaskClear 清空已结束的历史。
func (s *Server) handleTaskClear(w http.ResponseWriter, r *http.Request) {
	n, err := s.st.ClearSyncRuns(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": n})
}
