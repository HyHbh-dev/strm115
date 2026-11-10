package httpapi

// 系统配置 → EMBY 管理用到的两个只读/轻操作接口。
//
// 设置页以前那个 tab 是纯摆设：写死四个库名和条目数，点「测试」只是
// setTimeout 弹个成功。后端其实早就有完整的 mediaserver.Server
// （Libraries / Refresh / EnsureLibrary 都实现了），这里只是把它暴露出来。

import (
	"context"
	"net/http"
	"strings"
	"time"
)

// serverLibraryView 是给前端的一行媒体库。
// 条目数不在 Libraries() 里，硬凑一个数字只会又变成假数据，所以不返回。
type serverLibraryView struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Type   string   `json:"type"`
	Paths  []string `json:"paths"`
	Linked bool     `json:"linked"` // 路径是否落在本项目的媒体目录里
}

// handleServerLibraries 列出媒体服务器上真实的媒体库。
func (s *Server) handleServerLibraries(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	out := map[string]any{"ok": false, "libraries": []serverLibraryView{}}
	url, key := strings.TrimSpace(s.setting(ctx, "server_url")), strings.TrimSpace(s.setting(ctx, "server_key"))
	if url == "" || key == "" {
		out["error"] = "还没连接媒体服务器：填地址与 API Key，或者用管理员账号一键连接"
		writeJSON(w, http.StatusOK, out)
		return
	}
	srv, err := s.mediaServer(ctx)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	libs, err := srv.Libraries(ctx)
	if err != nil {
		out["error"] = err.Error()
		writeJSON(w, http.StatusOK, out)
		return
	}
	views := make([]serverLibraryView, 0, len(libs))
	for _, l := range libs {
		views = append(views, serverLibraryView{
			ID: l.ID, Name: l.Name, Type: l.CollectionType, Paths: l.Paths,
			Linked: libraryLinked(ctx, s, l.Paths),
		})
	}
	out["ok"] = true
	out["libraries"] = views
	out["kind"] = s.setting(ctx, "server_kind")
	out["url"] = url
	writeJSON(w, http.StatusOK, out)
}

// libraryLinked 判断媒体库路径有没有对上本项目的媒体根目录。
//
// 两边的路径常常不一样（容器里是 /media，Emby 里可能是 /mnt/media），
// 所以先看设置里的路径映射；对不上就如实标「未对齐」，不假装已扫描。
func libraryLinked(ctx context.Context, s *Server, paths []string) bool {
	from := strings.TrimSpace(s.setting(ctx, "server_path_from"))
	to := strings.TrimSpace(s.setting(ctx, "server_path_to"))
	for _, p := range paths {
		p = strings.TrimSuffix(strings.TrimSpace(p), "/")
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "/media") || (from != "" && strings.HasPrefix(p, from)) ||
			(to != "" && strings.HasPrefix(p, to)) {
			return true
		}
	}
	return false
}

// handleServerLibraryScan 触发一次媒体库扫描。空 libID = 全库刷新。
func (s *Server) handleServerLibraryScan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		LibraryID string `json:"library_id"`
	}
	_ = readJSON(r, &body)
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	srv, err := s.mediaServer(ctx)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if err := srv.Refresh(ctx, strings.TrimSpace(body.LibraryID)); err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "library_id": body.LibraryID})
}
