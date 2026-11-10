package httpapi

// 媒体目录（海报墙）的「增 / 改 / 删」。
//
// 三条操作都围绕同一个事实：网盘里的原文件 → 本地 .strm（+ nfo/海报/字幕）
// → 媒体服务器条目。所以「删」要按这条链逐段清理，而不是只删 Emby 里的一行；
// 「增」是把 115 上已有的视频补进这条链；「改」只重写 nfo，不重新联网刮削。
//
// 删除有两个档位：默认只移索引（本地 .strm/元数据 + 台账 + Emby 条目），
// purge=1 才额外把 115 源文件删进回收站（可恢复）。默认保守是刻意的：
// 用户点「删」时通常只是想让墙面上少一张卡片，不是要毁掉网盘里的片子。
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
	"time"

	"strm115/internal/mediaserver"
	"strm115/internal/notify"
	"strm115/internal/pan"
	"strm115/internal/scrape"
	"strm115/internal/store"
	"strm115/internal/strm"
)

// ==================== 增：从 115 目录选片生成 STRM ====================

type catalogVideo struct {
	FileID   string `json:"file_id"`
	Name     string `json:"name"`
	Size     int64  `json:"size"`
	RelDir   string `json:"rel_dir"`
	PickCode string `json:"pick_code,omitempty"`
}

type catalogPickReq struct {
	LibraryID  int64  `json:"library_id"`
	CID        string `json:"cid"`
	RelDir     string `json:"rel_dir"`
	RemotePath string `json:"remote_path,omitempty"` // 目录选择器给的显示路径，如 /电影/2026/空枪
	// AccountID 是目录选择器当前所在账号。一个库挂了两家网盘时，
	// 只有它才能说清「这个 cid 该用谁的凭证去读」。
	AccountID int64 `json:"account_id,omitempty"`
}

// catalogRelDir 算出「这些视频应该落到库根下的哪个目录」。
//
// 同步链路算 rel_path 时是相对 lib.RemoteCID 的（sync.go 的 frame{rel}），
// 所以这里必须用同一把尺子：把目录选择器的完整路径剥掉库 RemotePath 前缀。
// 否则从这个面板加进来的片会和下次同步生成的 .strm 落在两个目录里，
// 台账里出现两份，Emby 里也变成两部重复的片。
func catalogRelDir(lib store.Library, req catalogPickReq) string {
	if strings.TrimSpace(req.RelDir) != "" {
		return strm.CleanRel(req.RelDir)
	}
	full := strings.TrimSpace(req.RemotePath)
	if full == "" || strings.Contains(full, "根目录") {
		return ""
	}
	norm := func(v string) string {
		v = strings.TrimSpace(strings.ReplaceAll(v, "\\", "/"))
		v = strings.Trim(v, "/")
		if v == "" {
			return ""
		}
		return "/" + v
	}
	fullN, rootN := norm(full), norm(lib.RemotePath)
	if rootN != "" && (fullN == rootN || strings.HasPrefix(fullN, rootN+"/")) {
		return strm.CleanRel(strings.TrimPrefix(fullN, rootN))
	}
	// 选了库目录之外的位置：保守地只用最后一级目录名，别把整条网盘路径搬进库。
	return strm.CleanRel(path.Base(fullN))
}

// collectCatalogVideos 递归列目录，最多 4 层 / 24 个目录 / 300 个视频。
//
// 列目录是会打网盘接口的：不加这几个上限，用户在根目录上点一下「预览」
// 就可能发出几百次请求 —— 那正是风控最敏感的场景。
func (s *Server) collectCatalogVideos(ctx context.Context, mgr *pan.Manager,
	cid, relDir string, depth int, state *struct{ dirs int }, out *[]catalogVideo) error {

	if depth > 3 || state.dirs >= 24 || len(*out) >= 300 {
		return nil
	}
	state.dirs++
	entries, err := mgr.ListAll(ctx, cid, 200)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(*out) >= 300 {
			break
		}
		if e.IsDir {
			sub := path.Join(relDir, strm.Sanitize(e.Name))
			if err := s.collectCatalogVideos(ctx, mgr, e.ID, sub, depth+1, state, out); err != nil {
				s.log.Warn("预览子目录失败", "cid", e.ID, "err", err)
			}
			continue
		}
		if !strm.IsVideo(e.Name) {
			continue
		}
		*out = append(*out, catalogVideo{
			FileID: e.ID, Name: e.Name, Size: e.Size,
			RelDir: relDir, PickCode: e.PickCode,
		})
	}
	return nil
}

