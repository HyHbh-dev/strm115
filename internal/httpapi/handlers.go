package httpapi

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"strm115/internal/mediaserver"
	"strm115/internal/netproxy"
	"strm115/internal/notify"
	"strm115/internal/pan"
	"strm115/internal/pan/driver115"
	"strm115/internal/panpool"
	"strm115/internal/store"
	syncsvc "strm115/internal/sync"
	"strm115/internal/tmdb"
	"strm115/internal/tvdb"
	"strm115/internal/tvmaze"
)

// ==================== 概览 ====================

func (s *Server) handleInstance(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	accounts, _ := s.st.ListAccounts(ctx)
	libs, _ := s.st.ListLibraries(ctx)
	if accounts == nil {
		accounts = []store.Account{}
	}
	if libs == nil {
		libs = []store.Library{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"version":     Version,
		"accounts":    accounts,
		"libraries":   libs,
		"media_root":  s.cfg.MediaRoot,
		"play_domain": s.setting(ctx, "play_domain"),
		"kinds":       pan.Kinds(),
		"user":        s.currentUser(),
	})
}

func randToken() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b)
}

// ==================== 账号 ====================

func (s *Server) handleListAccounts(w http.ResponseWriter, r *http.Request) {
	accs, err := s.st.ListAccounts(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if accs == nil {
		accs = []store.Account{}
	}
	// 前端要知道「这个夸克账号能不能转存」：Cookie 是后补的，光看 kind 看不出来。
	// 只回一个布尔，不把 Cookie 本身回给浏览器。
	out := make([]map[string]any, 0, len(accs))
	for _, a := range accs {
		item := map[string]any{
			"id": a.ID, "kind": a.Kind, "name": a.Name, "created_at": a.CreatedAt,
		}
		if strings.EqualFold(a.Kind, quarkKind) {
			// ListAccounts 只查 id/kind/name/created_at（凭证是密文，不进列表），
			// 夸克这里要额外读一次完整账号才能判断 Cookie 配没配。账号个位数，值得。
			cred := a.Credential
			if cred == "" {
				if full, err := s.st.GetAccount(r.Context(), a.ID); err == nil {
					cred = full.Credential
				}
			}
			item["quark_cookie_set"] = quarkHasCookie(cred)
		}
		out = append(out, item)
	}
	writeJSON(w, http.StatusOK, out)
}

// quarkHasCookie 判断账号凭证里有没有网页 Cookie。
func quarkHasCookie(credential string) bool {
	var cr struct {
		WebCookie string `json:"web_cookie"`
	}
	if strings.TrimSpace(credential) == "" {
		return false
	}
	if err := json.Unmarshal([]byte(credential), &cr); err != nil {
		return false
	}
	return strings.TrimSpace(cr.WebCookie) != ""
}

func (s *Server) handleDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("非法账号 id"))
		return
	}
	s.pool.Evict(id)
	if err := s.st.DeleteAccount(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleAccountStats(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	snap, ok := s.pool.Snapshot(id)
	if !ok && s.st != nil {
		// 这个账号今天还没被调用过（Manager 没建起来），但库里有历史计数。
		// 不兜这一步的话，用户重启后进页面看到的是 0，会以为统计坏了。
		day := time.Now().Format("2006-01-02")
		if m, err := s.st.APICallsOn(r.Context(), day); err == nil {
			snap.Day = day
			snap.RequestsToday = m[id]
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"live": ok, "stats": snap})
}

// ==================== 扫码登录 ====================

type qrSession struct {
	login *driver115.QRLogin
	born  time.Time
	done  bool
	acct  *store.Account
}

func (s *Server) handleQRStart(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	login, err := driver115.StartQRLogin()
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	id := randToken()
	s.qrMu.Lock()
	s.qr[id] = &qrSession{login: login, born: time.Now()}
	s.qrMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"id":         id,
		"png":        "data:image/png;base64," + base64.StdEncoding.EncodeToString(login.PNG),
		"expires_in": 180,
	})
	_ = ctx
}

