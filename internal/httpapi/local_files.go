package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
)

// ==================== 本地文件管理 ====================
//
// 「网盘文件」看的是 115，「本地文件」看的是宿主 /media 里真实落了什么。
// 两者不能合成一个页面：网盘里有的东西本地不一定有（没同步），本地有的
// 东西网盘里也不一定有（手放的海报、删完没跟上同步的 .strm、发布组的广告壳）。
//
// 这一页只做三件事：看真实目录、看体积、删残留。刻意不做重命名/移动：
// 本地镜像是同步生成的，改名字下一轮同步就还原，等于骗自己。

// localLib 是一个媒体库在本地这一侧的概况。
type localLib struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Collection string `json:"collection_type"`
	Root       string `json:"root"`   // 实际落盘根目录（已按媒体根目录修正过）
	Exists     bool   `json:"exists"` // 目录在不在
	Ledger     int    `json:"ledger"` // 台账里应有的文件数
}

// localEntry 是本地目录里的一行。
//
// Known 只对文件有意义：台账里有这条 rel_path，说明它是同步写出来的；
// 没有的就是「本地多出来的东西」（广告壳、手放的海报、删库里条目后留下的
// 空 .strm）。这个标记是这一页最该显示的状态——它把「磁盘上有什么」和
// 「媒体库里该有什么」对上了。
type localEntry struct {
	Name  string `json:"name"`
	Path  string `json:"path"` // 相对库根，斜杠分隔
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
	MTime string `json:"mtime"`
	Ext   string `json:"ext,omitempty"`
	Known bool   `json:"known"`
}

type localCrumb struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type localBrowseResp struct {
	Library   localLib     `json:"library"`
	Path      string       `json:"path"`
	Crumbs    []localCrumb `json:"crumbs"`
	Entries   []localEntry `json:"entries"`
	Truncated bool         `json:"truncated"`
	Dirs      int          `json:"dirs"`
	Files     int          `json:"files"`
	Orphans   int          `json:"orphans"`
}

// localStat 是一次真实遍历的结果。只在用户点「统计」时算：
// 遍历开销随库大小线性增长，不能塞进每次进页面。
type localStat struct {
	LibraryID int64  `json:"library_id"`
	Root      string `json:"root"`
	Files     int    `json:"files"`
	Dirs      int    `json:"dirs"`
	Bytes     int64  `json:"bytes"`
	Orphans   int    `json:"orphans"` // 本地有、台账里没有的文件
	EmptyDirs int    `json:"empty_dirs"`
	ScannedAt string `json:"scanned_at"`
	Truncated bool   `json:"truncated"`
	Err       string `json:"err,omitempty"`
}

// maxLocalEntries 一次最多回多少行。115 上一个目录几百个文件是常态，
// 但一个跑偏的库根能塞几万个，JSON 会先把浏览器压死。
const maxLocalEntries = 1000

// localRootFor 取库的实际落盘目录。和孤儿扫描用同一个函数，
// 免得两处对「本地根」的理解慢慢分叉。
func (s *Server) localRootFor(lib store.Library) string {
	return s.libRoot(lib)
}

// resolveLocal 把「相对库根的路径」变成一个可以 stat 的绝对路径。
//
// 三条硬规则，缺一条都会被 ../ 穿越或绝对路径绕过：
//  1. 不接受绝对路径；
//  2. 清掉后的相对路径不能以 .. 开头；
//  3. 拼出来之后必须还在库根目录里面。
func resolveLocal(root, rel string) (string, error) {
	rel = strings.TrimSpace(filepath.ToSlash(rel))
	rel = strings.TrimPrefix(rel, "/")
	if filepath.IsAbs(rel) {
		return "", errors.New("路径必须是相对库根的路径")
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("路径越界")
	}
	if clean == "." {
		return filepath.Clean(root), nil
	}
	p := filepath.Join(root, clean)
	if !withinDir(root, p) {
		return "", errors.New("路径越界")
	}
	return p, nil
}