func (s *Server) catalogPicker(ctx context.Context, req catalogPickReq) (store.Library, *pan.Manager, []catalogVideo, string, error) {
	if req.LibraryID <= 0 {
		return store.Library{}, nil, nil, "", errors.New("请先选一个媒体库")
	}
	if strings.TrimSpace(req.CID) == "" {
		return store.Library{}, nil, nil, "", errors.New("请先选一个网盘目录")
	}
	lib, err := s.st.GetLibrary(ctx, req.LibraryID)
	if err != nil {
		return store.Library{}, nil, nil, "", errors.New("媒体库不存在")
	}
	accountID := s.accountForDir(ctx, lib, req.CID, req.AccountID)
	mgr, err := s.pool.Get(ctx, accountID)
	if err != nil {
		return store.Library{}, nil, nil, "", fmt.Errorf("读取网盘账号失败: %w", err)
	}
	relDir := catalogRelDir(lib, req)
	state := &struct{ dirs int }{}
	videos := []catalogVideo{}
	if err := s.collectCatalogVideos(ctx, mgr, req.CID, relDir, 0, state, &videos); err != nil {
		return store.Library{}, nil, nil, "", fmt.Errorf("列网盘目录失败: %w", err)
	}
	return lib, mgr, videos, relDir, nil
}

func (s *Server) handleCatalogScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var req catalogPickReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, _, videos, relDir, err := s.catalogPicker(ctx, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	// 预览时就告诉用户哪些已经入库，避免点完「添加」才发现全是重复。
	added, skipped := 0, 0
	for _, v := range videos {
		target := strm.Target(s.libRoot(lib), v.RelDir, v.Name)
		rel := filepath.ToSlash(relativeOrEmpty(s.libRoot(lib), target))
		if _, ok, _ := s.st.GetFileByRelPath(ctx, lib.ID, rel); ok {
			skipped++
		} else {
			added++
		}
	}
	preview := videos
	if len(preview) > 30 {
		preview = preview[:30]
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "library": lib.Name, "rel_dir": relDir,
		"videos": len(videos), "will_add": added, "will_skip": skipped,
		"items": preview,
	})
}

func relativeOrEmpty(root, full string) string {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return ""
	}
	return filepath.ToSlash(rel)
}

