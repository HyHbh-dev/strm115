package httpapi

// 海报墙的 TMDB 数据源。
//
// 和刮削的分工：刮削负责「把 TMDB 元数据写到 nfo / 台账 / 本地图片」，
// 这里负责「不依赖 Emby 也能看到一面像样的墙」。卡片骨架来自本地台账
// （只有它知道网盘里到底有什么），标题 / 评分 / 简介 / 类型 / 海报则按
// 台账里的 tmdb_id 现查 TMDB。
//
// 为什么还需要 Emby / 本地台账两个可切换来源：
//   - 没配 TMDB Key 且反代也不通时，TMDB 查不到，卡片会退回台账里的字段；
//   - 想看「媒体服务器里最终生效的那份 nfo」时切 Emby；
//   - 想核对「网盘里有、但还没进媒体服务器」时切本地台账。
//
// 查不到绝不删卡片：少几张海报可以接受，整面墙变空不行。

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/scrape"
	"strm115/internal/tmdb"
)

const (
	// 同一部作品的详情 30 分钟内不再重查。翻页、改排序、点详情都会重复命中。
	catalogTMDBTTL = 30 * time.Minute
	// 一次最多 4 个并发详情请求。TMDB 对突发请求很敏感，一页 30 张卡
	// 全并发过去容易被限流，反而比 4 并发更慢。
	catalogTMDBConcurrency = 4
)

type catalogTMDBEntry struct {
	at  time.Time
	d   *tmdb.MetaDetail
	err error
}

// catalogTMDBClient 每次都新建是故意的：TMDB 上游是进程级设置，
// 用户在设置页改完反代地址后下一次列表请求就该生效。详情缓存挂在
// Server 上，不会因为换 Client 而失效。
func (s *Server) catalogTMDBClient(ctx context.Context) *tmdb.Client {
	return tmdb.NewClient(s.tmdbKey(ctx))
}

func (s *Server) catalogTMDBGet(key string) (catalogTMDBEntry, bool) {
	s.catalogTMDBMu.Lock()
	defer s.catalogTMDBMu.Unlock()
	e, ok := s.catalogTMDB[key]
	if !ok || time.Since(e.at) > catalogTMDBTTL {
		return catalogTMDBEntry{}, false
	}
	return e, true
}

func (s *Server) catalogTMDBPut(key string, d *tmdb.MetaDetail, err error) {
	s.catalogTMDBMu.Lock()
	defer s.catalogTMDBMu.Unlock()
	if s.catalogTMDB == nil {
		s.catalogTMDB = map[string]catalogTMDBEntry{}
	}
	// 顺手清掉过期项，避免长跑的容器里这张表无限增长。
	now := time.Now()
	for k, e := range s.catalogTMDB {
		if now.Sub(e.at) > catalogTMDBTTL {
			delete(s.catalogTMDB, k)
		}
	}
	s.catalogTMDB[key] = catalogTMDBEntry{at: now, d: d, err: err}
}

// catalogTMDBKey 把「用什么标识查的」也编进缓存键，避免同一部片
// 用不同入口查到不同条目时互相污染。
func catalogTMDBKey(kind string, tmdbID int, imdbID, name, year string) string {
	switch {
	case tmdbID > 0:
		return fmt.Sprintf("id|%s|%d", kind, tmdbID)
	case strings.TrimSpace(imdbID) != "":
		return fmt.Sprintf("imdb|%s|%s", kind, strings.TrimSpace(imdbID))
	default:
		return fmt.Sprintf("name|%s|%s|%s", kind, strings.ToLower(strings.TrimSpace(name)), year)
	}
}

func catalogTMDBID(id int) string {
	if id <= 0 {
		return ""
	}
	return strconv.Itoa(id)
}

func catalogTMDBKindOf(kind string) string {
	if strings.EqualFold(kind, scrape.KindSeries) || strings.EqualFold(kind, tmdb.KindTV) {
		return tmdb.KindTV
	}
	return tmdb.KindMovie
}

func catalogTMDBParsedKind(kind string) string {
	if kind == tmdb.KindTV {
		return scrape.KindSeries
	}
	return scrape.KindMovie
}

