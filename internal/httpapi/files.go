package httpapi

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"strm115/internal/notify"
	"strm115/internal/store"
	"strm115/internal/strm"
)

// ==================== 网盘文件浏览 ====================

// fileEntry 是网盘里的一行。Strm 非空表示这个文件已经在我们台账里，
// 也就是「本地已经有对应的 .strm 了」——这是文件页最该显示的状态，
// 它直接把「网盘里有什么」和「媒体库里有没有」对上了。
type fileEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	IsDir   bool   `json:"is_dir"`
	Size    int64  `json:"size"`
	MTime   string `json:"mtime"`
	Strm    string `json:"strm,omitempty"`
	Library string `json:"library,omitempty"`
}

func (s *Server) handleFilesBrowse(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	accountID, _ := strconv.ParseInt(strings.TrimSpace(q.Get("account_id")), 10, 64)
	if accountID <= 0 {
		accountID = s.st.FirstAccountID(r.Context())
	}
	if accountID <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("还没有登录网盘账号，请先到「账号与媒体库」登录 115 或夸克TV"))
		return
	}
	cid := strings.TrimSpace(q.Get("cid"))
	if cid == "" {
		cid = "0"
	}
	dirsOnly := q.Get("dirs_only") == "1"

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

	ids := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir {
			ids = append(ids, e.ID)
		}
	}
	refs, err := s.st.FilesByFileIDs(ctx, accountID, ids)
	if err != nil {
		// 台账查不到只是少个角标，不该让整个目录打不开
		s.log.Warn("反查台账失败", "account", accountID, "err", err)
		refs = map[string]store.FileRef{}
	}
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		s.log.Warn("读取媒体库失败，文件页不显示库名", "err", err)
	}
	libName := make(map[int64]string, len(libs))
	for _, l := range libs {
		libName[l.ID] = l.Name
	}

	out := make([]fileEntry, 0, len(entries))
	for _, e := range entries {
		if dirsOnly && !e.IsDir {
			continue
		}
		fe := fileEntry{
			ID: e.ID, Name: e.Name, IsDir: e.IsDir, Size: e.Size,
			MTime: formatTime(e.ModTime),
		}
		if ref, ok := refs[e.ID]; ok {
			fe.Strm = ref.RelPath
			fe.Library = libName[ref.LibraryID]
		}
		out = append(out, fe)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].IsDir != out[j].IsDir {
			return out[i].IsDir
		}
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"account_id": accountID,
		"cid":        cid,
		"entries":    out,
	})
}

// handleLibraryRelDir 算出某个网盘目录相对「媒体库根目录」的路径。
//
// 为什么不让前端拼：网盘里同名的目录可以有好几层（电影/电影/华语电影），
// 库里存的 remote_path 可能只剩最后一级（"电影"），前端拿它做前缀剥离
// 就会把层级拼错，结果 .strm 落进 电影/华语电影/… 而不是 华语电影/…，
// 台账和下次同步对不上，Emby 里就会出现两份。
//
// 从 cid 往上走父目录直到库根，才是唯一可靠的做法：走的是 115 自己记的
// ParentID，跟名字无关。目录深度通常 3～5 层，几次 Stat 换一次正确。
func (s *Server) handleLibraryRelDir(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	cid := strings.TrimSpace(r.URL.Query().Get("cid"))
	lib, err := s.st.GetLibrary(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	if cid == "" || cid == lib.RemoteCID {
		writeJSON(w, http.StatusOK, map[string]any{"rel_dir": ""})
		return
	}
	if cid == "0" {
		writeErr(w, http.StatusBadRequest, errors.New("网盘根目录不在这个媒体库里，请先进入库内的目录"))
		return
	}

	// 一个库可能挂了 115 和夸克两个目录，库根也就不止一个。
	// 往上走到任何一个根都算「在库里」。
	roots := map[string]bool{lib.RemoteCID: true, "": true}
	for _, src := range lib.Sources {
		roots[src.RemoteCID] = true
	}

	ctx, cancel := withLibraryTimeout(r, 60*time.Second)
	defer cancel()
	accountID := s.accountForDir(ctx, lib, cid, queryID(r, "account_id"))
	mgr, err := s.pool.Get(ctx, accountID)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}

	var names []string
	cur := cid
	for depth := 0; depth < 16; depth++ {
		e, err := mgr.Stat(ctx, cur)
		if err != nil {
			// 往上走到库外（或者目录已被删）时 115 会回参数错误，
			// 对用户来说这只有一种含义：这个目录不属于这个库。
			break
		}
		names = append([]string{strm.Sanitize(e.Name)}, names...)
		if roots[e.ParentID] {
			writeJSON(w, http.StatusOK, map[string]any{"rel_dir": strings.Join(names, "/")})
			return
		}
		if e.ParentID == "" || e.ParentID == cur {
			break
		}
		cur = e.ParentID
	}
	writeErr(w, http.StatusBadRequest, errors.New("这个目录不在该媒体库的范围内，请进入库里的子目录再操作"))
}

