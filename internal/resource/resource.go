// Package resource 在「Telegram 公开频道」和「几个种子站」里搜一个片名。
//
// 设计目标是「够用就好，坏了不拖累整体」：
//   - 每个源独立超时、独立报错，一个站挂了不影响别的（TG 被墙时种子站照常出结果）；
//   - 只认公开可访问的入口：TG 走 t.me/s/ 的网页预览（不需要登录，也不需要 bot token），
//     种子站走各自的 RSS / JSON 接口，不去解析需要过 Cloudflare 的搜索页；
//   - 结果统一成一种结构，前端不管来源都能「复制磁力 / 提交离线」。
//
// 明确不做的事：不做全站抓取、不存库、不代理下载。这里只是把搜索结果摆出来，
// 真正下载仍然走 115 离线（internal/offline）。
package resource

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/netproxy"
)

// Item 是一条搜索结果。TG 消息和种子条目共用这套字段：
// 有 Magnet 的能直接丢给 115，没有的至少能点开原始页面。
type Item struct {
	Title     string   `json:"title"`
	Source    string   `json:"source"`     // 显示名：@QukanMovie / Nyaa
	SourceKey string   `json:"source_key"` // 稳定标识：tg:QukanMovie / nyaa
	Kind      string   `json:"kind"`       // tg / torrent
	Magnet    string   `json:"magnet,omitempty"`
	URL       string   `json:"url,omitempty"`
	Torrent   string   `json:"torrent,omitempty"` // 只有 .torrent 直链没有 info hash 的站（Mikan）
	Size      int64    `json:"size,omitempty"`
	Seeders   int      `json:"seeders,omitempty"`
	Leechers  int      `json:"leechers,omitempty"`
	Date      string   `json:"date,omitempty"` // 2024-11-08
	Snippet   string   `json:"snippet,omitempty"`
	Links     []string `json:"links,omitempty"` // TG 里的网盘链接（115 / 阿里 / 百度 …）
	// 115 分享：TG 帖子里最值钱的那条。认出分享码 + 提取码之后，前端可以直接
	// 「存到 115」，不用再点开 t.me 的帖子去复制粘贴。
	ShareCode string `json:"share_code,omitempty"`
	SharePwd  string `json:"share_pwd,omitempty"`
	ShareURL  string `json:"share_url,omitempty"`
	ShareHost string `json:"share_host,omitempty"`
	// 夸克分享：TG 里只放夸克方向的频道（yunpanquark / quarkshare）几乎每帖都是它。
	// 它和 115 分享走两条不同链路：115 走离线/转存，夸克走
	// 「网页 Cookie 转存 + TV 接口 302 播放」，所以单独标记，不能混进 ShareCode。
	QuarkShare string `json:"quark_share,omitempty"` // pan.quark.cn/s/ 后面那一段
	QuarkPwd   string `json:"quark_pwd,omitempty"`
	QuarkURL   string `json:"quark_url,omitempty"`
	Category   string `json:"category,omitempty"`
}

// Status 是单个源的执行情况。前端用它显示「哪几个源没搜到 / 挂了」。
type Status struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	Kind  string `json:"kind"`
	OK    bool   `json:"ok"`
	Count int    `json:"count"`
	MS    int64  `json:"ms"`
	Err   string `json:"error,omitempty"`
	// Cached 表示这一轮直接复用了缓存，没有真的去连这个源。
	// 界面上要把「0ms」解释清楚，否则会被误以为某个站快得离谱。
	Cached bool `json:"cached,omitempty"`
}

