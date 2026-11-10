package httpapi

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"
)

// 前端只有一份产物：webdist/（Vue 版管理界面，由 web/ 构建）。
//
// 上一版的单文件 index.html 已经删掉（它和 Vue 版共用同一批接口，
// 留着只会让每个改动都要维护两套界面）。开发期要热更新走 vite 的
// dev server（scripts/web-build.sh --dev），不需要「绕过内嵌副本」这条路。
//
//go:embed all:webdist
var webFS embed.FS

// handleSPA 处理 Vue 版的静态资源与前端路由。
//
// 关键点是 history 模式：/sync、/settings 这些路径刷新时浏览器会直接请求
// 服务端，找不到文件就要回 index.html，否则一刷新就 404。
func (s *Server) handleSPA(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	sub, err := fs.Sub(webFS, "webdist")
	if err != nil {
		s.serveNoFrontend(w)
		return
	}

	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if rel == "" || rel == "." {
		rel = "index.html"
	}

	if f, err := sub.Open(rel); err == nil {
		info, statErr := f.Stat()
		_ = f.Close()
		if statErr == nil && !info.IsDir() {
			setAssetCache(w, rel)
			http.FileServer(http.FS(sub)).ServeHTTP(w, r)
			return
		}
	}

	// 前端路由：一律回 index.html，由 vue-router 自己解析
	body, readErr := fs.ReadFile(sub, "index.html")
	if readErr != nil {
		s.serveNoFrontend(w)
		return
	}
	setAssetCache(w, "index.html")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}

// serveNoFrontend 是「前端没构建」时的说明页。
//
// 以前这里会退回上一版的单文件界面，看起来像正常打开、实际是两套 UI 里
// 老的那一套，排查时极易看错页面。现在明说是构建产物缺失，把命令也写上去。
func (s *Server) serveNoFrontend(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = w.Write([]byte(`<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>前端未构建</title>
<style>body{font:14px/1.7 system-ui,-apple-system,"Microsoft YaHei",sans-serif;
margin:12vh auto;max-width:620px;padding:0 24px;color:#222}
code{background:#f4f4f5;padding:2px 6px;border-radius:4px}
pre{background:#f4f4f5;padding:12px 14px;border-radius:8px;overflow:auto}</style>
</head><body>
<h2>前端构建产物缺失</h2>
<p>后端起来了，但二进制里没有 <code>webdist/</code>。这说明编译前没跑前端构建：</p>
<pre>./scripts/web-build.sh
go build ./cmd/server</pre>
<p>用 Docker 部署不需要手动做这一步，镜像里会自动构建。</p>
</body></html>`))
}

// setAssetCache 给带哈希的构建产物长缓存，入口文件不缓存。
// 不做这件事的话，升级后浏览器会拿旧 index.html 去配新 assets，白屏。
func setAssetCache(w http.ResponseWriter, rel string) {
	if strings.HasPrefix(rel, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")
}

// janitor 定期清理过期的扫码会话，避免内存里留一堆死对象。
func (s *Server) janitor() {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for range t.C {
		cut := time.Now().Add(-5 * time.Minute)
		s.qrMu.Lock()
		for k, v := range s.qr {
			if v.born.Before(cut) {
				delete(s.qr, k)
			}
		}
		s.qrMu.Unlock()

		s.qkMu.Lock()
		for k, v := range s.qk {
			if v.born.Before(cut) {
				delete(s.qk, k)
			}
		}
		s.qkMu.Unlock()
		cleanQuarkWebSessions(cut)
	}
}
