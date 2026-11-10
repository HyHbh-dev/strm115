package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
	syncsvc "strm115/internal/sync"
)

// ==================== Strm 管理：总览 ====================

// strmLibStatus 是一个库在「Strm 管理」页上需要的全部信息。
type strmLibStatus struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	AccountID     int64  `json:"account_id"`
	AccountName   string `json:"account_name"`
	RemotePath    string `json:"remote_path"`
	RemoteCID     string `json:"remote_cid"`
	LocalPath     string `json:"local_path"`
	LocalRoot     string `json:"local_root"` // 实际落盘根目录（已按媒体根目录修正过）
	Collection    string `json:"collection_type"`
	KeepExt       bool   `json:"keep_ext"`
	Files         int    `json:"files"` // 台账片目 = 应有的 .strm 数
	LastSyncAt    string `json:"last_sync_at"`
	LastSyncFiles int    `json:"last_sync_files"`
	// Sources 是这个库挂的网盘目录（115 / 夸克各一条）。页面用它显示
	// 「这个库的片从哪几个网盘来」，一条的用户不用关心这个字段。
	Sources []strmSourceStatus `json:"sources,omitempty"`
	// Job 只在「这个库有一轮在跑」或「刚跑完还没重启」时出现。
	Job *syncsvc.Progress `json:"job,omitempty"`
}

type strmSourceStatus struct {
	ID         int64  `json:"id"`
	AccountID  int64  `json:"account_id"`
	Kind       string `json:"kind"`
	Account    string `json:"account"`
	RemotePath string `json:"remote_path"`
	RemoteCID  string `json:"remote_cid"`
	Files      int    `json:"files"`
	LastSyncAt string `json:"last_sync_at"`
}

type strmAccountStatus struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Live          bool   `json:"live"`
	Requests      int64  `json:"requests"`
	Retries       int64  `json:"retries"`
	RiskHits      int64  `json:"risk_hits"`
	CoolDowns     int64  `json:"cooldowns"`
	CooldownUntil string `json:"cooldown_until"`
	LastError     string `json:"last_error"`
}

// handleStrmOverview 一次性返回 Strm 管理页首屏要的东西。
//
// 刻意不在这里遍历本地磁盘：那个开销随库大小线性增长，而首屏是每次进页面
// 都会调的。孤儿扫描是一个显式动作（见 handleStrmOrphans）。
func (s *Server) handleStrmOverview(w http.ResponseWriter, r *http.Request) {
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
	acctName := make(map[int64]string, len(accts))
	outAccts := make([]strmAccountStatus, 0, len(accts))
	for _, a := range accts {
		acctName[a.ID] = a.Name
		snap, live := s.pool.Snapshot(a.ID)
		outAccts = append(outAccts, strmAccountStatus{
			ID: a.ID, Name: a.Name, Kind: a.Kind, Live: live,
			Requests: snap.Requests, Retries: snap.Retries, RiskHits: snap.RiskHits,
			CoolDowns: snap.CoolDowns, CooldownUntil: formatTime(snap.CooldownUntil),
			LastError: snap.LastError,
		})
	}

	libOut := make([]strmLibStatus, 0, len(libs))
	totalFiles := 0
	running := 0
	var lastSync time.Time
	for _, l := range libs {
		n, _ := s.st.CountFiles(ctx, l.ID)
		totalFiles += n
		st := strmLibStatus{
			ID: l.ID, Name: l.Name, AccountID: l.AccountID,
			AccountName: acctName[l.AccountID],
			RemotePath:  l.RemotePath, RemoteCID: l.RemoteCID,
			LocalPath: l.LocalPath, LocalRoot: s.libRoot(l),
			Collection: l.CollectionType, KeepExt: l.KeepExt,
			Files: n, LastSyncAt: formatTime(l.LastSyncAt), LastSyncFiles: l.LastSyncFiles,
		}
		for _, src := range l.Sources {
			st.Sources = append(st.Sources, strmSourceStatus{
				ID: src.ID, AccountID: src.AccountID, Kind: src.AccountKind,
				Account: acctName[src.AccountID], RemotePath: src.RemotePath,
				RemoteCID: src.RemoteCID, Files: src.LastSyncFiles,
				LastSyncAt: formatTime(src.LastSyncAt),
			})
		}
		if j := s.sync.Job(l.ID); j != nil {
			p := j.Snapshot()
			st.Job = &p
			if p.Running {
				running++
			}
		}
		if l.LastSyncAt.After(lastSync) {
			lastSync = l.LastSyncAt
		}
		libOut = append(libOut, st)
	}

	runs, _ := s.st.ListSyncRuns(ctx, 20)
	if runs == nil {
		runs = []store.SyncRun{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"libraries": libOut,
		"accounts":  outAccts,
		"runs":      runs,
		"totals": map[string]any{
			"libraries": len(libOut),
			"files":     totalFiles,
			"running":   running,
		},
		"last_sync_at":   formatTime(lastSync),
		"last_full_sync": formatTime(s.st.LastFullSyncAt(ctx)),
		"domain":         s.setting(ctx, "play_domain"),
	})
}