// Result 是一整轮搜索的结果。
type Result struct {
	Items   []Item   `json:"items"`
	Sources []Status `json:"sources"`
	Query   string   `json:"query"`
	// SiteQuery 是种子站实际用的查询串。中文片名在种子站（尤其海盗湾）经常一条都对不上，
	// 调用方会用 IMDb 的英文原名替它，前端据此提示「种子站按英文名搜」。
	SiteQuery string `json:"site_query,omitempty"`
	// EnLookup 是「中文名换英文名」这一步的结果：ok=换到了，fail=没换到。
	// fail 时种子站只能按中文搜，前端据此提示用户改输英文名，而不是干放着一页空结果。
	EnLookup string `json:"en_lookup,omitempty"`
	// ChannelQuery 是 TG 频道额外用的英文词（TMDB 换来的）。有值时每个频道
	// 中文、英文各搜一次再合并，前端据此提示「频道也按英文名搜了」。
	ChannelQuery string `json:"channel_query,omitempty"`
	TookMS       int64  `json:"took_ms"`
	// Cached 表示这一轮所有源都命中了缓存：切换「全部 / 种子站 / TG 频道」
	// 不该重新打一遍网络，这个标记让用户知道为什么秒回。
	Cached bool `json:"cached,omitempty"`
	// Error 只在「一个源都没成功」时非空；有结果时即使部分源失败也不填。
	Error string `json:"error,omitempty"`
	// Filtered 是被当成广告/垃圾跳过的条数。前端据此提示「已过滤 N 条广告」，
	// 免得用户以为站点漏结果。
	Filtered int `json:"filtered,omitempty"`
}

// Options 是一次搜索的参数。为零值时用默认值。
type Options struct {
	Query string
	// SiteQuery 只作用于种子站；留空就用 Query。
	// 分开的理由：种子站是英文索引，中文基本搜不到。
	SiteQuery string
	// ChannelQuery 是 TG 频道额外搜一遍的词（一般是 TMDB 换来的英文原名）。
	// 留空 = 频道只按 Query 搜。中文频道里也有英文标题的帖子，
	// 光按中文搜会漏掉一大半，所以两种词都查再合并去重。
	ChannelQuery string
	Kind         string   // tg / torrent / 空 = 都要
	Channels     []string // TG 频道名（不带 @）
	Sites        []string // tpb / nyaa / dmhy / mikan / uindex / therarbg / clm
	// Custom 是用户自填的 RSS / Torznab 源，多行文本原样传进来
	// （格式见 ParseCustomSites）。这里收字符串而不是结构体，是为了让调用方
	// 直接从设置表读一行就传，不必自己解析一遍。
	Custom  string
	Proxy   string // 出站代理，空=直连
	Limit   int    // 每个源最多取几条
	Timeout time.Duration
	// Refresh 强制跳过缓存重新抓一遍。给界面上的「重搜」按钮用：
	// 默认复用几分钟内的结果，用户明确要最新时才付这一次网络代价。
	Refresh bool
}

// DefaultChannels 是内置的 TG 资源频道。
//
// 只放两个方向：115 和夸克。这不是随便挑的，是按实际抓下来的页面统计的：
//   - QukanMovie  —— 115 影视资源，一页 19 条全是 115 分享；
//   - yunpanquark —— 夸克网盘资源分享，影视剧为主，命名规范（「名称：xxx 更至 N 集」）；
//   - quarkshare  —— 夸克云盘资源收集，影视 + 合集。
//
// 曾经默认带上的 shareAliyun（阿里，46 阿里 / 12 夸克）和 bdwpzhpd（百度，40 百度）
// 已经去掉：这个项目的转存只有 115 一条路，阿里和百度的链接进了结果也点不动，
// 每轮白跑两个请求还挤占版面。
var DefaultChannels = []string{"QukanMovie", "yunpanquark", "quarkshare"}

// DefaultSites 是内置种子站，顺序即界面顺序。
// statusRank 用这个顺序排源标签，新增站点只要加在这里就不会乱序。
var DefaultSites = []string{"tpb", "nyaa", "dmhy", "mikan", "uindex", "therarbg", "clm"}

// Site 是一个种子站实现。
type Site struct {
	Key    string
	Name   string
	Search func(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error)
}

// Sites 返回全部内置种子站。
func Sites() []Site {
	return []Site{
		{Key: "tpb", Name: "海盗湾", Search: searchTPB},
		{Key: "nyaa", Name: "Nyaa", Search: searchNyaa},
		{Key: "dmhy", Name: "动漫花园", Search: searchDMHY},
		{Key: "mikan", Name: "Mikan", Search: searchMikan},
		{Key: "uindex", Name: "UIndex", Search: searchUIndex},
		{Key: "therarbg", Name: "TheRarBG", Search: searchTheRarBG},
		{Key: "clm", Name: "磁力猫", Search: searchClm},
	}
}

// SiteNames 返回 key → 显示名，给前端展示用。
func SiteNames() map[string]string {
	out := map[string]string{}
	for _, s := range Sites() {
		out[s.Key] = s.Name
	}
	return out
}