func (s *Server) handleCatalogAdd(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	var req catalogPickReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	lib, _, videos, relDir, err := s.catalogPicker(ctx, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if len(videos) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("这个目录里没找到视频文件"))
		return
	}
	// 这批文件属于哪个账号，.strm 里就要写哪家的播放地址：夸克的文件
	// 必须带 q/ 前缀，否则播放代理会拿 115 的凭证去换夸克的 fid。
	accountID := s.accountForDir(ctx, lib, req.CID, req.AccountID)
	accountKind, _ := s.st.AccountKind(ctx, accountID)

	domain := strings.TrimSpace(s.setting(ctx, "play_domain"))
	if domain == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请先在设置里填播放地址，.strm 里要写这个地址"))
		return
	}
	if normalized, err := normalizePlayDomain(domain); err == nil {
		domain = normalized
	}
	root := s.libRoot(lib)

	rows := make([]store.File, 0, len(videos))
	paths := make([]string, 0, len(videos))
	skipped := 0
	conflict := map[string]string{}
	for _, v := range videos {
		target := strm.Target(root, v.RelDir, v.Name)
		rel := relativeOrEmpty(root, target)
		if rel == "" || strings.HasPrefix(rel, "..") {
			continue
		}
		if _, ok, _ := s.st.GetFileByRelPath(ctx, lib.ID, rel); ok {
			skipped++
			continue
		}
		if pc, dup := conflict[target]; dup && pc != v.PickCode {
			target = strings.TrimSuffix(target, ".strm") + " [" + shortPick(v.PickCode) + "].strm"
			rel = relativeOrEmpty(root, target)
		}
		conflict[target] = v.PickCode
		content := strm.Content(domain, strm.PlayIDForKind(accountKind, v.PickCode), lib.KeepExt, v.Name)
		if err := strm.Write(target, content); err != nil {
			writeErr(w, http.StatusInternalServerError, fmt.Errorf("写 %s 失败: %w", rel, err))
			return
		}
		rows = append(rows, store.File{
			LibraryID: lib.ID, AccountID: accountID, FileID: v.FileID, PickCode: v.PickCode,
			RelPath: rel, Name: v.Name, Size: v.Size,
		})
		paths = append(paths, rel)
	}
	if err := s.st.UpsertFiles(ctx, accountID, rows); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 新写的 .strm 只有刮削过才有 nfo / 海报。整库刮削对已有条目全部命中本地
	// 缓存，实际只会为这几个新片名发请求，不会把库里的老片重查一遍。
	scrapeStarted := false
	if job, err := s.startScrape(lib, s.scrapeOptions(ctx, false, false)); err == nil {
		scrapeStarted = true
		go s.refreshServerAfterScrape(job, lib)
		s.chainSubtitlesAfterScrape(lib, job)
	} else {
		// 已有刮削任务在跑：不打断它，只让 Emby 先把新文件认出来。
		go s.refreshServerSoon(lib)
	}

	writeJSON(w, http.StatusAccepted, map[string]any{
		"ok": true, "added": len(rows), "skipped": skipped,
		"rel_dir": relDir, "paths": paths,
		"scrape_started": scrapeStarted,
		"note":           "已写入 .strm 并登记台账；刮削完成后 Emby 会自动刷新出海报与简介。",
	})
}

func shortPick(p string) string {
	if len(p) > 8 {
		return p[:8]
	}
	return p
}

func (s *Server) refreshServerSoon(lib store.Library) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	srv, err := s.mediaServer(ctx)
	if err != nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	case <-time.After(2 * time.Second):
	}
	_ = srv.Refresh(ctx, lib.ServerLibID)
}

// ==================== 改：重写本地 nfo ====================

type catalogUpdateReq struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Original string   `json:"original"`
	Year     string   `json:"year"`
	Overview string   `json:"overview"`
	Genres   []string `json:"genres"`
	Rating   string   `json:"rating"`
	IMDbID   string   `json:"imdb_id"`
	TMDbID   int      `json:"tmdb_id"`
}

func (s *Server) handleCatalogUpdate(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	var req catalogUpdateReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	res, err := s.catalogResolve(ctx, req.Key)
	if err != nil {
		writeErr(w, catalogStatusOf(err), err)
		return
	}
	if res.Lib.ID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("这条不属于任何媒体库，无法改写本地元数据"))
		return
	}
	m, err := s.catalogMediaFor(ctx, res)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}

	// 换 ID = 换了一部片，必须重新拉元数据（命中缓存时 0 请求）。
	// 纯 TMDB 之后优先看 tmdb_id：它才是主键，改它连 /find 都不用。
	// 只有 tmdb_id 没变、IMDb ID 变了时才走 /find 反查。
	switch {
	case req.TMDbID > 0 && req.TMDbID != m.TMDbID:
		out, err := s.scrape.RescrapeOneByTMDbID(ctx, res.Lib, m, req.TMDbID,
			s.scrapeOptions(ctx, true, false))
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		m = out
	default:
		newID := scrape.NormalizeIMDbID(req.IMDbID)
		if newID != "" && newID != m.IMDbID {
			out, err := s.scrape.RescrapeOne(ctx, res.Lib, m, newID, s.scrapeOptions(ctx, true, false))
			if err != nil {
				writeErr(w, http.StatusBadGateway, err)
				return
			}
			m = out
		}
	}
	edit := scrape.Edit{
		Title: req.Title, Original: req.Original, Year: req.Year,
		Overview: req.Overview, Genres: req.Genres, Rating: req.Rating,
	}
	if !editEmpty(edit) {
		out, err := s.scrape.EditMeta(ctx, res.Lib, m, edit)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		m = out
	}
	if err := s.st.UpsertMedia(ctx, m); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	go s.refreshMediaDir(res.Lib, m.RelPath)
	card := s.cardFromMedia(m, res.Lib)
	card.Key = req.Key
	card.InEmby = res.Source == "emby"
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "item": card, "local": catalogLocalView(m)})
}

