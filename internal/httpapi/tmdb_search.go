package httpapi

// 「先出候选、再搜资源」的后端：用户主动搜一个词，先问 TMDB 要一批影视
// 候选（中文名 / 原名 / 年份 / 海报），让用户点选锁定准确的那部，再拿锁定的
// 中文名 + 英文原名去搜 TG 频道和种子站。
//
// 和 /api/res/search 的分工：那条一轮把所有源搜回来，这里只负责「认片」。
// 认片是用户主动发起的低频动作，关键词千变万化，所以不做进程内缓存 ——
// 命中率极低，反而可能让用户改完反代配置后还拿到旧结果。

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"strm115/internal/tmdb"
)

const (
	tmdbSearchTimeout = 12 * time.Second
	tmdbSearchLimit   = 12
	tmdbOverviewRunes = 160
)

// tmdbSearchItem 是候选卡片要的字段，名字和前端 TitleHit 一一对齐。
// 不直接吐 tmdb.MetaHit：那边带 Backdrop 等我们用不上的字段，而且 json tag
// 用了 omitempty，前端拿到的字段会忽有忽无，不利于渲染。
type tmdbSearchItem struct {
	ID         int     `json:"id"`
	Kind       string  `json:"kind"`
	Title      string  `json:"title"`
	Original   string  `json:"original"`
	Year       string  `json:"year"`
	Poster     string  `json:"poster"`
	Vote       float64 `json:"vote"`
	Overview   string  `json:"overview"`
	Popularity float64 `json:"popularity"`
}

type tmdbSearchResp struct {
	Items []tmdbSearchItem `json:"items"`
	Query string           `json:"query"`
	// Total 是本次返回的候选条数（≤ tmdbSearchLimit），和 items 长度一致，
	// 前端拿它显示「共 N 部可选」，语义不含糊。
	Total int `json:"total"`
}

// fetchTMDBMeta 单独留成一个变量，是为了让单测能替换成假数据、不连外网
// 也能验证 handleTMDBSearch 的完整响应（见 tmdb_search_test.go）。
var fetchTMDBMeta = func(s *Server, ctx context.Context, q, year, kind string) ([]tmdb.MetaHit, error) {
	return tmdb.NewClient(s.tmdbKey(ctx)).SearchMeta(ctx, q, year, kind)
}

// handleTMDBSearch GET /api/tmdb/search?q=&type=&year=
//
// type 传 movie / tv / series 只搜对应一类，传空或不认识的值两类都搜。
// 路由由接线方注册，这里只管 HTTP 参数校验、超时与响应组装。
func (s *Server) handleTMDBSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少搜索词 q"))
		return
	}
	kind := normalizeSearchKind(r.URL.Query().Get("type"))
	year := strings.TrimSpace(r.URL.Query().Get("year"))

	ctx, cancel := context.WithTimeout(r.Context(), tmdbSearchTimeout)
	defer cancel()

	hits, err := fetchTMDBMeta(s, ctx, q, year, kind)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, buildTMDBSearchResp(hits, q, tmdbSearchLimit))
}

// normalizeSearchKind 把 URL 上的 type 收敛成 SearchMeta 认识的 kind。
// 认不出来的值（含空）一律返回 ""＝两类都搜：用户手改 URL 不该让整页
// 空白，多打一次 TMDB 比报错友好。
func normalizeSearchKind(typ string) string {
	switch strings.ToLower(strings.TrimSpace(typ)) {
	case "movie":
		return tmdb.KindMovie
	case "tv", "series":
		return tmdb.KindTV
	default:
		return ""
	}
}

// buildTMDBSearchResp 是纯函数：排序截断 + 字段映射 + 组装响应。
// 抽出来便于单测脱离 HTTP 直接验证 items 永不为 null、total 对得上。
func buildTMDBSearchResp(hits []tmdb.MetaHit, query string, limit int) tmdbSearchResp {
	chosen := sortAndTrimHits(hits, limit)
	return tmdbSearchResp{
		Items: projectHits(chosen),
		Query: query,
		Total: len(chosen),
	}
}

// sortAndTrimHits 按热度降序选出最多 limit 条。
//
// 策略（简单，但避免「热门电影把剧集全挤掉」）：
//  1. 先按 Kind 分桶，桶内各自按 Popularity 降序；
//  2. 只有一个桶：直接取前 limit；
//  3. 两个桶：先各取一半名额（limit/2），保证两类都有代表；某类不够，
//     空位由两桶剩余候选合起来按热度补齐；
//  4. 合起来的结果整体再按 Popularity 降序排一次，最后截断到 limit。
//
// 这样输出仍是「按热度降序」，又不会在两类都有结果时一条剧集都看不到。
func sortAndTrimHits(hits []tmdb.MetaHit, limit int) []tmdb.MetaHit {
	if limit <= 0 {
		limit = tmdbSearchLimit
	}
	byPopularity := func(s []tmdb.MetaHit) {
		sort.SliceStable(s, func(i, j int) bool { return s[i].Popularity > s[j].Popularity })
	}

	buckets := map[string][]tmdb.MetaHit{}
	var order []string
	for _, h := range hits {
		if _, seen := buckets[h.Kind]; !seen {
			order = append(order, h.Kind)
		}
		buckets[h.Kind] = append(buckets[h.Kind], h)
	}
	for k := range buckets {
		byPopularity(buckets[k])
	}

	var chosen []tmdb.MetaHit
	switch len(order) {
	case 0:
		// 没有结果，保持空切片（projectHits 会保证 items 是 []）。
	case 1:
		chosen = append(chosen, buckets[order[0]]...)
	default:
		half := limit / 2
		used := map[string]int{}
		for _, k := range order {
			n := half
			if len(buckets[k]) < n {
				n = len(buckets[k])
			}
			chosen = append(chosen, buckets[k][:n]...)
			used[k] = n
		}
		if len(chosen) < limit {
			var rest []tmdb.MetaHit
			for _, k := range order {
				rest = append(rest, buckets[k][used[k]:]...)
			}
			byPopularity(rest)
			need := limit - len(chosen)
			if len(rest) < need {
				need = len(rest)
			}
			chosen = append(chosen, rest[:need]...)
		}
	}

	byPopularity(chosen)
	if len(chosen) > limit {
		chosen = chosen[:limit]
	}
	return chosen
}

// projectHits 映射字段并截断简介。返回值一定非 nil，保证 JSON 里
// items 是 [] 而不是 null（Go 里 nil slice 会序列化成 null）。
func projectHits(hits []tmdb.MetaHit) []tmdbSearchItem {
	items := make([]tmdbSearchItem, 0, len(hits))
	for _, h := range hits {
		items = append(items, tmdbSearchItem{
			ID:         h.ID,
			Kind:       h.Kind,
			Title:      h.Title,
			Original:   h.Original,
			Year:       h.Year,
			Poster:     h.Poster,
			Vote:       h.Vote,
			Overview:   truncateRunes(h.Overview, tmdbOverviewRunes),
			Popularity: h.Popularity,
		})
	}
	return items
}

// 简介截断直接复用 notification.go 里同包的 truncateRunes（按 rune 截断，
// 不把中文切半个字），这里不再重复定义一份。