// Service 负责并发调度各个源。
//
// 所有请求共用同一个实例：TG 频道页有一个短 TTL 缓存（见 telegram()），
// 每次请求新建一个 Service 就等于没有缓存 —— 而 t.me 对同一 IP 短时间内
// 重复抓同一个频道会直接把连接挂住，直到超时（实测第二次必超时）。
type Service struct {
	log *slog.Logger

	mu  sync.Mutex
	tg  map[string]tgEntry
	src map[string]srcEntry // 源级结果缓存（TG 频道 + 种子站 + 自定义源）
	now func() time.Time    // 测试里可以拨表
	// fetchTG 是真正抓频道的函数。默认就是 searchTelegram，
	// 单测里换成一个返回固定条目的假实现，缓存逻辑才不用真连 t.me。
	fetchTG func(ctx context.Context, hc *http.Client, ch, q string, limit int) ([]Item, error)
}

type tgEntry struct {
	items []Item
	at    time.Time
}

// srcEntry 是一个源一轮搜索的结果。失败也存，但只存很短一会儿：
// 一次网络抖动不该让用户接下来五分钟都看到同一个失败，可一个已经明确
// 挂掉的源也不该在「切换筛选 / 连点两次」时每次都重新超时一轮。
type srcEntry struct {
	items []Item
	err   error
	at    time.Time
}

const (
	// tgCacheTTL 是频道页原始 HTML 的缓存时长。太短没意义（t.me 的限流窗口
	// 就是几十秒），太长又会让用户觉得「刚发的资源搜不到」。
	tgCacheTTL = 60 * time.Second
	// siteCacheTTL 是种子站结果的缓存时长。种子站不像 t.me 会限流，
	// 但一次搜索要打七个站、TheRarBG 还要翻详情页，5 分钟内复用最划算。
	siteCacheTTL = 5 * time.Minute
	// failCacheTTL 是「这个源挂了」的负缓存时长。比正常结果短一个数量级，
	// 站点恢复了半分钟内就能自己回来。
	failCacheTTL = 30 * time.Second
	// srcCacheMaxKeys 是缓存上限。超了整体清掉，比逐条淘汰简单，
	// 也不会因为某个用户配了几十个自定义源把内存吃住。
	srcCacheMaxKeys = 300
)

func New(log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{
		log: log, tg: map[string]tgEntry{}, src: map[string]srcEntry{},
		now: time.Now, fetchTG: searchTelegram,
	}
}

// cachedSource 给「一个源的整轮抓取」包一层缓存。
//
// 缓存键由调用方拼好，必须包含这个源真正用到的所有查询词和 limit ——
// 少了任何一个，换个英文名或换个每源条数就会拿到上一次的结果。
// 成功按 ttl 缓存，失败按 failCacheTTL 做负缓存；Refresh 跳过读但仍写回。
func (s *Service) cachedSource(key string, ttl time.Duration, refresh bool,
	fetch func() ([]Item, error)) ([]Item, bool, error) {

	if !refresh {
		s.mu.Lock()
		e, ok := s.src[key]
		s.mu.Unlock()
		if ok {
			alive := ttl
			if e.err != nil {
				alive = failCacheTTL
			}
			if s.now().Sub(e.at) < alive {
				return e.items, true, e.err
			}
		}
	}
	items, err := fetch()
	if err != nil {
		items = nil // 失败时的半成品不许进结果列表
	} else if items == nil {
		items = []Item{}
	}
	s.mu.Lock()
	if len(s.src) > srcCacheMaxKeys {
		s.src = map[string]srcEntry{}
	}
	s.src[key] = srcEntry{items: items, err: err, at: s.now()}
	s.mu.Unlock()
	return items, false, err
}

// sourceCacheKey 把「源 + 它用的查询词 + 条数 + 代理」拼成一个键。
// 代理也算进去：同一个站走不走代理拿到的结果可能完全不同。
func sourceCacheKey(source, query, proxy string, limit int) string {
	return strings.ToLower(source) + "\x00" + strings.ToLower(strings.TrimSpace(query)) +
		"\x00" + strconv.Itoa(limit) + "\x00" + strings.TrimSpace(proxy)
}