func (s *Server) handleStrmRuns(w http.ResponseWriter, r *http.Request) {
	limit := atoiOr(r.URL.Query().Get("limit"), 30)
	runs, err := s.st.ListSyncRuns(r.Context(), limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if runs == nil {
		runs = []store.SyncRun{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// ==================== 孤儿扫描 / 清理 ====================

// orphanLib 是一个库的孤儿扫描结果。
//
// 两个方向都要报，因为它们对应的坏法完全不同：
//   - Extra（盘上有、台账没有）：同步中途挂掉留下的残骸，或者有人手动丢了文件；
//   - Missing（台账有、盘上没有）：有人手动删了 .strm，Emby 里那部片就只剩空壳。
type orphanLib struct {
	LibraryID int64    `json:"library_id"`
	Name      string   `json:"name"`
	Root      string   `json:"root"`
	Scanned   int      `json:"scanned"` // 盘上 .strm 总数
	Extra     []string `json:"extra"`
	Missing   []string `json:"missing"`
	Err       string   `json:"err,omitempty"`
}

// listStrmOnDisk 走一遍库根目录，返回相对路径 → 绝对路径。
//
// 用 WalkDir 而不是读目录表：孤儿判定的依据就是「磁盘上真实存在的文件」，
// 任何缓存都会让这个判定失去意义。
func listStrmOnDisk(root string) (map[string]string, int, error) {
	out := make(map[string]string, 1024)
	total := 0
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			// 单个目录没权限不该让整次扫描失败，跳过即可
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if !strings.EqualFold(filepath.Ext(d.Name()), ".strm") {
			return nil
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = p
		total++
		return nil
	})
	return out, total, err
}

// scanOrphans 扫一个库，返回完整结果。limit<=0 表示不截断。
func (s *Server) scanOrphans(ctx context.Context, lib store.Library, limit int) orphanLib {
	res := orphanLib{LibraryID: lib.ID, Name: lib.Name, Root: s.libRoot(lib)}
	root := res.Root
	if root == "" {
		res.Err = "本地路径为空"
		return res
	}
	if _, err := os.Stat(root); err != nil {
		res.Err = "本地目录不存在：" + root
		return res
	}

	onDisk, total, err := listStrmOnDisk(root)
	res.Scanned = total
	if err != nil {
		res.Err = err.Error()
	}
	ledger, err := s.st.FileRelPaths(ctx, lib.ID)
	if err != nil {
		res.Err = err.Error()
		return res
	}
	known := make(map[string]struct{}, len(ledger))
	for _, rel := range ledger {
		known[rel] = struct{}{}
	}

	extra := make([]string, 0)
	for rel := range onDisk {
		if _, ok := known[rel]; !ok {
			extra = append(extra, rel)
		}
	}
	missing := make([]string, 0)
	for rel := range known {
		if _, ok := onDisk[rel]; !ok {
			missing = append(missing, rel)
		}
	}
	sort.Strings(extra)
	sort.Strings(missing)
	if limit > 0 {
		if len(extra) > limit {
			extra = extra[:limit]
		}
		if len(missing) > limit {
			missing = missing[:limit]
		}
	}
	res.Extra, res.Missing = extra, missing
	return res
}

// handleStrmOrphans 扫描孤儿。library_id 为空表示扫全部库。
func (s *Server) handleStrmOrphans(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	wantID := int64(atoiOr(r.URL.Query().Get("library_id"), 0))
	out := make([]orphanLib, 0, len(libs))
	for _, l := range libs {
		if wantID > 0 && l.ID != wantID {
			continue
		}
		out = append(out, s.scanOrphans(ctx, l, 200))
	}
	if wantID > 0 && len(out) == 0 {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"libraries": out, "scanned_at": formatTime(time.Now())})
}

type cleanOrphansReq struct {
	LibraryID int64    `json:"library_id"`
	Extra     []string `json:"extra"`
	Missing   []string `json:"missing"`
	// All 为真时忽略前端那两个清单，服务端现场重扫一遍全清掉。
	//
	// 扫描接口对每个清单只回前 200 条（不然一个跑偏的库能吐出几万行 JSON），
	// 所以「全清」不能靠前端回传——它手上本来就只有一页。
	All bool `json:"all"`
}

// handleCleanOrphans 删掉一批孤儿。
//
// 两道保险：
//  1. 只删 .strm，且路径必须在库根目录里（防 ../ 穿越）；
//  2. 删之前重新查一次台账，台账里有的绝不删——前端拿到的清单可能是几分钟
//     前的，这中间刚好跑过一轮同步的话，那份清单就已经过期了。
func (s *Server) handleCleanOrphans(w http.ResponseWriter, r *http.Request) {
	var req cleanOrphansReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), req.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	root := s.libRoot(lib)
	if req.All {
		// 现场重扫，拿全量清单（scan 的 limit=0）
		fresh := s.scanOrphans(r.Context(), lib, 0)
		req.Extra, req.Missing = fresh.Extra, fresh.Missing
	}
	if root == "" || len(req.Extra)+len(req.Missing) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"removed": 0, "rows": 0, "skipped": 0})
		return
	}

	ledger, err := s.st.FileRelPaths(r.Context(), lib.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	known := make(map[string]struct{}, len(ledger))
	for _, rel := range ledger {
		known[rel] = struct{}{}
	}

	removed, skipped, failed := 0, 0, 0
	for _, rel := range req.Extra {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || strings.Contains(rel, "..") || !strings.HasSuffix(strings.ToLower(rel), ".strm") {
			skipped++
			continue
		}
		if _, ok := known[rel]; ok {
			skipped++ // 清单过期：这其实是台账里的正常文件
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		if !withinDir(root, p) {
			skipped++
			continue
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			failed++
			continue
		}
		removed++
	}

	// 台账里指向已消失文件的那些行：删行即可，没有文件可删
	missing := make([]string, 0, len(req.Missing))
	for _, rel := range req.Missing {
		rel = filepath.ToSlash(strings.TrimSpace(rel))
		if rel == "" || strings.Contains(rel, "..") {
			continue
		}
		if _, ok := known[rel]; ok {
			missing = append(missing, rel)
		}
	}
	rows := 0
	if len(missing) > 0 {
		if err := s.st.DeleteFilesByRelPaths(r.Context(), lib.ID, missing); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		rows = len(missing)
		if n, e := s.st.CountFiles(r.Context(), lib.ID); e == nil {
			_ = s.st.MarkLibrarySynced(r.Context(), lib.ID, lib.LastSyncAt, n)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removed, "rows": rows, "skipped": skipped, "failed": failed,
	})
}

// withinDir 判断 p 是否在 root 里面（两边都会先求绝对路径）。
func withinDir(root, p string) bool {
	absRoot, err1 := filepath.Abs(root)
	absP, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	rel, err := filepath.Rel(absRoot, absP)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// ==================== 按库配置 ====================

// handleSetKeepExt 改某个库「直链里是否带扩展名」。
//
// 这个开关必须能改：不少播放器/代理会根据 URL 后缀判断容器格式，
// 而 115 的直链带不带 .mkv 直接决定能不能播。
func (s *Server) handleSetKeepExt(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var req struct {
		KeepExt bool `json:"keep_ext"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	if err := s.st.SetLibraryKeepExt(r.Context(), id, req.KeepExt); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("媒体库直链扩展名设置已更新", "lib", lib.Name, "keep_ext", req.KeepExt)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keep_ext": req.KeepExt})
}

// ==================== 小工具 ====================

func formatTime(t time.Time) string {
	if t.IsZero() || t.Year() < 2000 {
		return ""
	}
	return t.Format(time.RFC3339)
}

func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}