// ==================== 网盘文件操作 ====================

type fileMoveReq struct {
	AccountID int64    `json:"account_id"`
	IDs       []string `json:"ids"`
	ToCID     string   `json:"to_cid"`
}

// handleFilesMove 把选中的条目移到另一个网盘目录。
//
// 只动网盘，不碰本地：下一轮同步会把 .strm 挪到新位置并删掉旧的
// （台账里记着 file_id，改名/移动认得出来）。这是「同步只读网盘」这条
// 规矩的例外——但它是用户显式发起的整理动作，不是同步顺手做的事。
func (s *Server) handleFilesMove(w http.ResponseWriter, r *http.Request) {
	var req fileMoveReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	req.ToCID = strings.TrimSpace(req.ToCID)
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("没有选中文件"))
		return
	}
	if req.ToCID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请选择目标目录"))
		return
	}
	if len(req.IDs) > 500 {
		writeErr(w, http.StatusBadRequest, errors.New("一次最多移动 500 项"))
		return
	}
	for _, id := range req.IDs {
		if id == req.ToCID {
			writeErr(w, http.StatusBadRequest, errors.New("不能把目录移动到它自己里面"))
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	mgr, err := s.pool.Get(ctx, req.AccountID)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	moved, err := mgr.Move(ctx, req.ToCID, req.IDs...)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	s.log.Info("网盘文件移动", "account", req.AccountID, "moved", moved, "to", req.ToCID)
	writeJSON(w, http.StatusOK, map[string]any{"moved": moved, "to_cid": req.ToCID})
}

type fileRemoveReq struct {
	AccountID int64    `json:"account_id"`
	IDs       []string `json:"ids"`
}

// handleFilesRemove 删除网盘条目（115 是进回收站，可以恢复）。
//
// 本地 .strm 这里不删：删网盘和删本地是两件事，把它们绑在一起做过一次
// 就再也不敢用了。本地残留交给「增量同步（勾选删除失效）+ 孤儿清理」。
func (s *Server) handleFilesRemove(w http.ResponseWriter, r *http.Request) {
	var req fileRemoveReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不合法"))
		return
	}
	if len(req.IDs) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("没有选中文件"))
		return
	}
	if len(req.IDs) > 200 {
		writeErr(w, http.StatusBadRequest, errors.New("一次最多删除 200 项"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	mgr, err := s.pool.Get(ctx, req.AccountID)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	removed, err := mgr.Remove(ctx, req.IDs...)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}
	s.log.Warn("网盘文件已删除（进回收站）", "account", req.AccountID, "count", removed)
	// 这一步只把网盘文件放进回收站，本地 .strm 要等下一轮同步才清掉 ——
	// 中间那段时间墙面上还看得到它。不通知的话用户会以为没删成功。
	s.pushEvent("delete", notify.KindDelete, "🗑 网盘已删除",
		strconv.Itoa(removed)+" 个文件 · 已进 115 回收站")
	writeJSON(w, http.StatusOK, map[string]any{
		"removed": removed,
		"hint":    "已移入 115 回收站；本地 STRM 会在下一轮「删除失效」同步或孤儿清理时清掉",
	})
}