// telegram 包一层缓存再调 searchTelegram。
//
// cached=true 只影响「测试」结果的展示：让人知道这条是刚才那次的结果，
// 不是此刻重新抓的 —— 否则会误以为 t.me 秒回。
func (s *Service) telegram(ctx context.Context, hc *http.Client, ch, q string, limit int, refresh bool) ([]Item, bool, error) {
	key := ch + "\x00" + strings.ToLower(strings.TrimSpace(q)) + "\x00" + strconv.Itoa(limit)

	if !refresh {
		s.mu.Lock()
		if e, ok := s.tg[key]; ok && s.now().Sub(e.at) < tgCacheTTL {
			items := e.items
			s.mu.Unlock()
			return items, true, nil
		}
		s.mu.Unlock()
	}

	items, err := s.fetchTG(ctx, hc, ch, q, limit)
	if err != nil {
		return nil, false, err
	}

	s.mu.Lock()
	// 一个频道一页就几十条，缓存不会长；超过 200 个键说明设了太多频道，整体清掉最省事
	if len(s.tg) > 200 {
		s.tg = map[string]tgEntry{}
	}
	s.tg[key] = tgEntry{items: items, at: s.now()}
	s.mu.Unlock()

	return items, false, nil
}

// Search 并发搜所有源。返回的第一条错误只在「全部源都失败」时给出，
// 单个源失败会写进 Sources（前端显示成灰色小字），不打断整轮。
func (s *Service) Search(ctx context.Context, opt Options) (*Result, error) {
	q := strings.TrimSpace(opt.Query)
	if q == "" {
		return nil, errors.New("搜索词不能为空")
	}
	if len(q) > 100 {
		q = q[:100]
	}
	siteQ := strings.TrimSpace(opt.SiteQuery)
	if len(siteQ) > 100 {
		siteQ = siteQ[:100]
	}
	if siteQ == "" {
		siteQ = q
	}
	channelQ := strings.TrimSpace(opt.ChannelQuery)
	if len(channelQ) > 100 {
		channelQ = channelQ[:100]
	}
	if strings.EqualFold(channelQ, q) {
		channelQ = "" // 和主查询一样就没必要再打一遍 t.me
	}
	if opt.Limit <= 0 {
		opt.Limit = 20
	}
	if opt.Limit > 50 {
		opt.Limit = 50
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 12 * time.Second
	}
	channels := opt.Channels
	if len(channels) == 0 {
		channels = DefaultChannels
	}
	siteKeys := opt.Sites
	if len(siteKeys) == 0 {
		siteKeys = DefaultSites
	}

	hc := httpClient(opt.Proxy, opt.Timeout)
	kind := strings.ToLower(strings.TrimSpace(opt.Kind))

	started := time.Now()
	var (
		mu      sync.Mutex
		items   []Item
		status  []Status
		wg      sync.WaitGroup
		sem     = make(chan struct{}, 6)
		wantTG  = kind == "" || kind == "tg" || kind == "telegram"
		wantTor = kind == "" || kind == "torrent" || kind == "site"
	)

	collect := func(st Status, its []Item) {
		mu.Lock()
		defer mu.Unlock()
		status = append(status, st)
		items = append(items, its...)
	}

	if wantTG {
		for _, ch := range channels {
			ch = strings.TrimPrefix(strings.TrimSpace(ch), "@")
			if ch == "" {
				continue
			}
			wg.Add(1)
			go func(ch string) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				t0 := time.Now()
				// 一个频道两种词在同一个 goroutine 里串行查：t.me 对同一 IP
				// 的并发很敏感，把它们也丢进 sem 很容易自己把自己测成超时。
				cacheQ := q + "\x01" + channelQ
				its, cached, err := s.cachedSource(
					sourceCacheKey("tg:"+ch, cacheQ, opt.Proxy, opt.Limit),
					tgCacheTTL, opt.Refresh,
					func() ([]Item, error) {
						a, _, e := s.telegram(ctx, hc, ch, q, opt.Limit, opt.Refresh)
						if e != nil {
							// 中文没查到不代表英文也查不到，但只要有一个成了
							// 就当这个源是好的，别整条标红。
							if channelQ == "" {
								return nil, e
							}
							b, _, e2 := s.telegram(ctx, hc, ch, channelQ, opt.Limit, opt.Refresh)
							if e2 != nil {
								return nil, e
							}
							return b, nil
						}
						if channelQ == "" {
							return a, nil
						}
						b, _, e2 := s.telegram(ctx, hc, ch, channelQ, opt.Limit, opt.Refresh)
						if e2 == nil {
							a = mergeItems(a, b)
						}
						return a, nil
					})
				st := Status{Key: "tg:" + ch, Name: "@" + ch, Kind: "tg", OK: err == nil,
					Count: len(its), MS: time.Since(t0).Milliseconds(), Cached: cached}
				if err != nil {
					st.Err = err.Error()
					s.log.Warn("TG 频道搜索失败", "channel", ch, "err", err)
				}
				collect(st, its)
			}(ch)
		}
	}

	if wantTor {
		for _, key := range siteKeys {
			site, ok := findSite(key)
			if !ok {
				continue
			}
			wg.Add(1)
			go func(site Site) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				t0 := time.Now()
				its, cached, err := s.cachedSource(
					sourceCacheKey(site.Key, siteQ, opt.Proxy, opt.Limit),
					siteCacheTTL, opt.Refresh,
					func() ([]Item, error) { return site.Search(ctx, hc, siteQ, opt.Limit) })
				st := Status{Key: site.Key, Name: site.Name, Kind: "torrent", OK: err == nil,
					Count: len(its), MS: time.Since(t0).Milliseconds(), Cached: cached}
				if err != nil {
					st.Err = err.Error()
					s.log.Warn("种子站搜索失败", "site", site.Key, "err", err)
				}
				collect(st, its)
			}(site)
		}
		// 用户自填的源和内置站一视同仁：同样的并发、同样的独立报错。
		// 一个私有站要登录才能搜，挂了不该影响别的源出结果。
		for _, cs := range ParseCustomSites(opt.Custom) {
			wg.Add(1)
			go func(cs CustomSite) {
				defer wg.Done()
				sem <- struct{}{}
				defer func() { <-sem }()
				t0 := time.Now()
				its, cached, err := s.cachedSource(
					sourceCacheKey(cs.Key, siteQ, opt.Proxy, opt.Limit),
					siteCacheTTL, opt.Refresh,
					func() ([]Item, error) { return searchCustom(ctx, hc, cs, siteQ, opt.Limit) })
				st := Status{Key: cs.Key, Name: cs.Name, Kind: "torrent", OK: err == nil,
					Count: len(its), MS: time.Since(t0).Milliseconds(), Cached: cached}
				if err != nil {
					st.Err = err.Error()
					s.log.Warn("自定义源搜索失败", "site", cs.Name, "err", err)
				}
				collect(st, its)
			}(cs)
		}
	}

	wg.Wait()

	// 广告/垃圾先剔掉：种子站和公开频道里混着大量「加微信」「最新网址」
	// 这类条目，体积往往只有几十 MB。它们不但占位置，还会被用户当成资源点开。
	items, filtered := dropAds(items)
	// 中文名和英文名都算相关性：TG 频道现在两种词都搜，
	// 只按中文打分会把英文标题的结果全压到列表底部。
	sortItems(items, q, siteQ, channelQ)
	// 源的状态按「注册顺序」排，不然每次刷新顺序都在跳。
	sort.SliceStable(status, func(i, j int) bool {
		ri, rj := statusRank(status[i]), statusRank(status[j])
		if ri != rj {
			return ri < rj
		}
		// 同档（多个自定义源、多个频道）也定死，靠 key 排 ——
		// 并发 collect 的顺序是随机的，界面上的源标签会每次都换位置。
		return status[i].Key < status[j].Key
	})

	allCached := len(status) > 0
	for _, st := range status {
		if !st.Cached {
			allCached = false
			break
		}
	}
	res := &Result{
		Items: items, Sources: status, Query: q, SiteQuery: siteQ,
		ChannelQuery: channelQ, TookMS: time.Since(started).Milliseconds(),
		Cached: allCached, Filtered: filtered,
	}
	if len(items) == 0 {
		if err := allFailed(status); err != nil {
			return res, err
		}
	}
	return res, nil
}

