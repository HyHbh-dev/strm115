package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
)

// 一个媒体库挂多个网盘目录（v0.2.7）。
//
// 场景：电影一半在 115、一半在夸克。以前只能建两个库、Emby 里出现两个分类；
// 现在两边的目录都挂到同一个库上，.strm 写进同一个本地目录，Emby 只认一个库。
// 唯一的差别是 .strm 里那行地址：115 是 /d/{pickcode}，夸克是 /d/q/{fid}。
//
// 这几条接口只管「挂什么」，真正的遍历在 internal/sync。

type libSourceReq struct {
	AccountID  int64  `json:"account_id"`
	RemoteCID  string `json:"remote_cid"`
	RemotePath string `json:"remote_path"`
}

func (s *Server) handleListLibrarySources(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if _, err := s.st.GetLibrary(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	srcs, err := s.st.ListSources(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": srcs})
}

func (s *Server) handleAddLibrarySource(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	var req libSourceReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	req.RemoteCID = strings.TrimSpace(req.RemoteCID)
	if req.AccountID == 0 || req.RemoteCID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请选择网盘账号和目录"))
		return
	}
	if _, err := s.st.GetLibrary(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, errors.New("媒体库不存在"))
		return
	}
	if _, err := s.st.GetAccount(r.Context(), req.AccountID); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("账号不存在"))
		return
	}

	sid, err := s.st.AddSource(r.Context(), store.LibrarySource{
		LibraryID:  id,
		AccountID:  req.AccountID,
		RemoteCID:  req.RemoteCID,
		RemotePath: req.RemotePath,
	})
	if errors.Is(err, store.ErrDuplicate) {
		writeErr(w, http.StatusConflict, errors.New("这个目录已经挂在这个库上了"))
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": sid})
}

func (s *Server) handleUpdateLibrarySource(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	sid := pathID(r, "sid")
	var req libSourceReq
	if err := readJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体格式错误"))
		return
	}
	req.RemoteCID = strings.TrimSpace(req.RemoteCID)
	if req.RemoteCID == "" {
		writeErr(w, http.StatusBadRequest, errors.New("请选择网盘目录"))
		return
	}
	if err := s.st.UpdateSource(r.Context(), id, sid, req.RemoteCID, req.RemotePath); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteLibrarySource(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	sid := pathID(r, "sid")
	if err := s.st.DeleteSource(r.Context(), id, sid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, errors.New("这个来源不存在"))
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	// 摘掉来源不动已经生成的 .strm：Emby 里的条目还在，能播；
	// 下一轮同步才会把它们当成「网盘侧已消失」清掉。这给了用户一次反悔的机会。
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// sourceForCID 找出 cid 属于这个库的哪个来源。
//
// 「目录反推账号」是多来源之后必须有的能力：用户在夸克的目录里点「加入媒体库」，
// 后端不能拿 115 的凭证去 Stat 夸克的 fid。
func (s *Server) sourceForCID(ctx context.Context, lib store.Library, cid string) (store.LibrarySource, bool) {
	srcs, err := s.st.ListSources(ctx, lib.ID)
	if err != nil {
		return store.LibrarySource{}, false
	}
	for _, src := range srcs {
		if src.RemoteCID == cid {
			return src, true
		}
	}
	return store.LibrarySource{}, false
}

// accountForDir 是「这个目录该用哪个账号去读」的统一入口。
//
// 先按来源精确匹配；匹配不到（用户选的是库内的子目录）就按账号参数走，
// 再不行退回主来源——子目录在哪个账号下，只有 Stat 一遍才知道，而调用方
// 手里通常已经带着 account_id（目录选择器就是从某个账号里点出来的）。
func (s *Server) accountForDir(ctx context.Context, lib store.Library, cid string, hint int64) int64 {
	if src, ok := s.sourceForCID(ctx, lib, cid); ok {
		return src.AccountID
	}
	if hint > 0 {
		return hint
	}
	if len(lib.Sources) > 0 {
		return lib.Sources[0].AccountID
	}
	return lib.AccountID
}

// withLibraryTimeout 给需要多次网盘请求的接口一个统一的上限。
func withLibraryTimeout(r *http.Request, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), d)
}

// queryID 读一个可选的整数查询参数（缺省/非法都当 0）。
func queryID(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get(key)), 10, 64)
	return n
}

// libHasSource 判断「这个账号的这个目录」是不是该库的来源之一。
//
// 转存完要不要顺手跑一轮同步、目录该显示成「媒体库 X」还是裸 id，
// 都靠它判断。一个库挂了两个网盘时不能只看主账号。
func libHasSource(lib store.Library, accountID int64, cid string) bool {
	if cid == "" {
		return false
	}
	if lib.AccountID == accountID && lib.RemoteCID == cid {
		return true
	}
	for _, src := range lib.Sources {
		if src.AccountID == accountID && src.RemoteCID == cid {
			return true
		}
	}
	return false
}

// libHasDir 只看目录，不看账号（显示用）。
func libHasDir(lib store.Library, cid string) bool {
	if cid == "" {
		return false
	}
	if lib.RemoteCID == cid {
		return true
	}
	for _, src := range lib.Sources {
		if src.RemoteCID == cid {
			return true
		}
	}
	return false
}