func editEmpty(e scrape.Edit) bool {
	return strings.TrimSpace(e.Title) == "" && strings.TrimSpace(e.Original) == "" &&
		strings.TrimSpace(e.Year) == "" && strings.TrimSpace(e.Overview) == "" &&
		len(e.Genres) == 0 && strings.TrimSpace(e.Rating) == ""
}

func catalogStatusOf(err error) int {
	if errors.Is(err, errCatalogNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, errCatalogBadKey) {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

var (
	errCatalogNotFound = errors.New("条目不存在")
	errCatalogBadKey   = errors.New("条目 key 不合法")
)

// ==================== 删：按链逐段清理 ====================

func (s *Server) handleCatalogDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	key := r.URL.Query().Get("key")
	purge := r.URL.Query().Get("purge") == "1"
	res, err := s.catalogResolve(ctx, key)
	if err != nil {
		writeErr(w, catalogStatusOf(err), err)
		return
	}
	if res.Lib.ID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("这条不属于任何媒体库，无法删除"))
		return
	}

	lib := res.Lib
	var files []store.File
	if res.ShowDir != "" {
		files, err = s.st.ListFilesByPrefix(ctx, lib.ID, res.ShowDir)
	} else {
		f, ok, e := s.st.GetFileByRelPath(ctx, lib.ID, res.RelPath)
		err = e
		if ok {
			files = []store.File{f}
		}
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	// 先把要删的东西算清楚，再动任何一处。purge 放在最前面：
	// 网盘删失败就整个中止，避免留下「本地索引没了、网盘还在」的半吊子状态。
	//
	// 按台账里的归属账号分组：一个库挂了两家网盘之后，用 115 的凭证去删
	// 夸克的 fid 只会拿到一串参数错误，而用户看到的是「删除失败」。
	if purge && len(files) > 0 {
		byAccount := map[int64][]string{}
		for _, f := range files {
			if f.FileID == "" {
				continue
			}
			owner := f.AccountID
			if owner == 0 {
				owner = lib.AccountID
			}
			byAccount[owner] = append(byAccount[owner], f.FileID)
		}
		for accountID, ids := range byAccount {
			mgr, err := s.pool.Get(ctx, accountID)
			if err != nil {
				writeErr(w, http.StatusBadGateway, fmt.Errorf("读取网盘账号失败: %w", err))
				return
			}
			for i := 0; i < len(ids); i += 50 {
				end := i + 50
				if end > len(ids) {
					end = len(ids)
				}
				if _, err := mgr.Remove(ctx, ids[i:end]...); err != nil {
					writeErr(w, http.StatusBadGateway,
						fmt.Errorf("删网盘源文件失败（本地未改动）: %w", err))
					return
				}
			}
		}
	}

	rels := make([]string, 0, len(files)+1)
	for _, f := range files {
		if f.RelPath != "" {
			rels = append(rels, f.RelPath)
		}
	}
	if len(rels) == 0 && res.RelPath != "" {
		rels = append(rels, res.RelPath)
	}
	removed := s.catalogRemoveFiles(s.libRoot(lib), rels, res.ShowDir)

	if len(rels) > 0 {
		if err := s.st.DeleteFilesByRelPaths(ctx, lib.ID, rels); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}
	if res.ShowDir != "" {
		err = s.st.DeleteMediaByPrefix(ctx, lib.ID, res.ShowDir)
	} else {
		err = s.st.DeleteMediaByRelPaths(ctx, lib.ID, rels)
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	embyRemoved := false
	if res.Cat != nil && res.EmbyID != "" {
		if err := res.Cat.DeleteItem(ctx, res.EmbyID); err == nil {
			embyRemoved = true
		} else {
			s.log.Warn("从媒体服务器移除条目失败", "id", res.EmbyID, "err", err)
		}
	}
	go s.refreshServerSoon(lib)

	// 删除是不可逆动作（purge 时连 115 源文件一起进回收站），推一条留痕。
	// 名字三级兜底：台账 → Emby 条目 → 文件名，删完再想查是哪部片就没机会了。
	name := strings.TrimSpace(res.Media.Title)
	if name == "" {
		name = strings.TrimSpace(res.EmbyItem.Name)
	}
	if name == "" {
		name = path.Base(res.RelPath)
	}
	if y := strings.TrimSpace(res.Media.Year); y != "" {
		name += "（" + y + "）"
	}
	line := fmt.Sprintf("本地 %d 个文件", removed)
	if purge {
		line += fmt.Sprintf(" · 115 源文件 %d 个（回收站）", purgedCount(purge, len(files)))
	}
	if embyRemoved {
		line += " · Emby 条目已移除"
	}
	s.pushEvent("delete", notify.KindDelete, "🗑 已删除 · "+name, line)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "removed_files": removed, "removed_rows": len(rels),
		"remote_files": len(files),
		"purged":       purgedCount(purge, len(files)),
		"purge":        purge,
		"emby_removed": embyRemoved,
		"note":         catalogDeleteNote(purge, embyRemoved),
	})
}

