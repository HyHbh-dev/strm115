package scrape

// 演员表的中文化。
//
// 以前 nfo 里的 <actor> 只有 <name>，名字来自 IMDb / Cinemeta，永远是拉丁字母。
// 于是 Emby 里三件事同时不对：
//
//  1. 演员头像是灰的 —— nfo 没有 <thumb>，Emby 也没有把这些人关联到 TMDB，
//     它连去哪儿找图都不知道；
//  2. 华人演员显示成 "Tony Ka Fai Leung"，而不是「梁家辉」；
//  3. 角色名干脆没有。
//
// 修法是把演员表换成 TMDB 的 credits（language=zh-CN）：中文名、角色名、
// 头像路径一次都拿到。<thumb> 一写进 nfo，Emby 扫库时就会把图下载下来。
//
// 为什么不是「维护一张英文名→中文名对照表」：这类表永远不够用，
// 而且新片一出来就过时。TMDB 是人维护的，比我们维护得勤。

import (
	"context"
	"strings"

	"strm115/internal/tmdb"
)

// enrichCast 用 TMDB credits 覆盖 m.Cast / m.CastDetail。
//
// 时机放在 localize 之后、写缓存之前：这样缓存里存的就是带中文名和头像的
// 版本，下一轮刮削零请求。
//
// 失败一律静默降级：TMDB 挂了不该让整部片刮不成，损失的只是头像和中文名。
func (s *Service) enrichCast(ctx context.Context, m Meta, opt Options) Meta {
	// 已经补过就不重复问 TMDB：缓存里的条目走到这里就是零请求，
	// 否则每次刮削都会为同一部片再打两次接口。
	if len(m.CastDetail) > 0 {
		return m
	}
	c := s.tmdbClient(ctx)
	if c == nil {
		return m
	}

	// 类型必须由 Meta.Kind 直接决定，不能借 pickCreditsID 反推：
	// 那个函数在两个 id 都是 0 时会把电影也判成 tv，于是电影去请求
	// /movie/<id>/aggregate_credits 的孪生路径 /tv/<id>/aggregate_credits，
	// 404 之后演员表静默为空。
	kind := tmdb.KindMovie
	if m.Kind == KindSeries || strings.EqualFold(m.Kind, "tv") {
		kind = tmdb.KindTV
	}
	var id int
	if m.TMDbID > 0 {
		// 纯 TMDB 路径：详情已经把 ID 带回来了，直接按类型取 credits，
		// 不用再走一次 /find。IMDb ID 可能为空，这条也照样能拿到演员。
		id = m.TMDbID
	} else {
		if m.IMDbID == "" {
			return m
		}
		movieID, tvID, err := c.Find(ctx, m.IMDbID)
		if err != nil {
			s.log.Debug("TMDB 反查失败，演员表保持原样", "imdb", m.IMDbID, "err", err)
			return m
		}
		// 老缓存路径：/find 返回的 ID 属于哪种端点就用哪种端点取 credits，
		// 不能按 m.Kind 硬套（同一个 ttID 在 TMDB 偶尔只挂在电影条目下）。
		kind, id = pickCreditsID(m.Kind, movieID, tvID)
	}
	if id <= 0 {
		return m
	}
	wantChinese := opt.Language == "" || strings.EqualFold(opt.Language, "zh")
	credits, err := c.Credits(ctx, kind, id, 15)
	if err != nil || credits == nil || len(credits.Cast) == 0 {
		if err != nil {
			s.log.Debug("TMDB 取演员表失败", "imdb", m.IMDbID, "err", err)
		}
		return m
	}
	cast := credits.Cast
	m.CastDetail = cast
	// 导演也顺手换成中文名：同一部华语片里出现「韩延 + 朱一龙」才不别扭。
	if len(credits.Directors) > 0 {
		m.Director = credits.Directors
	}

	// m.Cast 是「逗号串」的老字段（搜索、列表、导出都用它），
	// 同步换成中文名，否则界面上演员一会儿中文一会儿英文。
	if wantChinese {
		names := make([]string, 0, len(cast))
		for _, p := range cast {
			if n := strings.TrimSpace(p.Name); n != "" {
				names = append(names, n)
			}
		}
		if len(names) > 0 {
			m.Cast = names
		}
	}
	return m
}

// tmdbClient 按当前 Key + 上游地址复用客户端。
//
// 每次刮削都 NewClient 会让「HTTP 连接复用」和 TMDB 的进程内缓存全部失效，
// 一个 500 部的库就是 500 次 TLS 握手。Key 改了才重建 —— **上游地址改了
// 也要重建**：用户在设置页切到反代时，缓存里那个还指着 api.themoviedb.org，
// 不重建的话改完设置要重启容器才生效。
//
// Key 为空时不再返回 nil：刮削现在只有 TMDB 一家数据源，返回 nil 会让
// 没填自己 Key 的用户整库刮不动。和电影墙共用 tmdb.DefaultKey 兜底
// （来自 STRM115_TMDB_KEY 环境变量，源码不内置）。
func (s *Service) tmdbClient(ctx context.Context) *tmdb.Client {
	key := strings.TrimSpace(s.tmdbKey(ctx))
	if key == "" {
		key = tmdb.DefaultKey()
	}
	api, _ := tmdb.CurrentUpstream()
	forKey := key + "|" + api
	s.tmdbMu.Lock()
	defer s.tmdbMu.Unlock()
	if s.tmdbC == nil || s.tmdbFor != forKey {
		s.tmdbC = tmdb.NewClient(key)
		s.tmdbFor = forKey
	}
	return s.tmdbC
}

// pickCreditsID 按「我们认出来的类型」挑 TMDB id，而不是 TMDB 先给哪个用哪个。
//
// 真实例子：tt12590266（赛博朋克：边缘行者 第一季）在 TMDB 的 find 结果里
// 只有一条 movie_results（"Cyberpunk: Edgerunners II"），没有 tv_results。
// 如果照 TMDB 给的用，剧集就会去请求 /tv/<电影id>/aggregate_credits，
// 404，演员表静默为空 —— 界面上看起来就是「这部剧没有演员」。
// 挑不到本类型的 id 时才退回另一个：有头像总比没有好。
func pickCreditsID(kind string, movieID, tvID int) (string, int) {
	wantTV := kind == KindSeries || kind == "tv"
	if wantTV {
		if tvID > 0 {
			return tmdb.KindTV, tvID
		}
		return tmdb.KindMovie, movieID
	}
	if movieID > 0 {
		return tmdb.KindMovie, movieID
	}
	return tmdb.KindTV, tvID
}