// mergeItems 合并一个频道两次搜索（中文 / 英文）的结果。
//
// 同一条帖子很可能两个词都命中，去重优先用消息地址，其次磁力，最后才
// 退回标题 —— 帖子链接是唯一的，标题可能被截断或加了前缀。
func mergeItems(a, b []Item) []Item {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]Item, 0, len(a)+len(b))
	for _, list := range [][]Item{a, b} {
		for _, it := range list {
			k := itemKey(it)
			if k == "" || seen[k] {
				continue
			}
			seen[k] = true
			out = append(out, it)
		}
	}
	return out
}

func itemKey(it Item) string {
	switch {
	case it.URL != "":
		return "u:" + it.URL
	case it.Magnet != "":
		return "m:" + it.Magnet
	case it.Title != "":
		return "t:" + strings.ToLower(strings.TrimSpace(it.Title))
	}
	return ""
}

// sortItems 决定结果顺序，三层：
//
//  1. 标题命中几个关键词 —— 搜「流浪地球」时，中文 TG 帖排在
//     「Spider-Man」这种明显不相关的结果前面；
//  2. 同等相关度里体积大的在前（原盘 > 压制 > 片段），体积未知的沉底；
//  3. 体积相同再「能直接用的」优先：磁力 > TG 消息 > 只有种子页；
//  4. 最后按做种数（热度）和时间（越新越好）。
func sortItems(items []Item, queries ...string) {
	rank := func(it Item) int {
		switch {
		case it.Magnet != "":
			return 0
		case it.Kind == "tg":
			return 1
		default:
			return 2
		}
	}
	score := make([]int, len(items))
	for i := range items {
		// 多个查询词取最高分：中文命中的中文帖、英文命中的英文帖都能排前面。
		for _, q := range queries {
			if q == "" {
				continue
			}
			if n := relevance(items[i].Title, q); n > score[i] {
				score[i] = n
			}
		}
	}
	sort.SliceStable(items, func(i, j int) bool {
		// 相关度仍然是第一层：搜「流浪地球」时不能因为某条广告种子 80GB
		// 就把它排到正片前面。
		if score[i] != score[j] {
			return score[i] > score[j]
		}
		// 同等相关度里，体积大的在前（原盘 > 压制 > 片段），
		// 体积未知的沉底 —— 没有体积的多半是 TG 文字帖。
		si, sj := items[i].Size, items[j].Size
		if (si > 0) != (sj > 0) {
			return si > 0
		}
		if si != sj {
			return si > sj
		}
		ri, rj := rank(items[i]), rank(items[j])
		if ri != rj {
			return ri < rj
		}
		// 体积一样再按热度（做种数）。
		if items[i].Seeders != items[j].Seeders {
			return items[i].Seeders > items[j].Seeders
		}
		return items[i].Date > items[j].Date
	})
}