func purgedCount(purge bool, files int) int {
	if !purge {
		return 0
	}
	return files
}

func catalogDeleteNote(purge, embyRemoved bool) string {
	tail := "Emby 条目已一并移除。"
	if !embyRemoved {
		tail = "这条来自本地台账，Emby 里的条目未改动。"
	}
	if purge {
		return "已删除本地 .strm、元数据与台账；115 源文件进了回收站，可在网盘里恢复。" + tail
	}
	return "已移除本地 .strm、元数据与台账；115 源文件保留，下次同步会重新生成。" + tail
}

// catalogRemoveFiles 删 .strm 与它旁边的 nfo / 图片 / 字幕。
//
// 目录级的 movie.nfo / poster.jpg 只在「这个目录里已经没有 .strm」时才删：
// 一个目录里放了好几部片，删掉一部不能把共用的封面一起带走。
func (s *Server) catalogRemoveFiles(root string, rels []string, showDir string) int {
	count := 0
	dirs := map[string]bool{}
	for _, rel := range rels {
		full := filepath.Join(root, filepath.FromSlash(rel))
		dirs[filepath.Dir(full)] = true
		if removeCatalogFile(full) {
			count++
		}
		base := strings.TrimSuffix(full, filepath.Ext(full))
		for _, suffix := range []string{
			".nfo", ".srt", ".zh.srt", ".zh-CN.srt", ".zht.srt", ".ass", ".zh.ass",
			"-poster.jpg", "-poster.png", "-fanart.jpg", "-fanart.png",
		} {
			if removeCatalogFile(base + suffix) {
				count++
			}
		}
	}
	if showDir != "" {
		dirs[filepath.Join(root, filepath.FromSlash(showDir))] = true
	}
	for dir := range dirs {
		if catalogDirHasStrm(dir) {
			continue
		}
		for _, name := range []string{"movie.nfo", "tvshow.nfo", "poster.jpg", "poster.png", "fanart.jpg", "fanart.png"} {
			if removeCatalogFile(filepath.Join(dir, name)) {
				count++
			}
		}
	}

	// 只清理变空的目录，最长回溯 3 层，且绝不删库根本身。
	cleanRoot := filepath.Clean(root)
	for dir := range dirs {
		for i := 0; i < 3; i++ {
			if dir == cleanRoot || !strings.HasPrefix(dir, cleanRoot+string(filepath.Separator)) {
				break
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 0 {
				break
			}
			if err := os.Remove(dir); err != nil {
				break
			}
			count++
			dir = filepath.Dir(dir)
		}
	}
	return count
}

func removeCatalogFile(p string) bool {
	st, err := os.Lstat(p)
	if err != nil || st.IsDir() {
		return false
	}
	if err := os.Remove(p); err != nil {
		return false
	}
	return true
}

func catalogDirHasStrm(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return true // 读不了就当还有内容，宁可少删
	}
	for _, e := range entries {
		if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".strm") {
			return true
		}
	}
	return false
}