// catalogTMDBMeta 按「台账 TMDB ID → IMDb ID → 片名+年份」依次定位 TMDB 条目。
//
// 老库里刮削过但没落 tmdb_id 的行走后两级；IMDb ID 只是给 TMDB 的
// /find 当反查键，不会再请求 IMDb 或 Cinemeta 任何服务。
func (s *Server) catalogTMDBMeta(ctx context.Context, c *tmdb.Client,
	kind string, tmdbID int, imdbID, name, year string) (*tmdb.MetaDetail, error) {

	key := catalogTMDBKey(kind, tmdbID, imdbID, name, year)
	if hit, ok := s.catalogTMDBGet(key); ok {
		return hit.d, hit.err
	}
	d, err := resolveCatalogTMDBMeta(ctx, c, kind, tmdbID, imdbID, name, year)
	s.catalogTMDBPut(key, d, err)
	return d, err
}

func resolveCatalogTMDBMeta(ctx context.Context, c *tmdb.Client,
	kind string, tmdbID int, imdbID, name, year string) (*tmdb.MetaDetail, error) {

	if tmdbID > 0 {
		return c.MetaByID(ctx, kind, tmdbID)
	}
	if id := strings.TrimSpace(imdbID); id != "" {
		return c.MetaByIMDbID(ctx, id, kind)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("没有可用于查询 TMDB 的标识")
	}
	hits, err := c.SearchMeta(ctx, name, year, kind)
	if err != nil {
		return nil, err
	}
	hit, ok := scrape.Pick(scrape.Parsed{
		Title: name, Year: year, Kind: catalogTMDBParsedKind(kind),
	}, hits)
	if !ok {
		return nil, errors.New("TMDB 里没找到足够接近的结果")
	}
	return c.MetaByID(ctx, hit.Kind, hit.ID)
}

// applyCatalogTMDB 用 TMDB 详情覆盖展示字段。
//
// 只覆盖「TMDB 确实给了」的字段：查不到时卡片仍然是完整的本地台账卡片，
// 不会变成一张空白卡。Key / MediaID / NFO / 字幕 / 路径这些本地状态一个不动。
func applyCatalogTMDB(c *catalogCard, d *tmdb.MetaDetail) {
	if c == nil || d == nil {
		return
	}
	if v := strings.TrimSpace(d.Title); v != "" {
		c.Name = v
	}
	if v := strings.TrimSpace(d.Original); v != "" {
		c.Original = v
	}
	if v := yearOfLocal(d.Year); v > 0 {
		c.Year = v
	}
	if d.Rating != "" {
		c.Rating = parseFloatLocal(d.Rating)
	}
	if v := strings.TrimSpace(d.Overview); v != "" {
		c.Overview = v
	}
	if len(d.Genres) > 0 {
		c.Genres = d.Genres
	}
	if d.Poster != "" && c.Poster == "" {
		// 只补空位：磁盘上那张图比远端 URL 快，别把它换掉
		c.Poster = d.Poster
	}
	if d.Backdrop != "" && c.Backdrop == "" {
		c.Backdrop = d.Backdrop
	}
	if v := strings.TrimSpace(d.IMDbID); v != "" {
		c.IMDbID = v
	}
	if d.ID > 0 {
		c.TMDBID = strconv.Itoa(d.ID)
	}
}

// enrichCatalogTMDB 只对当前这一页里**缺图**的卡片查 TMDB，返回未命中的张数。
//
// 整库可能有几千条，先按页裁再 enrich 才不会一次打几千个请求。
// 单个失败只记数、不中断，最后在 hint 里明说「有几张用的本地信息」。
//
// skipFilled=true 时跳过已经有海报的卡片 —— 这是默认行为。刮削阶段已经把
// 标题 / 年份 / 评分 / 简介 / 类型 / 海报 URL 全写进 media_meta 了，列表页再问
// 一遍纯属白等：TMDB 反代单次 1.6–2.6 秒，六张卡（4 并发）就是两三秒的首屏。
// force（refresh=1）才让已经有图的卡也重查。
func (s *Server) enrichCatalogTMDB(ctx context.Context, cards []catalogCard, force bool) int {
	if len(cards) == 0 {
		return 0
	}
	c := s.catalogTMDBClient(ctx)
	var mu sync.Mutex
	missing := 0
	sem := make(chan struct{}, catalogTMDBConcurrency)
	var wg sync.WaitGroup
	for i := range cards {
		if !force && strings.TrimSpace(cards[i].Poster) != "" {
			continue // 有图就不问，见上面的说明
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			card := &cards[i]
			id, _ := strconv.Atoi(strings.TrimSpace(card.TMDBID))
			d, err := s.catalogTMDBMeta(ctx, c,
				catalogTMDBKindOf(card.Type), id, card.IMDbID,
				card.Name, strconv.Itoa(card.Year))
			if err != nil || d == nil {
				mu.Lock()
				missing++
				mu.Unlock()
				return
			}
			mu.Lock()
			applyCatalogTMDB(card, d)
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	return missing
}