// relevance 数一数标题里命中了几个查询词。
//
// 故意做得这么粗：中文没有分词，英文标题里的分隔符又五花八门（点、空格、下划线），
// 精确匹配反而会把「Oppenheimer.2023.1080p」这种正常结果判成不相关。
func relevance(title, query string) int {
	t := strings.ToLower(title)
	hit := 0
	for _, tok := range strings.Fields(strings.ToLower(query)) {
		if tok != "" && strings.Contains(t, tok) {
			hit++
		}
	}
	return hit
}

func statusRank(s Status) int {
	if s.Kind == "torrent" {
		for i, key := range DefaultSites {
			if s.Key == key {
				return i
			}
		}
		return 90
	}
	return 100
}

// allFailed 只在「一个源都没成功」时返回错误，否则返回 nil。
func allFailed(st []Status) error {
	if len(st) == 0 {
		return errors.New("没有可用的搜索源")
	}
	for _, s := range st {
		if s.OK {
			return nil
		}
	}
	return fmt.Errorf("所有来源都失败了：%s", st[0].Err)
}

func findSite(key string) (Site, bool) {
	key = strings.ToLower(strings.TrimSpace(key))
	for _, s := range Sites() {
		if s.Key == key {
			return s, true
		}
	}
	return Site{}, false
}