// hostLocalPath 把「容器内绝对路径」换成「宿主机绝对路径」。
//
// 只用于展示 / 复制 / file:// 链接：后端自己读写文件一律用容器内路径。
// MEDIA_HOST_PATH 必须是宿主机上的绝对路径（Linux /volume1/media、
// Windows D:/media）；相对值 ./media 在容器里换算不出来，宁可返回空，
// 也不能给用户一个看着像能点、其实不存在的地址。
func (s *Server) hostLocalPath(abs string) string {
	host := strings.TrimSpace(s.cfg.MediaHostRoot)
	if host == "" {
		return ""
	}
	host = strings.TrimSuffix(strings.ReplaceAll(host, "\\", "/"), "/")
	absolute := strings.HasPrefix(host, "/")
	if len(host) >= 3 && host[1] == ':' && host[2] == '/' &&
		((host[0] >= 'A' && host[0] <= 'Z') || (host[0] >= 'a' && host[0] <= 'z')) {
		absolute = true
	}
	if !absolute {
		return ""
	}
	root := filepath.Clean(s.cfg.MediaRoot)
	rel, err := filepath.Rel(root, filepath.Clean(abs))
	if err != nil || rel == ".." ||
		strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ""
	}
	if rel == "." {
		return host
	}
	return host + "/" + filepath.ToSlash(rel)
}

// knownPaths 返回台账里的 rel_path 集合（斜杠分隔）。
func (s *Server) knownPaths(ctx context.Context, libID int64) map[string]struct{} {
	byID, err := s.st.FileRelPaths(ctx, libID)
	if err != nil {
		return map[string]struct{}{}
	}
	out := make(map[string]struct{}, len(byID))
	for _, rel := range byID {
		out[filepath.ToSlash(rel)] = struct{}{}
	}
	return out
}

func (s *Server) localLibBrief(ctx context.Context, lib store.Library) localLib {
	root := s.localRootFor(lib)
	_, err := os.Stat(root)
	n, _ := s.st.CountFiles(ctx, lib.ID)
	return localLib{
		ID: lib.ID, Name: lib.Name, Collection: lib.CollectionType,
		Root: root, Exists: err == nil, Ledger: n,
	}
}

// handleLocalOverview 列出所有媒体库在本地这一侧的概况。
func (s *Server) handleLocalOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]localLib, 0, len(libs))
	ledger := 0
	for _, l := range libs {
		b := s.localLibBrief(ctx, l)
		ledger += b.Ledger
		out = append(out, b)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"libraries": out,
		"totals":    map[string]any{"libraries": len(out), "ledger": ledger},
	})
}

// handleLocalBrowse 列一层目录。path 为空表示库根。
func (s *Server) handleLocalBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	libID, _ := strconv.ParseInt(strings.TrimSpace(q.Get("library_id")), 10, 64)
	if libID <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("请先选一个媒体库"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), libID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	root := s.localRootFor(lib)
	if root == "" {
		writeErr(w, http.StatusBadRequest, errors.New("这个库还没配本地目录"))
		return
	}
	rel := strings.TrimSpace(q.Get("path"))
	dir, err := resolveLocal(root, rel)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, errors.New("目录不存在："+dir))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	known := s.knownPaths(r.Context(), lib.ID)
	relClean := ""
	if dir != filepath.Clean(root) {
		if rr, err := filepath.Rel(root, dir); err == nil {
			relClean = filepath.ToSlash(rr)
		}
	}

	entries := make([]localEntry, 0, len(items))
	dirs, files, orphans := 0, 0, 0
	for _, it := range items {
		if len(entries) >= maxLocalEntries {
			break
		}
		info, ierr := it.Info()
		var size int64
		var mtime time.Time
		isDir := it.IsDir()
		if ierr == nil {
			if !isDir {
				size = info.Size()
			}
			mtime = info.ModTime()
		}
		name := it.Name()
		childRel := name
		if relClean != "" {
			childRel = relClean + "/" + name
		}
		_, inLedger := known[childRel]
		e := localEntry{
			Name: name, Path: childRel, IsDir: isDir, Size: size,
			MTime: formatTime(mtime), Known: !isDir && inLedger,
		}
		if !isDir {
			e.Ext = strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
			files++
			if !inLedger {
				orphans++
			}
		} else {
			dirs++
		}
		entries = append(entries, e)
	}
	sortLocalEntries(entries)

	crumbs := []localCrumb{{Name: lib.Name, Path: ""}}
	if relClean != "" {
		acc := ""
		for _, seg := range strings.Split(relClean, "/") {
			if acc == "" {
				acc = seg
			} else {
				acc += "/" + seg
			}
			crumbs = append(crumbs, localCrumb{Name: seg, Path: acc})
		}
	}

	writeJSON(w, http.StatusOK, localBrowseResp{
		Library: s.localLibBrief(r.Context(), lib),
		Path:    relClean, Crumbs: crumbs,
		Entries: entries, Truncated: len(items) > maxLocalEntries,
		Dirs: dirs, Files: files, Orphans: orphans,
	})
}