func (s *Server) handleQRPoll(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.qrMu.Lock()
	sess := s.qr[id]
	s.qrMu.Unlock()
	if sess == nil {
		writeErr(w, http.StatusNotFound, errors.New("二维码会话不存在或已过期"))
		return
	}
	if time.Since(sess.born) > 3*time.Minute {
		writeJSON(w, http.StatusOK, map[string]any{"status": "expired", "msg": "二维码已过期，请刷新"})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	status, msg, cred, err := sess.login.Poll(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	resp := map[string]any{"status": string(status), "msg": msg}
	if status == driver115.QRAllowed && cred != "" && !sess.done {
		acct, err := s.saveAccount(ctx, "115", "", cred)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		sess.done = true
		sess.acct = &acct
		resp["account"] = acct
	}
	writeJSON(w, http.StatusOK, resp)
}

// ==================== Cookie 登录 ====================

type cookieReq struct {
	Cookie string `json:"cookie"`
	Name   string `json:"name"`
}

func (s *Server) handleCookieLogin(w http.ResponseWriter, r *http.Request) {
	var req cookieReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	if strings.TrimSpace(req.Cookie) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请粘贴 Cookie"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cred, userName, err := driver115.LoginWithCookie(ctx, req.Cookie)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	name := req.Name
	if name == "" {
		name = userName
	}
	acct, err := s.saveAccount(ctx, "115", name, cred)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, acct)
}

func (s *Server) saveAccount(ctx context.Context, kind, name, cred string) (store.Account, error) {
	if name == "" {
		name = "115 账号"
	}
	id, err := s.st.CreateAccount(ctx, kind, name, cred)
	if err != nil {
		return store.Account{}, err
	}
	return store.Account{ID: id, Kind: kind, Name: name, CreatedAt: time.Now()}, nil
}

// ==================== 目录浏览 ====================

type browseEntry struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	accountID := pathID(r, "id")
	if accountID == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("非法账号 id"))
		return
	}
	cid := r.URL.Query().Get("cid")
	if cid == "" {
		cid = "0" // 115 的根目录
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	mgr, err := s.pool.Get(ctx, accountID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, errors.New("账号不存在，可能已被删除"))
			return
		}
		writeErr(w, statusOf(err), err)
		return
	}
	entries, err := mgr.ListAll(ctx, cid, 200)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}

	out := make([]browseEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, browseEntry{ID: e.ID, Name: e.Name, IsDir: e.IsDir, Size: e.Size})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{"cid": cid, "entries": out})
}

// ==================== 媒体库 ====================

func (s *Server) handleListLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.st.ListLibraries(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if libs == nil {
		libs = []store.Library{}
	}
	writeJSON(w, http.StatusOK, libs)
}

type createLibReq struct {
	AccountID      int64  `json:"account_id"`
	Name           string `json:"name"`
	RemoteCID      string `json:"remote_cid"`
	RemotePath     string `json:"remote_path"`
	LocalPath      string `json:"local_path"`
	CollectionType string `json:"collection_type"`
	KeepExt        *bool  `json:"keep_ext"`
	// Sources 是「这个库要挂的全部网盘目录」。
	//
	// 不传时按上面的 account_id/remote_cid 建单来源（老前端、bootstrap 都走这条）；
	// 传了就按它来，第一个来源顺带写进 libraries 的 account_id/remote_cid，
	// 让「主来源」这个概念对老代码仍然成立。
	Sources []libSourceReq `json:"sources"`
}