// ==================== 公共：把 key 解析成可操作的目标 ====================

type catalogResolved struct {
	Source   string
	EmbyID   string
	Lib      store.Library
	RelPath  string // 单条：.strm 的相对路径
	ShowDir  string // 剧集：剧根目录（前缀）
	IsSeries bool
	Media    store.Media
	EmbyItem mediaserver.CatalogItem
	Srv      mediaserver.Server
	Cat      mediaserver.Catalog
}

func (s *Server) catalogResolve(ctx context.Context, key string) (*catalogResolved, error) {
	src, id := splitCatalogKey(key)
	if id == "" || (src != "emby" && src != "local") {
		return nil, errCatalogBadKey
	}
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		return nil, err
	}
	out := &catalogResolved{Source: src}

	if src == "emby" {
		srv, cat, err := s.catalogServer(ctx)
		if err != nil {
			return nil, fmt.Errorf("没连上 Emby: %w", err)
		}
		out.Srv, out.Cat, out.EmbyID = srv, cat, id
		it, err := cat.Item(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", errCatalogNotFound, err)
		}
		out.EmbyItem = it
		lib, rel, ok := s.catalogLocate(ctx, libs, it.Path)
		if ok {
			out.Lib, out.RelPath = lib, rel
			out.IsSeries = it.Type == "Series"
			if out.IsSeries {
				out.ShowDir, out.RelPath = rel, ""
			} else if m, ok, _ := s.st.GetMedia(ctx, lib.ID, rel); ok {
				out.Media = m
			}
		}
		return out, nil
	}

	mid, err := strconv.ParseInt(id, 10, 64)
	if err != nil || mid <= 0 {
		return nil, errCatalogBadKey
	}
	m, ok, err := s.st.GetMediaByID(ctx, mid)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errCatalogNotFound
	}
	out.Media = m
	out.RelPath = m.RelPath
	out.IsSeries = m.Kind == "series"
	if out.IsSeries {
		out.ShowDir = catalogShowRoot(m.RelPath)
	}
	for _, l := range libs {
		if l.ID == m.LibraryID {
			out.Lib = l
			break
		}
	}
	return out, nil
}

// catalogMediaFor 找一条能改写的刮削记录；Emby 里有、台账里没有时补建一条。
func (s *Server) catalogMediaFor(ctx context.Context, res *catalogResolved) (store.Media, error) {
	if res.Media.ID > 0 {
		return res.Media, nil
	}
	if res.ShowDir != "" {
		all, err := s.st.ListMediaAll(ctx, res.Lib.ID)
		if err != nil {
			return store.Media{}, err
		}
		for _, m := range all {
			if strings.HasPrefix(m.RelPath, res.ShowDir+"/") || m.RelPath == res.ShowDir {
				return m, nil
			}
		}
	}
	if res.RelPath == "" {
		return store.Media{}, errors.New("找不到对应的本地 .strm")
	}
	p := scrape.Parse(res.RelPath)
	m := store.Media{
		LibraryID: res.Lib.ID, RelPath: res.RelPath,
		Kind:  firstNonEmptyLocal(p.Kind, "movie"),
		Title: p.Title, Year: p.Year, Season: p.Season, Episode: p.Episode,
		IMDbID:    res.EmbyItem.IMDbID,
		MetaTitle: res.EmbyItem.Name,
		Overview:  res.EmbyItem.Overview,
		Rating:    fmtRating(res.EmbyItem.CommunityRating),
		Genres:    strings.Join(res.EmbyItem.Genres, ", "),
		Status:    "ok",
	}
	if y := res.EmbyItem.ProductionYear; y > 0 {
		m.MetaYear = strconv.Itoa(y)
	}
	if err := s.st.UpsertMedia(ctx, m); err != nil {
		return store.Media{}, err
	}
	got, ok, err := s.st.GetMedia(ctx, res.Lib.ID, res.RelPath)
	if err != nil || !ok {
		return m, nil
	}
	return got, nil
}

func fmtRating(f float64) string {
	if f <= 0 {
		return ""
	}
	return strconv.FormatFloat(f, 'f', 1, 64)
}