// sortLocalEntries 目录在前，同类按名字排。
// 用不区分大小写的比较：网盘里 Heart 和 heart 都可能出现，
// 按字节序排会把同名的挤到两端。
func sortLocalEntries(es []localEntry) {
	sort.SliceStable(es, func(i, j int) bool {
		if es[i].IsDir != es[j].IsDir {
			return es[i].IsDir
		}
		a, b := strings.ToLower(es[i].Name), strings.ToLower(es[j].Name)
		if a == b {
			return es[i].Name < es[j].Name
		}
		return a < b
	})
}

type localStatReq struct {
	LibraryID int64 `json:"library_id"`
}

// handleLocalStat 真遍历一遍库根，算体积 / 孤儿 / 空目录。
//
// 超时或文件太多就停，并把 truncated 带回去——「算了 20 分钟才出结果」
// 对一个管理页面来说没有意义。
func (s *Server) handleLocalStat(w http.ResponseWriter, r *http.Request) {
	var req localStatReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), req.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	root := s.localRootFor(lib)
	res := localStat{LibraryID: lib.ID, Root: root, ScannedAt: formatTime(time.Now())}
	if root == "" {
		res.Err = "这个库还没配本地目录"
		writeJSON(w, http.StatusOK, res)
		return
	}
	if _, err := os.Stat(root); err != nil {
		res.Err = "本地目录不存在：" + root
		writeJSON(w, http.StatusOK, res)
		return
	}
	known := s.knownPaths(ctx, lib.ID)

	const maxFiles = 50000
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			// 单个目录没权限不该让整次统计失败
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != filepath.Clean(root) {
				res.Dirs++
				if ents, err := os.ReadDir(p); err == nil && len(ents) == 0 {
					res.EmptyDirs++
				}
			}
			return nil
		}
		if res.Files >= maxFiles {
			res.Truncated = true
			return fs.SkipAll
		}
		res.Files++
		if info, err := d.Info(); err == nil {
			res.Bytes += info.Size()
		}
		if rel, err := filepath.Rel(root, p); err == nil {
			if _, ok := known[filepath.ToSlash(rel)]; !ok {
				res.Orphans++
			}
		}
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) && ctx.Err() == nil {
		res.Err = walkErr.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

type localRemoveReq struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"`
	// Recursive 只对目录有意义：非空目录必须显式带上它，
	// 避免「点了删除，整季都没了」这种事故。
	Recursive bool `json:"recursive"`
}