func (s *Server) handleCreateLibrary(w http.ResponseWriter, r *http.Request) {
	var req createLibReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请填媒体库名称"))
		return
	}

	// 归一来源：老调用方只给一个 account_id + remote_cid；新前端给 sources[]。
	// 两种写法都接受，后面一律按「至少一个来源」处理。
	srcs := make([]libSourceReq, 0, len(req.Sources)+1)
	for _, s := range req.Sources {
		s.RemoteCID = strings.TrimSpace(s.RemoteCID)
		if s.AccountID == 0 || s.RemoteCID == "" {
			continue
		}
		srcs = append(srcs, s)
	}
	if len(srcs) == 0 {
		srcs = append(srcs, libSourceReq{
			AccountID: req.AccountID, RemoteCID: req.RemoteCID, RemotePath: req.RemotePath,
		})
	}
	primary := srcs[0]
	req.AccountID, req.RemoteCID, req.RemotePath = primary.AccountID, primary.RemoteCID, primary.RemotePath

	if req.AccountID == 0 || req.RemoteCID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请选择网盘目录"))
		return
	}
	for _, src := range srcs {
		if _, err := s.st.GetAccount(r.Context(), src.AccountID); err != nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("账号 #%d 不存在", src.AccountID))
			return
		}
	}

	local := strings.TrimSpace(req.LocalPath)
	if local == "" {
		local = s.uniqueLocalPath(r.Context(), localDirName(req.Name))
	}
	local, err := safeUnder(s.cfg.MediaRoot, local)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := checkASCIIPath(local); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := os.MkdirAll(local, 0o750); err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Errorf("创建本地目录失败: %w", err))
		return
	}

	ct := req.CollectionType
	switch ct {
	case "movies", "tvshows", "mixed":
	default:
		ct = "movies"
	}
	keepExt := true
	if req.KeepExt != nil {
		keepExt = *req.KeepExt
	}

	id, err := s.st.CreateLibrary(r.Context(), store.Library{
		AccountID:      req.AccountID,
		Name:           req.Name,
		RemoteCID:      req.RemoteCID,
		RemotePath:     req.RemotePath,
		LocalPath:      local,
		CollectionType: ct,
		KeepExt:        keepExt,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 主来源由 CreateLibrary 一起写进去，其余来源逐条补上。
	// 单条失败不回滚整库：库已经建好了，用户可以在界面上补挂，
	// 而且把「夸克那一条失败了」演成「整个库没建成」更让人困惑。
	for _, src := range srcs[1:] {
		if _, err := s.st.AddSource(r.Context(), store.LibrarySource{
			LibraryID: id, AccountID: src.AccountID,
			RemoteCID: src.RemoteCID, RemotePath: src.RemotePath,
		}); err != nil {
			s.log.Warn("补挂来源失败", "lib", id, "account", src.AccountID, "err", err)
		}
	}
	lib, _ := s.st.GetLibrary(r.Context(), id)
	writeJSON(w, http.StatusOK, lib)
}

func (s *Server) handleDeleteLibrary(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if err := s.st.DeleteLibrary(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type syncReq struct {
	Domain      string `json:"domain"`
	DeleteStale *bool  `json:"delete_stale"`
	MinSize     *int64 `json:"min_size"`
	// Force 为真表示全量：内容没变也重写 .strm，让 Emby 整库重扫。
	// 不传时按「有的写、没变就跳过」处理（增量语义）。
	Force bool `json:"force"`
}

// syncOptions 组装一次同步的参数：接口传了就用传的，否则读设置里的默认值。
func (s *Server) syncOptions(ctx context.Context, lib store.Library, req syncReq) (syncsvc.Options, error) {
	domain := strings.TrimSpace(req.Domain)
	if domain == "" {
		domain = s.setting(ctx, "play_domain")
	}
	if domain == "" {
		return syncsvc.Options{}, errors.New("请先在设置里填播放地址（如 http://192.168.1.10:6086）")
	}
	// 同步会把地址写进成百上千个 .strm，这里再校验一次：
	// 前端对话框里手填的地址不一定走过设置页的规整逻辑。
	if normalized, err := normalizePlayDomain(domain); err != nil {
		return syncsvc.Options{}, err
	} else {
		domain = normalized
	}

	delStale := s.settingBool(ctx, "sync_delete_stale", true)
	if req.DeleteStale != nil {
		delStale = *req.DeleteStale
	}
	var minSize int64
	if req.MinSize != nil {
		minSize = *req.MinSize
	} else if v, ok := s.st.GetSetting(ctx, "sync_min_size"); ok {
		minSize, _ = strconv.ParseInt(v, 10, 64)
	}

	trigger := "incremental"
	if req.Force {
		trigger = "full"
	}
	return syncsvc.Options{
		Domain:      domain,
		KeepExt:     lib.KeepExt,
		DeleteStale: delStale,
		MinSize:     minSize,
		// 默认去重：网盘里同一部片常有多个副本，全写出来 Emby 就是几条同名空白条目
		Dedupe:  s.settingBool(ctx, "sync_dedupe", true),
		Force:   req.Force,
		Trigger: trigger,
	}, nil
}

// beginSync 用默认参数给某个库开一次同步（后台自动触发用，没有请求上下文）。
func (s *Server) beginSync(lib store.Library) (*syncsvc.Job, error) {
	opt, err := s.syncOptions(context.Background(), lib, syncReq{})
	if err != nil {
		return nil, err
	}
	// 自动触发的这轮在流水账里要能一眼认出来，否则和手动增量混在一起，
	// 用户会以为「我明明没点过，怎么多了一轮」。
	opt.Trigger = "auto"
	return s.sync.Start(lib, opt)
}

// watchSyncAndNotify 等同步跑完再发通知。
//
// 之所以在 HTTP 层轮询而不是让 sync 包回调：sync 不该知道「通知」这回事，
// 换个通知渠道也不该动同步逻辑。轮询一个内存里的 Job 每秒一次，代价可以忽略。
func (s *Server) watchSyncAndNotify(job *syncsvc.Job, lib store.Library) {
	go func() {
		for job.Running() {
			time.Sleep(time.Second)
		}
		p := job.Snapshot()
		switch p.Phase {
		case "done":
			s.pushEvent("sync_ok", notify.KindIngest,
				"✓ 同步完成 · "+lib.Name,
				fmt.Sprintf("目录 %d · 视频 %d · 写入 %d · 清理 %d · 失败 %d",
					p.Dirs, p.Files, p.Written, p.Removed, p.Failed))
		case "error":
			msg := p.Err
			if msg == "" {
				msg = p.Message
			}
			s.pushEvent("sync_fail", notify.KindSystem, "✕ 同步失败 · "+lib.Name, msg)
		}
		if p.Phase == "done" {
			s.autoScrapeAfterSync(lib)
		}
	}()
}

// autoScrapeAfterSync 在同步成功后按设置自动刮削，刮完再按设置自动补字幕。
//
// 之所以把「刮削 → 字幕」串成链条而不是并行：字幕要靠刮削产出的 IMDb ID，
// 没有 ID 就无从查字幕。串行的代价只是晚几分钟，换来的是字幕命中率。
func (s *Server) autoScrapeAfterSync(lib store.Library) {
	ctx := context.Background()
	if !s.loadScrapeConfig(ctx).Auto {
		return
	}
	job, err := s.startScrape(lib, s.scrapeOptions(ctx, false, false))
	if err != nil {
		s.log.Warn("自动刮削启动失败", "lib", lib.Name, "err", err)
		return
	}
	go func() {
		<-job.Done()
		if job.Snapshot().Phase != "done" {
			return
		}
		// 与补字幕并行：刷新元数据是 Emby 侧的事，和字幕互不依赖，
		// 串起来只会让「刮完到能看」多等半分钟。
		go s.refreshServerAfterScrape(job, lib)
		s.chainSubtitlesAfterScrape(lib, job)
	}()
}

func (s *Server) handleStartSync(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx := r.Context()
	lib, err := s.st.GetLibrary(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	var req syncReq
	_ = readJSON(r, &req) // 允许空 body

	opt, err := s.syncOptions(ctx, lib, req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	job, err := s.sync.Start(lib, opt)
	if err != nil {
		writeErr(w, http.StatusConflict, err)
		return
	}
	s.watchSyncAndNotify(job, lib)
	writeJSON(w, http.StatusAccepted, job.Snapshot())
}

func (s *Server) handleJob(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	j := s.sync.Job(id)
	if j == nil {
		writeJSON(w, http.StatusOK, map[string]any{"running": false, "phase": "idle"})
		return
	}
	writeJSON(w, http.StatusOK, j.Snapshot())
}

func (s *Server) handleCancelSync(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if j := s.sync.Job(id); j != nil {
		j.Cancel()
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	files, total, err := s.st.ListFiles(r.Context(), id, limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files, "total": total})
}

// handleAttachServer 在 Emby/Jellyfin 上建库并触发一次扫描。
func (s *Server) handleAttachServer(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	lib, err := s.st.GetLibrary(ctx, id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	srv, err := s.mediaServer(ctx)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	serverPath := s.mapServerPath(ctx, lib.LocalPath)
	created, err := srv.EnsureLibrary(ctx, lib.Name, lib.CollectionType, []string{serverPath})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	if err := s.st.SetLibraryServer(ctx, lib.ID, srv.Name(), created.ID); err != nil {
		s.log.Warn("记录媒体库映射失败", "err", err)
	}
	// 老库里可能残留「混合内容」类型，那种库永远扫不出 .strm（见 EmbyCollectionType）。
	// 我们改不了已有库的类型，只能提示用户去 Emby 后台删掉重建。
	mixed := strings.EqualFold(created.CollectionType, "mixed")
	if mixed {
		s.log.Warn("Emby 里这个库是「混合内容」类型，会扫不到 .strm，需删掉重建为「电影」",
			"library", lib.Name, "server_lib_id", created.ID)
	}
	if created.ID != "" {
		if err := srv.Refresh(ctx, created.ID); err != nil {
			s.log.Warn("触发扫描失败", "err", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"server":        srv.Name(),
		"server_path":   serverPath,
		"server_lib_id": created.ID,
		"library":       created,
		"mixed_warning": mixed,
	})
}

// ==================== 媒体服务器 ====================

func (s *Server) mediaServer(ctx context.Context) (mediaserver.Server, error) {
	cfg := mediaserver.Config{
		Kind:   s.setting(ctx, "server_kind"),
		URL:    s.setting(ctx, "server_url"),
		APIKey: s.setting(ctx, "server_key"),
	}
	return mediaserver.New(cfg)
}

// mapServerPath 处理「本机路径」与「媒体服务器看到的路径」不一致的情况。
// 两者挂同一个卷时留空即可（恒等映射）。
func (s *Server) mapServerPath(ctx context.Context, local string) string {
	from := s.setting(ctx, "server_path_from")
	to := s.setting(ctx, "server_path_to")
	if from == "" || to == "" {
		return path.Clean(filepath.ToSlash(local))
	}
	rel, err := filepath.Rel(from, local)
	if err != nil || strings.HasPrefix(rel, "..") {
		return path.Clean(filepath.ToSlash(local))
	}
	return path.Join(filepath.ToSlash(to), filepath.ToSlash(rel))
}

// ==================== 设置 ====================

var settingKeys = []string{
	"play_domain",
	"server_kind", "server_url", "server_key",
	// Emby 302 网关：开关 + UA 例外名单。它和播放代理共用一个端口，
	// 不再有自己的端口；老配置里的 emby_gateway_port 只用于兼容读取。
	// emby_gateway_mode 决定 PlaybackInfo 改写后把 Path 指向哪（三档，见 embygw/mode.go）。
	"emby_gateway_on", "emby_gateway_bypass", "emby_gateway_mode",
	"server_path_from", "server_path_to",
	"sync_keep_ext", "sync_delete_stale", "sync_min_size", "sync_dedupe",
	"imdb_api_key", "tmdb_api_key",
	// TVDB v4：播出日历拿它补「几点播 / 哪个电视网 / 分集」。
	// 两项都留空时日历自动降级成纯 TMDB，不是必填项。
	"tvdb_api_key", "tvdb_pin",
	// TVDB v4 接口根：留空走官方 https://api4.thetvdb.com/v4，填了就是自建反代。
	// 和密钥不同，这一项「留空保存 = 恢复官方」，不是不修改。
	"tvdb_base",
	// 播出日历的时刻增强源：tvmaze（默认，免 Key）/ tvdb / off。
	// TheTVDB v4 已不再向个人发免费项目 Key，所以默认值从 tvdb 换成了 tvmaze。
	"calendar_source", "tvmaze_base",
	// 豆瓣数据源开关（"0" 关，留空/其它都算开）。国产剧和国内档期只有豆瓣有，
	// 默认开；部署在境外、豆瓣整站被墙时可以关掉，免得每次搜索白等十几秒。
	// douban_base 是豆瓣反代根（留空走官方），和 tmdb_base 同一套语义：
	// 留空 = 恢复官方直连。
	"douban_enabled", "douban_base",
	// TMDB 反代：tmdb_base 是 v3 接口根（如 https://proxy.example/3），
	// tmdb_image_base 可选，留空时按 /t/p 规则从 tmdb_base 推导。
	// 两个都留空 = 回官方直连。
	"tmdb_base", "tmdb_image_base",
	"scrape_config", "subtitle_config",
	"net_proxy", "tg_channels", "res_sites", "res_sites_custom",
	// 资源搜索里「存到 115」的落点：目录 id + 给人看的路径（面包屑提示用）
	"res_save_cid", "res_save_path",
	// 夸克：quark_save_fid 是「存到夸克」的落点（夸克目录 fid，0 = 根），
	// quark_web_base 是网页接口反代根，quark_tv_base 是 TV 开放接口反代根。
	// 两个 base 留空都表示走官方地址。
	"quark_save_fid", "quark_web_base", "quark_tv_base",
}

// applyTMDBUpstream 把设置里的反代地址同步给 tmdb 包。
//
// 启动时调一次，用户在设置页改完也要立刻调：电影墙每次请求新建 Client、
// 演员刮削则缓存 Client，只改设置不刷新的话，得重启容器才生效。
func (s *Server) applyTMDBUpstream(ctx context.Context) {
	tmdb.SetUpstream(s.setting(ctx, "tmdb_base"), s.setting(ctx, "tmdb_image_base"))
}

// validAPIBase 校验自定义接口根地址。空串合法（= 恢复官方直连）。
//
// 必须带 http(s) scheme：只填域名的话 c.Base + "/tv/123" 会拼成相对路径，
// Go 的 http.NewRequest 不报错、发出去才失败，排查起来很费劲。
// example 只用于错误提示，让 TMDB / TVDB 各自给出对得上的示例。
func validAPIBase(v, example string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "", nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "", fmt.Errorf("地址要像 %s（必须带 http:// 或 https://）", example)
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("接口地址不要带查询参数或 # 片段")
	}
	return v, nil
}

func validTMDBBase(v string) (string, error) {
	return validAPIBase(v, "https://your-domain/3")
}

func validTVDBBase(v string) (string, error) {
	return validAPIBase(v, "https://api4.thetvdb.com/v4")
}

func validTVMazeBase(v string) (string, error) {
	return validAPIBase(v, "https://api.tvmaze.com")
}

func validDoubanBase(v string) (string, error) {
	return validAPIBase(v, "https://m.douban.com")
}

// secretSettingKeys 是不回传明文、且「留空 = 不修改」的键。
var secretSettingKeys = []string{"server_key", "imdb_api_key", "tmdb_api_key", "tvdb_api_key", "tvdb_pin"}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]string{}
	for _, k := range settingKeys {
		out[k] = s.setting(ctx, k)
	}
	// 密钥不回传给浏览器：前端只需要知道「有没有配」，不需要那串值本身。
	// 真要测连接，后端会用库里存的那份（见 handleTestServer）。
	for _, k := range secretSettingKeys {
		if v := out[k]; v != "" {
			out[k+"_masked"] = "已配置（" + strconv.Itoa(len(v)) + " 位）"
			out[k+"_set"] = "1"
			out[k] = ""
		}
	}
	// 刮削与字幕的配置存成 JSON 字符串，但前端要的是对象——在这里解开，
	// 免得每个页面都自己 JSON.parse 一遍（也就少一处解析失败的可能）。
	resp := make(map[string]any, len(out)+2)
	for k, v := range out {
		resp[k] = v
	}
	resp["scrape"] = s.loadScrapeConfig(ctx)
	resp["subtitle"] = s.loadSubtitleConfig(ctx)
	// 回显「此刻真正生效」的上游：tmdb_image_base 留空时图片根是从
	// tmdb_base 推导出来的，光看设置项猜不到，界面上得能核对一次。
	api, img := tmdb.CurrentUpstream()
	resp["tmdb_upstream"] = map[string]string{"api": api, "image": img}
	// TVDB 同理：留空时真正生效的是官方 v4 地址，界面上要能核对，
	// 否则用户以为「没填 = 没在用」，其实日历一直在走官方。
	tvBase := strings.TrimSpace(s.setting(ctx, "tvdb_base"))
	if tvBase == "" {
		tvBase = tvdb.DefaultBase
	}
	resp["tvdb_upstream"] = map[string]string{"api": tvBase}
	// TVMaze 同理：留空时生效的是官方公共 API，界面上要能核对；
	// calendar_source 留空由 calendar.source() 兜成 tvmaze，这里也回显出来。
	tzBase := strings.TrimSpace(s.setting(ctx, "tvmaze_base"))
	if tzBase == "" {
		tzBase = tvmaze.DefaultBase
	}
	resp["tvmaze_upstream"] = map[string]string{"api": tzBase}
	src := strings.TrimSpace(s.setting(ctx, "calendar_source"))
	if src == "" {
		src = "tvmaze"
	}
	resp["calendar_source"] = src
	// 代理回显「此刻真正生效」的值：设置页留空但容器里带 HTTPS_PROXY 时，
	// 界面也得看得见，否则用户会以为自己填的代理没起作用。
	configured := strings.TrimSpace(s.setting(ctx, "net_proxy"))
	effective := netproxy.Get()
	resp["net_proxy_configured"] = configured
	resp["net_proxy_effective"] = effective
	// 容器里把 127.0.0.1 改写成宿主机网关时，界面上要说明「为什么显示的地址和填的不一样」。
	resp["net_proxy_rewritten"] = configured != "" && effective != configured
	// 302 网关：端口是配置值，真正有没有在听只有进程知道，两样都给出去。
	resp["emby_gateway"] = s.gatewayStatus()
	switch {
	case effective == "":
		resp["net_proxy_source"] = ""
	case configured != "":
		resp["net_proxy_source"] = "设置页"
	default:
		resp["net_proxy_source"] = "环境变量"
	}
	writeJSON(w, http.StatusOK, resp)
}

// boolSetting 把各种「开/关」的写法收敛成 "0" / "1"。
//
// 库里存字符串（设置表只有一个 value 列），所以开关必须有个统一形态：
// 前端传 true / "on" / 1 都当开；只有明确表示关的值才存 "0"。
func boolSetting(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return "0"
	}
	return "1"
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	ctx := r.Context()
	isSecret := map[string]bool{}
	for _, k := range secretSettingKeys {
		isSecret[k] = true
	}
	// 播放地址要在写库之前规整：补 http://、去尾斜杠、挡掉 127.0.0.1。
	// 校验放在这里而不是前端，是因为「用 curl 直接改配置」同样要受约束。
	if v, ok := req["play_domain"]; ok && strings.TrimSpace(v) != "" {
		normalized, err := normalizePlayDomain(v)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		req["play_domain"] = normalized
	}
	// 代理地址存之前先验一遍格式：存进去一个错的，网络检测会整片飘红，
	// 用户很难想到是这里写错了。
	if v, ok := req["net_proxy"]; ok && strings.TrimSpace(v) != "" {
		norm, valid := validProxy(v)
		if !valid {
			writeErr(w, http.StatusBadRequest, errors.New("代理地址格式不对，应该像 http://192.168.1.10:7890"))
			return
		}
		req["net_proxy"] = norm
	}
	// 开关类设置统一成 "0" / "1"：前端可能传 true/false，也可能是勾选框的 "on"。
	if v, ok := req["douban_enabled"]; ok {
		req["douban_enabled"] = boolSetting(v)
	}
	// 时刻数据源只能是白名单里的三种：写错一个字母不该让日历静默少一列时刻。
	// 空串也收下，落库成 tvmaze（默认）。
	if v, ok := req["calendar_source"]; ok {
		norm := strings.ToLower(strings.TrimSpace(v))
		if norm == "" {
			norm = "tvmaze"
		}
		if norm != "tvmaze" && norm != "tvdb" && norm != "off" {
			writeErr(w, http.StatusBadRequest, errors.New("日历时刻数据源只能是 tvmaze / tvdb / off"))
			return
		}
		req["calendar_source"] = norm
	}
	// 反代地址属于「留空 = 清空」（不是密钥那种留空 = 不修改），
	// 所以校验要收下空串，写库时照常覆盖。
	for k, check := range map[string]func(string) (string, error){
		"tmdb_base":       validTMDBBase,
		"tmdb_image_base": validTMDBBase,
		"tvdb_base":       validTVDBBase,
		"tvmaze_base":     validTVMazeBase,
		"douban_base":     validDoubanBase,
	} {
		if v, ok := req[k]; ok {
			norm, err := check(v)
			if err != nil {
				writeErr(w, http.StatusBadRequest, fmt.Errorf("%s 格式不对：%w", k, err))
				return
			}
			req[k] = norm
		}
	}

	oldDomain := s.setting(ctx, "play_domain")
	for _, k := range settingKeys {
		v, ok := req[k]
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		// 密钥留空表示「不改」，否则前端每次保存设置都会把已存的 key 冲掉。
		if isSecret[k] && v == "" {
			continue
		}
		if err := s.st.SetSetting(ctx, k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}

	// 立刻生效：电影墙 / 演员刮削 / 订阅都在下一次请求用新地址，不用重启容器。
	s.applyTMDBUpstream(ctx)
	// 夸克 TV 接口根同理（改了反代地址，下一次扫码/列目录就用新的）。
	s.applyQuarkUpstream(ctx)
	// 302 网关：端口或上游改了就按新配置重绑，用户不用为了改一个端口重启容器。
	s.applyGateway(ctx)
	// 代理：推给进程级开关，所有包级单例 Client 的下一次请求就换过去。
	// 少了这一句，设置页保存完只有豆瓣/资源搜索生效，电影墙仍直连 ——
	// 这正是「代理设置不起作用」的直接原因。
	s.applyNetProxy(ctx)

	// 地址变了就顺手把已有 .strm 改过来。
	// 不这么做的话，用户改完设置会发现「页面显示新地址、影片还是播不了」——
	// 因为磁盘上的 .strm 还是旧地址，只有重新同步才会更新。
	resp := map[string]any{"ok": true}
	if newDomain, ok := req["play_domain"]; ok {
		if nd := strings.TrimSpace(newDomain); nd != "" && nd != oldDomain {
			if files, changed, err := s.rewritePlayDomain(ctx, nd); err == nil {
				resp["rewritten"] = changed
				resp["scanned"] = files
				if hint := domainHint(nd); hint != "" {
					resp["hint"] = hint
				}
			} else {
				s.log.Warn("改写 .strm 播放地址失败", "err", err)
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

type testServerReq struct {
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	APIKey string `json:"api_key"`
}

func (s *Server) handleTestServer(w http.ResponseWriter, r *http.Request) {
	var req testServerReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	// 页面上留空表示「用已保存的那份」，省得每次都去后台抄 key
	ctx0 := r.Context()
	kind, rawURL, key := req.Kind, req.URL, req.APIKey
	if kind == "" {
		kind = s.setting(ctx0, "server_kind")
	}
	if rawURL == "" {
		rawURL = s.setting(ctx0, "server_url")
	}
	if key == "" {
		key = s.setting(ctx0, "server_key")
	}
	srv, err := mediaserver.New(mediaserver.Config{Kind: kind, URL: rawURL, APIKey: key})
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	if err := srv.Ping(ctx); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	libs, _ := srv.Libraries(ctx)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "libraries": len(libs)})
}

// ==================== 连接媒体服务器（网页里一键配好） ====================

// handleServerStatus 汇报「媒体服务器现在是什么状态」：
// 配没配、连不连得上、上面有几个库。设置页打开时调它，用户不用猜。
func (s *Server) handleServerStatus(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
	defer cancel()

	kind := s.setting(ctx, "server_kind")
	if kind == "" {
		kind = "emby"
	}
	url := s.setting(ctx, "server_url")
	key := s.setting(ctx, "server_key")

	out := map[string]any{
		"kind":       kind,
		"url":        url,
		"configured": url != "" && key != "",
		"connected":  false,
		"libraries":  0,
	}
	if url == "" || key == "" {
		writeJSON(w, http.StatusOK, out)
		return
	}
	srv, err := mediaserver.New(mediaserver.Config{Kind: kind, URL: url, APIKey: key})
	if err != nil {
		writeJSON(w, http.StatusOK, out)
		return
	}
	if info, err := mediaserver.NewAdmin(url).PublicInfo(ctx); err == nil {
		out["server_name"] = info.ServerName
		out["version"] = info.Version
	} else {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	if libs, err := srv.Libraries(ctx); err == nil {
		out["connected"] = true
		out["libraries"] = len(libs)
	} else {
		out["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, out)
}

type connectServerReq struct {
	Kind     string `json:"kind"`
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

// handleConnectServer 用管理员账号密码登进 Emby/Jellyfin，自动建一个专属 API Key
// 并写进设置。
//
// 为什么需要它：让用户去后台「高级 → API 密钥」手抄一串 32 位十六进制，
// 是这套部署里最容易卡住的一步（地址填错、抄漏一位、忘了保存）。
// 密码只在这一次请求里用，不落库、不打日志。
func (s *Server) handleConnectServer(w http.ResponseWriter, r *http.Request) {
	var req connectServerReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = "emby"
	}
	base := mediaserver.NormalizeBase(req.URL)
	if base == "" {
		writeErr(w, http.StatusBadRequest, errors.New("先填媒体服务器地址，例如 http://192.168.1.10:8096"))
		return
	}
	if strings.TrimSpace(req.Username) == "" {
		writeErr(w, http.StatusBadRequest, errors.New("填一下媒体服务器的管理员账号"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	admin := mediaserver.NewAdmin(base)
	info, err := admin.PublicInfo(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, fmt.Errorf("连不上 %s：%w（容器里访问同一台机器上的 Emby，地址要写 http://emby:8096 这种容器能解析的）", base, err))
		return
	}

	// 还没走过启动向导时，顺手用这组账号密码把向导走完 —— 少一步就少一个卡点。
	finishedWizard := true
	if done, werr := admin.WizardCompleted(ctx); werr == nil && !done {
		if req.Password == "" {
			writeErr(w, http.StatusBadRequest, errors.New("这台媒体服务器还没初始化，需要填管理员密码，我用它来创建管理员账号"))
			return
		}
		if err := admin.CompleteWizard(ctx, req.Username, req.Password, "zh-CN", "CN"); err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		finishedWizard = false
	}

	// 刚走完向导时服务端要缓一两秒才接受登录，给几次重试
	token, err := admin.Authenticate(ctx, req.Username, req.Password)
	if err != nil {
		time.Sleep(time.Second)
		token, err = admin.Authenticate(ctx, req.Username, req.Password)
	}
	if err != nil {
		writeErr(w, http.StatusUnauthorized, fmt.Errorf("登录失败：%w（账号密码是媒体服务器后台的管理员账号，不是 strm115 的）", err))
		return
	}

	key, err := admin.EnsureAPIKey(ctx, token, "strm115")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	for k, v := range map[string]string{"server_kind": kind, "server_url": base, "server_key": key} {
		if err := s.st.SetSetting(ctx, k, v); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
	}

	srv, err := mediaserver.New(mediaserver.Config{Kind: kind, URL: base, APIKey: key})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	libs, _ := srv.Libraries(ctx)
	s.log.Info(fmt.Sprintf("已连接媒体服务器 %s（%s %s，%d 个库）", base, info.ServerName, info.Version, len(libs)))

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"kind":        kind,
		"url":         base,
		"server_name": info.ServerName,
		"version":     info.Version,
		"libraries":   len(libs),
		"initialized": finishedWizard,
		"key_preview": maskSecret(key),
	})
}

func maskSecret(s string) string {
	if len(s) <= 6 {
		return "***"
	}
	return s[:3] + "…" + s[len(s)-3:]
}

// ==================== 杂项 ====================

func statusOf(err error) int {
	switch pan.KindOf(err) {
	case pan.ErrCooling:
		return http.StatusServiceUnavailable
	case pan.ErrRisk:
		return http.StatusTooManyRequests
	case pan.ErrAuth:
		return http.StatusForbidden
	case pan.ErrNotFound:
		return http.StatusNotFound
	case pan.ErrUnsupported:
		return http.StatusNotImplemented
	case pan.ErrRejected:
		return http.StatusUnprocessableEntity
	default:
		return http.StatusBadGateway
	}
}

// safeUnder 保证路径落在媒体根目录内。
// 网盘里的目录名可能很想让人穿越（../../etc），这是唯一的防线。
func safeUnder(root, p string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(absRoot, abs)
	if err != nil {
		return "", errors.New("路径非法")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("目录必须位于媒体根 %s 之内", absRoot)
	}
	return abs, nil
}

// zhDirHints 常见中文库名 → 英文目录名。
//
// 不做音译，只给最常出现的几个词一个像样的默认值：
// 一个叫「电影」的库落到 /media/lib-1a2b 上，用户会以为坏了。
var zhDirHints = []struct {
	dir  string
	keys []string
}{
	{"movies", []string{"电影", "影片", "影视", "电影库"}},
	{"tv", []string{"电视剧", "剧集", "连续剧", "美剧", "英剧", "韩剧", "日剧", "国产剧", "短剧"}},
	{"anime", []string{"动漫", "动画", "番剧", "日番", "番"}},
	{"variety", []string{"综艺"}},
	{"documentary", []string{"纪录片", "记录片", "纪录"}},
	{"music", []string{"音乐", "演唱会"}},
	{"sports", []string{"体育", "赛事"}},
	{"kids", []string{"儿童", "少儿", "亲子"}},
}

// localDirName 把媒体库名转成一个纯英文的目录名。
//
// 中文目录名本身 Emby 是能扫的（实测 /media/中文库 正常入库），但落地路径
// 保持全英文更省心：容器 uid、Windows bind mount、网盘同步盘、各种播放器
// 对非 ASCII 路径的编码处理各不相同，出问题时最难查。库的显示名仍然是中文，
// 只有磁盘上的目录名用英文，用户看不出区别。
//
// 取名顺序：
//  1. 名字里有中文 → 先查 zhDirHints（「电影」→ movies）
//  2. 名字里本来就有英文数字 → 直接用（「My Library」→ my-library）
//  3. 都没有 → 用名字哈希压成 lib-xxxx，保证同名稳定（重建库不换路径）
func localDirName(name string) string {
	if !hasNonASCII(name) {
		return asciiSlug(name)
	}
	for _, h := range zhDirHints {
		for _, k := range h.keys {
			if strings.Contains(name, k) {
				// 「电影 2024」→ movies-2024，免得两个年份撞一个目录
				if tail := asciiSlug(dropNonASCII(name)); tail != "" {
					return h.dir + "-" + tail
				}
				return h.dir
			}
		}
	}
	if s := asciiSlug(name); s != "" {
		return s
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("lib-%04x", h.Sum32()&0xffff)
}

func hasNonASCII(s string) bool {
	for _, r := range s {
		if r > 0x7f {
			return true
		}
	}
	return false
}

func dropNonASCII(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r <= 0x7f {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// asciiSlug 只留小写字母和数字，其余折成单个连字符。
func asciiSlug(s string) string {
	var b strings.Builder
	dash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case r >= 'A' && r <= 'Z':
			b.WriteRune(r + ('a' - 'A'))
			dash = false
		default:
			if b.Len() > 0 && !dash {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

// uniqueLocalPath 拼出默认本地目录，重名时加 -2 / -3。
// 两个都叫「电影」的库不能写进同一个目录，否则同步会互相删文件。
func (s *Server) uniqueLocalPath(ctx context.Context, base string) string {
	taken := map[string]bool{}
	if libs, err := s.st.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			taken[filepath.Clean(l.LocalPath)] = true
		}
	}
	cand := filepath.Join(s.cfg.MediaRoot, base)
	if abs, err := filepath.Abs(cand); err == nil {
		cand = abs
	}
	if !taken[cand] {
		return cand
	}
	for i := 2; i < 100; i++ {
		next := filepath.Join(filepath.Dir(cand), fmt.Sprintf("%s-%d", base, i))
		if !taken[next] {
			return next
		}
	}
	return cand
}

// checkASCIIPath 要求库根路径保持全英文（见 localDirName 的理由）。
func checkASCIIPath(p string) error {
	for _, r := range p {
		if r > 0x7f {
			return fmt.Errorf("本地目录请只用英文和数字（这里出现了「%c」）；库的显示名可以照旧用中文", r)
		}
	}
	return nil
}

var _ = panpool.Pool{}