// httpClient 按代理配置建客户端。超时由调用方通过 ctx 控制，
// 这里再兜一层，避免某个站挂住整轮。
func httpClient(proxy string, timeout time.Duration) *http.Client {
	tr := &http.Transport{
		Proxy:                 netproxy.URL,
		ResponseHeaderTimeout: timeout,
		MaxIdleConnsPerHost:   4,
	}
	if proxy = strings.TrimSpace(proxy); proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

// NormalizeChannels 把用户填的一串频道名拆开：支持逗号、空格、换行，容忍带 @ 和 t.me 链接。
//
// Telegram 的 username 只能是 ASCII 字母/数字/下划线。用户常把频道的
// **显示名**（例如「Torrent Search Bot 🔎」）直接粘进来，按空格拆会变成
// Torrent / Search / Bot / 🔎 四个不存在的频道 —— 每个都要真打一次 t.me，
// 白占并发槽、拖慢整轮，还会把种子站挤到超时。含空格又带非 ASCII 字符的
// 整条直接丢掉；纯 ASCII 的空格分隔仍然照旧支持。
func NormalizeChannels(raw string) []string {
	separators := func(r rune) bool {
		return r == ',' || r == '，' || r == '\n' || r == '\r' || r == ';' || r == '；' || r == '、'
	}
	entries := strings.FieldsFunc(raw, separators)
	kept := make([]string, 0, len(entries))
	for _, e := range entries {
		if strings.ContainsAny(e, " \t") && !asciiOnly(e) {
			continue
		}
		kept = append(kept, e)
	}

	fields := strings.FieldsFunc(strings.Join(kept, ","), func(r rune) bool {
		return separators(r) || r == ' ' || r == '\t'
	})
	out := make([]string, 0, len(fields))
	seen := map[string]bool{}
	for _, f := range fields {
		f = strings.TrimSpace(f)
		f = strings.TrimPrefix(f, "@")
		if i := strings.Index(f, "t.me/"); i >= 0 {
			f = f[i+len("t.me/"):]
			f = strings.TrimPrefix(f, "s/") // t.me/s/<username> 预览页链接
		}
		f = strings.TrimSuffix(f, "/")
		// 纯 emoji / 纯标点不是 username，别让它变成一次注定失败的请求。
		if f == "" || !hasAlphaNum(f) || seen[strings.ToLower(f)] {
			continue
		}
		seen[strings.ToLower(f)] = true
		out = append(out, f)
	}
	return out
}

func asciiOnly(s string) bool {
	for _, r := range s {
		if r > 127 {
			return false
		}
	}
	return true
}

func hasAlphaNum(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			return true
		}
	}
	return false
}

// NormalizeSites 把用户填的站点 key 规整，只保留认识的。
func NormalizeSites(raw string) []string {
	known := map[string]string{}
	for _, s := range Sites() {
		known[s.Key] = s.Key
	}
	var out []string
	for _, f := range NormalizeChannels(raw) {
		if k, ok := known[strings.ToLower(f)]; ok {
			out = append(out, k)
		}
	}
	return out
}

// adWords 是种子站/公开频道里广告贴的高频词。
//
// 只收「几乎不可能出现在正常片名里」的词：宁少勿多，误杀比漏过更烦人。
var adWords = []string{
	"加微信", "加qq", "qq群", "微信群", "公众号", "官网", "博彩", "赌博", "棋牌",
	"时时彩", "六合彩", "真人荷官", "约炮", "招嫖", "卖淫", "代开发票", "刷单",
	"兼职日结", "优惠券", "返利", "代理加盟", "最新网址", "永久网址", "备用网址",
	"发布页", "防失联", "防走失", "点击进入", "点此进入", "下载app", "免费观看",
	"高清无码", "成人网站",
}

// adSizeFloor 是「不像正片」的体积下限：80 MB。
//
// 一段 4K 预告都不止这个数；几十 MB 的种子基本是广告页、样片或电子书。
// 体积未知（0）的条目不过滤 —— TG 文字帖本来就常常不给体积。
const adSizeFloor = 80 << 20

// dropAds 剔掉广告贴，返回保留项和被丢弃的条数。
func dropAds(items []Item) ([]Item, int) {
	if len(items) == 0 {
		return items, 0
	}
	out := items[:0]
	dropped := 0
	for _, it := range items {
		if isAd(it) {
			dropped++
			continue
		}
		out = append(out, it)
	}
	return out, dropped
}

func isAd(it Item) bool {
	if it.Size > 0 && it.Size < adSizeFloor {
		return true
	}
	low := strings.ToLower(it.Title)
	for _, w := range adWords {
		if strings.Contains(low, w) {
			return true
		}
	}
	return false
}
