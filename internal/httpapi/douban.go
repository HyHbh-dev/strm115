package httpapi

// 豆瓣数据源的三个入口：搜索、榜单、详情。
//
// 「发现与订阅」的搜索框要的是「先认出是哪部片」，国产片在 TMDB 上常常
// 查不到或查错，所以候选清单里 TMDB 和豆瓣各出一份，用户点哪个都行。
// 播出日历里的国产剧 / 国内档期直接走豆瓣榜单 —— 那边才有「更新至第 N 集」
// 和「11 月 10 日上映」这种 TMDB 不给的信息。
//
// 这里只做「转发 + 归一化」，所有反爬细节（UA / Referer / 节流 / 缓存）
// 都在 internal/douban 里。豆瓣挂了就返回错误，前端退回只显示 TMDB，
// 不能让整页搜不出东西。

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/douban"
)

// doubanClient 每次新建：上游代理是进程级设置，用户在设置页改完立刻生效。
//
// douban_base 留空就走官方两个域名；填了（自建反代）两个根都指过去 ——
// 反代按路径分流，用户只需要配一个地址。
func (s *Server) doubanClient(ctx context.Context) *douban.Client {
	base := strings.TrimSpace(s.setting(ctx, "douban_base"))
	return douban.NewWithBases(s.netProxy(ctx), base, base)
}

// doubanOn 是总开关。默认开（留空即开），设置页可以关掉。
//
// 关掉的场景很具体：部署在境外、豆瓣全站被墙，每次搜索都白等十几秒。
func (s *Server) doubanOn(ctx context.Context) bool {
	return strings.TrimSpace(s.setting(ctx, "douban_enabled")) != "0"
}

// handleDoubanSearch 按片名搜豆瓣，给「先认片再搜资源」出候选。
func (s *Server) handleDoubanSearch(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 q"))
		return
	}
	if !s.doubanOn(ctx) {
		writeJSON(w, http.StatusOK, map[string]any{"items": []douban.Item{}, "disabled": true})
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	items, err := s.doubanClient(ctx).Search(ctx, q, limit)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":       items,
		"attribution": douban.Attribution,
	})
}

// handleDoubanCollection 取一个榜单（正在上映 / 即将上映 / 国产剧…）。
func (s *Server) handleDoubanCollection(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 key"))
		return
	}
	if !s.doubanOn(ctx) {
		writeJSON(w, http.StatusOK, map[string]any{
			"key": key, "total": 0, "items": []douban.Item{}, "disabled": true,
		})
		return
	}
	start, _ := strconv.Atoi(r.URL.Query().Get("start"))
	count, _ := strconv.Atoi(r.URL.Query().Get("count"))
	coll, err := s.doubanClient(ctx).Collection(ctx, key, start, count)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"key": coll.Key, "total": coll.Total, "items": coll.Items,
		"attribution":     douban.Attribution,
		"attribution_url": douban.AttributionURL,
	})
}

// handleDoubanDetail 取一条豆瓣详情。kind 可以猜错，客户端会自己换端点。
func (s *Server) handleDoubanDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	q := r.URL.Query()
	id, _ := strconv.Atoi(strings.TrimSpace(q.Get("id")))
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 id"))
		return
	}
	if !s.doubanOn(ctx) {
		writeJSON(w, http.StatusOK, map[string]any{"disabled": true})
		return
	}
	kind := strings.ToLower(strings.TrimSpace(q.Get("kind")))
	if kind != "tv" {
		kind = "movie"
	}
	it, err := s.doubanClient(ctx).Detail(ctx, kind, id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, it)
}

// handleDoubanTest 给设置页的「测试豆瓣」按钮用。
//
// 打一次搜索而不是首页：首页是静态 HTML，国内直连永远 200，测不出
// 「接口被限流」这种真正会让人以为功能坏了的情况。
func (s *Server) handleDoubanTest(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	c := s.doubanClient(ctx)
	started := time.Now()
	items, err := c.Search(ctx, "沙丘", 3)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "豆瓣连通：" + strconv.Itoa(len(items)) + " 条候选，耗时 " + strconv.FormatInt(time.Since(started).Milliseconds(), 10) + " ms",
		"count":   len(items),
	})
}