// handleLocalRemove 删除本地的一个文件或目录。
//
// 四道保险：
//  1. 不能删库根本身；
//  2. 路径必须落在库根里面（resolveLocal 已经挡了 ../ 和绝对路径）；
//  3. 非空目录必须显式 recursive；
//  4. 删完不改台账 —— 台账是「应该有什么」，本地是「实际有什么」，
//     把它们绑在一起会让「删一个本地文件」变成「悄无声息地改了库内容」。
//     台账里还留着的，下一轮同步会照常把 .strm 写回来，这一点会在返回里说清楚。
func (s *Server) handleLocalRemove(w http.ResponseWriter, r *http.Request) {
	var req localRemoveReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("不能删除媒体库根目录"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), req.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	root := s.localRootFor(lib)
	p, err := resolveLocal(root, req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if filepath.Clean(p) == filepath.Clean(root) {
		writeErr(w, http.StatusBadRequest, errors.New("不能删除媒体库根目录"))
		return
	}
	info, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			writeErr(w, http.StatusNotFound, errors.New("文件已经不在了"))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	removed := 1
	wasDir := info.IsDir()
	if wasDir {
		ents, rerr := os.ReadDir(p)
		if rerr != nil {
			writeErr(w, http.StatusInternalServerError, rerr)
			return
		}
		if len(ents) > 0 && !req.Recursive {
			writeErr(w, http.StatusConflict, errors.New("这个目录不是空的，要删得先确认「连同里面的文件一起删」"))
			return
		}
		if req.Recursive {
			removed = countLocalFiles(p)
			err = os.RemoveAll(p)
		} else {
			err = os.Remove(p)
		}
	} else {
		err = os.Remove(p)
	}
	if err != nil && !os.IsNotExist(err) {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	rel := filepath.ToSlash(req.Path)
	_, inLedger := s.knownPaths(r.Context(), lib.ID)[rel]
	hint := "已从本地删除"
	if inLedger {
		hint = "已从本地删除；这条还在同步台账里，下一轮同步会重新写回来。要彻底不要，请先去 115 删掉，或把这个库的条目移除"
	}
	s.log.Warn("本地文件已删除", "lib", lib.Name, "path", rel, "dir", wasDir, "removed", removed)
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removed,
		"is_dir":  wasDir,
		"hint":    hint,
	})
}

// ==================== 打开本地目录 ====================

type localOpenReq struct {
	LibraryID int64  `json:"library_id"`
	Path      string `json:"path"` // 相对库根的目录，绝不接受绝对路径
}

// handleLocalOpen 在「运行服务的那台机器」上打开目录。
//
// 浏览器从 http 页面跳 file:// 会被拦，Docker 里更没有宿主机的桌面，
// 所以这里由后端按系统调资源管理器。三道限制：
//  1. 只收相对库根的路径，走 resolveLocal 挡 ../ 和绝对路径；
//  2. 目标必须存在且是目录；
//  3. 容器里直接回「打开不了」，不去调一个注定失败的 xdg-open。
//
// 服务跑在 Windows 本机（strm115.exe）时这一条能直接把文件夹弹出来；
// 跑在 Docker 时前端仍可复制路径，自己到宿主机里打开。
func (s *Server) handleLocalOpen(w http.ResponseWriter, r *http.Request) {
	var req localOpenReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, err := s.st.GetLibrary(r.Context(), req.LibraryID)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	root := s.localRootFor(lib)
	dir, err := resolveLocal(root, req.Path)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		writeErr(w, http.StatusNotFound, errors.New("本地目录不存在："+dir))
		return
	}
	if inContainer() {
		host := s.hostLocalPath(dir)
		hint := "服务跑在容器里，打不开宿主机的文件夹；请复制路径自己打开"
		if host != "" {
			hint = "服务跑在容器里，打不开宿主机的文件夹；已为你换算出宿主机路径，复制它自己打开"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"opened": false, "path": dir, "host_path": host, "hint": hint,
		})
		return
	}

	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("explorer.exe", filepath.Clean(dir))
	case "darwin":
		cmd = exec.Command("open", dir)
	default:
		cmd = exec.Command("xdg-open", dir)
	}
	// 只 Start 不 Wait：资源管理器的进程生命周期和 HTTP 请求无关。
	if err := cmd.Start(); err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("打开目录失败：%w", err))
		return
	}
	go func() { _ = cmd.Wait() }()
	writeJSON(w, http.StatusOK, map[string]any{"opened": true, "path": dir})
}

// inContainer 用 /.dockerenv 判断，比读 cgroup 稳；容器里没有桌面，
// 「打开目录」这个动作没有意义，早一点告诉用户比弹一个错误好。
func inContainer() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	_, err := os.Stat("/.dockerenv")
	return err == nil
}

// countLocalFiles 数一个目录下有多少个文件（含子目录），用于回执。
func countLocalFiles(root string) int {
	n := 0
	_ = filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	return n
}
