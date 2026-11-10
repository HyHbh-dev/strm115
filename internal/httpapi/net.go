package httpapi

// 网络检测与资源搜索两个接口。都比较薄：真正的活在各自己的包里，
// 这里只负责「读设置 → 传参 → 兜住超时」。

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"strm115/internal/douban"
	"strm115/internal/imdb"
	"strm115/internal/netcheck"
	"strm115/internal/netproxy"
	"strm115/internal/resource"
	"strm115/internal/tmdb"
)

// netProxy 返回出站代理：先看设置，再看环境变量。
//
// 只做一件事，但很关键：docker 里部署时，设置页填的通常是「宿主机的代理」，
// 而纯命令行部署更习惯用环境变量 —— 两种都支持，省得为了一个字段重建容器。
func (s *Server) netProxy(ctx context.Context) string {
	raw := strings.TrimSpace(s.setting(ctx, "net_proxy"))
	if raw == "" {
		for _, k := range []string{"NET_PROXY", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				raw = v
				break
			}
		}
	}
	// 容器里 127.0.0.1 指向容器自己，宿主机上的代理会连不上。
	// 库里保留用户填的原值，只在「此刻要用的地址」上改写。
	return netproxy.Normalize(raw)
}

// applyNetProxy 把「此刻应该生效的代理」推给进程级开关。
//
// 只改数据库是不够的：tmdb / tvdb / tvmaze / 夸克登录这些包级单例
// 在启动时就把 http.Client 建好了，读不到设置页后来的修改。
// netproxy.Set 改的是一个每次请求都会读的值，所以保存完立即生效。
// 启动时调一次，保存设置时再调一次。
func (s *Server) applyNetProxy(ctx context.Context) {
	p := s.netProxy(ctx)
	if _, ok := validProxy(p); !ok {
		p = "" // 坏地址当直连，别让整站外网请求一起挂掉
	}
	netproxy.Set(p)
}

// validProxy 检查代理地址能不能用。格式不对就别往下走 ——
// 否则所有「经代理」的检查都会变成「连不上」，用户还以为是网络问题。
func validProxy(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", true
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", false
	}
	switch u.Scheme {
	case "http", "https", "socks5", "socks5h":
		return raw, true
	}
	return "", false
}

// handleNetCheck 把所有端点戳一遍。整轮 20 秒足够，超了就让前端重试。
func (s *Server) handleNetCheck(w http.ResponseWriter, r *http.Request) {
	proxy, ok := validProxy(s.netProxy(r.Context()))
	if !ok {
		writeErr(w, http.StatusBadRequest, errors.New("代理地址格式不对，应该像 http://192.168.1.10:7890"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	rep := netcheck.Run(ctx, proxy)
	// 代理没配也别让前端猜：显式告诉它「这一轮没有代理行」
	writeJSON(w, http.StatusOK, map[string]any{
		"proxy":      rep.Proxy,
		"results":    rep.Results,
		"ok":         rep.OK,
		"total":      rep.Total,
		"checked_at": rep.CheckedAt,
	})
}

// handleResSearch 在 TG 频道和种子站里搜一个片名。
//
// 参数：q=关键词（TG 用），en=英文原名（种子站用，可选），
// kind=tg|torrent（留空=都搜），limit=每个源取几条（默认 20）。
// TG 频道清单和启用的站点都从设置读，前端不用重复维护一份。
//
// 中文片名丢给海盗湾、Nyaa 基本一条都对不上，所以种子站单独走 en：
// 前端知道原名（电影墙的 original_title）就直接传；不知道就传空，
// 这里用 IMDb 搜一次换英文名 —— 拿不到就还是用原词，不额外报错。
func (s *Server) handleResSearch(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()

	q := strings.TrimSpace(query.Get("q"))
	if q == "" {
		writeErr(w, http.StatusBadRequest, errors.New("先输入要搜的片名"))
		return
	}

	limit := 20
	if v := strings.TrimSpace(query.Get("limit")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n // 上限由 resource.Search 再兜一次（50）
		}
	}

	kind := strings.TrimSpace(query.Get("kind"))
	// refresh=1 只有「重搜」按钮会带：默认几分钟内复用缓存，
	// 切换来源筛选是前端本地过滤，压根不该再发一次请求。
	refresh := query.Get("refresh") == "1"

	res, err := s.searchResource(r.Context(), resSearchOpts{
		Q: q, Kind: kind, EN: strings.TrimSpace(query.Get("en")),
		Limit: limit, Refresh: refresh,
	})
	if res == nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	// 告诉前端「换名」这一步的结果：换到了就提示种子站按哪个词搜，
	// 没换到就明说种子站只能按中文搜 —— 否则用户只会看到「一条都没有」。
	// domestic（国产片，故意不换英文名）由 searchResource 直接写进 EnLookup；
	// 其余情况在这里按「有没有换到英文名」算出来。
	if res.EnLookup == "" {
		res.EnLookup = enLookupState(q, kind, res.SiteQuery)
	}
	if err != nil {
		// 一个源都没成功：仍然把 sources 返回去，前端能列出每个源失败的原因，
		// 这比一句「搜索失败」有用得多。
		res.Error = err.Error()
	}
	writeJSON(w, http.StatusOK, res)
}

// handleResTest 探测单个搜索源，供设置页的「测试」按钮用。
//
// 为什么要单独一个接口，而不是让前端「搜一次看看有没有结果」：
// 「这个源没这条片」和「这个源已经挂了」在搜索结果里长得一模一样。
// 这里把「连没连上 / 解析出几条 / 失败原文」分开返回，用户才知道
// 该改地址还是该换关键词。
func (s *Server) handleResTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Kind   string `json:"kind"`
		Target string `json:"target"`
		Query  string `json:"query"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	proxy, ok := validProxy(s.netProxy(r.Context()))
	if !ok {
		proxy = "" // 代理坏了当直连，和搜索一致：探测本身不该整页报错
	}
	res, err := s.res.Test(r.Context(), resource.TestOptions{
		Kind:   body.Kind,
		Target: body.Target,
		Query:  body.Query,
		Proxy:  proxy,
	})
	if err != nil {
		// 参数类错误（目标为空、kind 写错）才走到这里；源连不上是正常结果。
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// resSearchOpts 是一次资源搜索的完整参数。抽成结构体是为了让「重搜」
// 多带一个 Refresh 时不用动订阅、机器人两处调用签名。
type resSearchOpts struct {
	Q, Kind, EN string
	Limit       int
	Refresh     bool
}

// resourceSearch 是订阅轮询器和 Telegram 机器人共用的入口。
// HTTP 接口走 searchResource（它会带 Refresh）。
func (s *Server) resourceSearch(ctx context.Context, q, kind, en string, limit int) (*resource.Result, error) {
	return s.searchResource(ctx, resSearchOpts{Q: q, Kind: kind, EN: en, Limit: limit})
}

// searchResource 是搜索的真正实现，网页、机器人、订阅三处共用。
//
// 共用一份的意义：三条入口如果各写一套参数拼装，很快就会出现
// 「网页搜得到、机器人搜不到」— 用户没法判断是哪边的问题。
//
// en 是英文原名（种子站用）；留空且查询里有中文时先问 TMDB、再退回 IMDb。
// 换到的英文名同时给 TG 频道再搜一遍：中文频道里有大量英文标题的帖子，
// 只按中文搜会漏掉一大半。
func (s *Server) searchResource(ctx context.Context, o resSearchOpts) (*resource.Result, error) {
	limit := o.Limit
	if limit <= 0 {
		limit = 20
	}
	proxy, ok := validProxy(s.netProxy(ctx))
	if !ok {
		proxy = "" // 搜索是「尽最大努力」的场景，代理坏了就当直连，别整页报错
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	// 中文片名先问豆瓣：国产/港台片根本没有「英文原名」这回事，
	// 以前却照样 TMDB 3 秒 + IMDb 4 秒地白等，还多打一轮 t.me 英文搜索。
	// 豆瓣命中并判成国产就直接按中文搜，外国片则用豆瓣的 original_title。
	names := s.resolveSearchNames(ctx, o.Q, o.EN)
	siteQ := names.SiteQuery
	// TG 频道用「不带年份」的英文名：帖子标题里通常不写年份，
	// 带年份的 ?q= 会把本来能命中的帖子全过滤掉。
	channelQ := ""
	if !names.Domestic {
		channelQ = channelQueryFor(o.Q, siteQ)
	}

	res, err := s.res.Search(ctx, resource.Options{
		Query:        o.Q,
		SiteQuery:    siteQ,
		ChannelQuery: channelQ,
		Kind:         o.Kind,
		Channels:     resource.NormalizeChannels(s.setting(ctx, "tg_channels")),
		Sites:        resource.NormalizeSites(s.setting(ctx, "res_sites")),
		Custom:       s.setting(ctx, "res_sites_custom"),
		Proxy:        proxy,
		Limit:        limit,
		Timeout:      12 * time.Second,
		Refresh:      o.Refresh,
	})
	if res == nil {
		return nil, err
	}
	// 结果可能来自缓存，先把上一轮留下的换名标记清掉再写，免得国产标记串到
	// 别的查询上；非国产的情况由 handleResSearch 按 site_query 算。
	res.EnLookup = ""
	if names.Domestic {
		res.EnLookup = "domestic"
	}
	return res, err
}

// channelQueryFor 决定 TG 频道要不要额外搜一遍英文、用哪个词。
//
// 只有「确实换到了一个和中文查询不同的拉丁名字」才值得多打一次 t.me；
// 中文原名、或者用户本来就输的英文词都会让两次搜索完全重复。
func channelQueryFor(q, siteQ string) string {
	en := stripYear(strings.TrimSpace(siteQ))
	if en == "" || strings.EqualFold(en, strings.TrimSpace(q)) || !hasLatin(en) {
		return ""
	}
	return en
}

// stripYear 去掉英文名末尾的四位年份（"Dune Part Two 2024" → "Dune Part Two"）。
// 只切末尾，中间的年份是片名的一部分（"2001: A Space Odyssey"）不能动。
func stripYear(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 5 {
		return s
	}
	tail := s[len(s)-4:]
	if (strings.HasPrefix(tail, "19") || strings.HasPrefix(tail, "20")) && isDigits(tail) &&
		(len(s) == 4 || s[len(s)-5] == ' ') {
		return strings.TrimSpace(s[:len(s)-5])
	}
	return s
}

// hasCJK 判断查询里有没有汉字/假名。只有这种查询才值得花一次 IMDb 请求换英文名 ——
// 英文标题本来就在英文索引里能搜到，多查一次只是拖慢。
func hasCJK(s string) bool {
	for _, r := range s {
		switch {
		case r >= 0x3040 && r <= 0x30FF: // 平假名 / 片假名
			return true
		case r >= 0x3400 && r <= 0x9FFF: // 汉字（含扩展 A）
			return true
		case r >= 0xF900 && r <= 0xFAFF: // 兼容汉字
			return true
		}
	}
	return false
}

// hasLatin 判断标题里有没有拉丁字母 —— 用来挡掉 IMDb 返回的另一条中文片名。
func hasLatin(s string) bool {
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			return true
		}
	}
	return false
}

// enLookupState 汇总「中文名换英文名」这一步的结果，写进响应的 en_lookup。
// 只搜 TG 时不走种子站，也就没换名这回事，返回空。
func enLookupState(q, kind, siteQ string) string {
	switch {
	case !hasCJK(q) || kind == "tg":
		return ""
	case siteQ != "" && siteQ != q:
		return "ok"
	default:
		return "fail"
	}
}

// nameLookup 是一次资源搜索最终用的词。
type nameLookup struct {
	SiteQuery string // 种子站用的词，空 = 用原中文查询
	Domestic  bool   // 国产/港台内容：故意不换英文名
	Source    string // explicit / douban / fallback / none，给日志和测试看
}

// resolveSearchNames 决定「种子站 / TG 频道各自拿什么词搜」。
//
// 以前中文查询一律 TMDB → IMDb 换英文名，国产剧也要白等 3+4 秒，换回来的
// 还常常是张冠李戴的英文片名 —— 这就是「找资源国内为什么也刷新慢」。
// 现在的顺序：
//  1. 用户/认片器已经给了英文名 → 直接用，一个请求都不发
//  2. 中文查询 → 豆瓣 suggest + detail（国内直连很快）
//     · 判成国产/港台 → 不换名，siteQ 留空，channelQ 也留空
//     · 外国片 → 用豆瓣的 original_title
//  3. 豆瓣关了 / 没认出 → 才退回原来的 TMDB → IMDb
func (s *Server) resolveSearchNames(ctx context.Context, q, en string) nameLookup {
	return s.resolveSearchNamesWith(ctx, q, en, s.englishTitle)
}

// resolveSearchNamesWith 把「退回 TMDB/IMDb」这一步做成参数，单测才不用真联网。
func (s *Server) resolveSearchNamesWith(ctx context.Context, q, en string,
	fallback func(context.Context, string) string) nameLookup {
	if v := strings.TrimSpace(en); v != "" {
		return nameLookup{SiteQuery: v, Source: "explicit"}
	}
	if !hasCJK(q) {
		return nameLookup{Source: "none"}
	}
	if v, domestic := s.doubanName(ctx, q); domestic || v != "" {
		if domestic {
			s.log.Info("资源搜索：豆瓣判定国产内容，按中文名直搜", "query", q)
			return nameLookup{Domestic: true, Source: "douban"}
		}
		s.log.Info("资源搜索：豆瓣换英文名", "query", q, "english", v)
		return nameLookup{SiteQuery: v, Source: "douban"}
	}
	return nameLookup{SiteQuery: fallback(ctx, q), Source: "fallback"}
}

// doubanName 用豆瓣认片并返回「种子站该用的英文原名」。
// domestic=true 表示这是国产/港台内容，压根不该去找英文名。
//
// 两次请求是有意的：suggest 只给 id 和演员表，制片地区与 original_title
// 都在详情里。豆瓣国内直连通常几百毫秒，比 TMDB+IMDb 那串便宜得多。
// 整段最多 2.5 秒，任何失败都返回零值让调用方退回老链路。
func (s *Server) doubanName(ctx context.Context, q string) (string, bool) {
	if !s.doubanOn(ctx) {
		return "", false
	}
	cctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	c := s.doubanClient(ctx)

	// 搜索词末尾的年份会挡住豆瓣的模糊匹配（"沙丘2 2024"），去掉再问。
	hits, err := c.Search(cctx, stripYear(q), 1)
	if err != nil || len(hits) == 0 || !titleLooksLike(q, hits[0].Title) {
		return "", false
	}
	it, err := c.Detail(cctx, hits[0].Kind, hits[0].ID)
	if err != nil || it == nil {
		return "", false
	}
	if isDomesticDouban(it) {
		return "", true
	}
	out := strings.TrimSpace(it.Original)
	if !hasLatin(out) {
		return "", false
	}
	if it.Year > 0 && !strings.HasSuffix(out, strconv.Itoa(it.Year)) {
		out += " " + strconv.Itoa(it.Year)
	}
	return out, false
}

// titleLooksLike 防住豆瓣把「同名的另一部片」当成第一条返回。
// 只做粗匹配：去掉年份、空白和常见标点后互为包含即可，认不出就宁可退回 TMDB。
func titleLooksLike(q, hit string) bool {
	norm := func(v string) string {
		v = strings.ToLower(stripYear(strings.TrimSpace(v)))
		return strings.Map(func(r rune) rune {
			switch {
			case r == ' ' || r == '\t' || r == '　' || r == '·' || r == ':' || r == '：' || r == '-':
				return -1
			}
			return r
		}, v)
	}
	a, b := norm(q), norm(hit)
	if a == "" || b == "" {
		return false
	}
	return strings.Contains(a, b) || strings.Contains(b, a)
}

// isDomesticDouban 用豆瓣详情里的制片地区判断国产内容。
// card_subtitle 形如「2023 / 中国大陆 / 剧情」，pubdate 形如「2023-04-22(中国大陆)」。
func isDomesticDouban(it *douban.Item) bool {
	text := it.CardSubtitle + " " + it.ReleaseText
	for _, k := range []string{"中国大陆", "中国香港", "中国台湾", "内地"} {
		if strings.Contains(text, k) {
			return true
		}
	}
	return false
}

// englishTitle 把中文片名换成英文原名：先 TMDB（有规范的 original_title），
// 查不到再退回 IMDb 的自动建议接口。
//
// 为什么是两级而不是只用一家：TMDB 遇到冷门老片、译名不全会返回空，
// 而 IMDb 的建议接口覆盖面更广；反过来 IMDb 的建议经常张冠李戴，
// 有 TMDB 时优先用更准的那个。
func (s *Server) englishTitle(ctx context.Context, q string) string {
	if v := s.tmdbEnglishTitle(ctx, q); v != "" {
		s.log.Info("资源搜索：TMDB 换英文名", "query", q, "english", v)
		return v
	}
	return s.imdbEnglishTitle(ctx, q)
}

// tmdbEnglishTitle 走 /search/multi 拿英文原名，最多等 3 秒。
// 失败一律返回空串：换名只是搜索前的加分项，不该让整轮搜索失败。
func (s *Server) tmdbEnglishTitle(ctx context.Context, q string) string {
	cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	hit, err := tmdb.NewClient(s.tmdbKey(ctx)).SearchOriginal(cctx, q)
	if err != nil || hit == nil || hit.Name == "" {
		if err != nil {
			s.log.Warn("资源搜索：TMDB 换英文名失败，退回 IMDb", "query", q, "err", err)
		}
		return ""
	}
	out := strings.TrimSpace(hit.Name)
	if y := strings.TrimSpace(hit.Year); y != "" && !strings.HasSuffix(out, y) {
		out += " " + y
	}
	return out
}

// imdbEnglishTitle 用 IMDb 把中文片名换成英文原名（TMDB 查不到时的兜底）。
//
// 两条约束：
//   - 最多等 4 秒。这只是搜索前的一次「翻译」，不该让整轮搜索卡在 IMDb 上；
//     imdb 包内部没有 ctx，所以丢进 goroutine 里 select 一下，超时就走人。
//   - 任何失败都返回空字符串：调用方会退回原词，搜索照常进行，
//     同时在响应的 en_lookup 里标 fail，前端会提示用户改输英文名。
func (s *Server) imdbEnglishTitle(ctx context.Context, q string) string {
	key := s.setting(ctx, settingIMDbKey) // 先读出来：goroutine 里不再碰 store
	proxy, _ := validProxy(s.netProxy(ctx))

	ch := make(chan string, 1)
	go func() {
		resp, err := imdbSearch(q, key, proxy)
		if err != nil || resp == nil {
			ch <- ""
			return
		}
		ch <- pickEnglish(resp, q)
	}()

	select {
	case v := <-ch:
		if v != "" {
			s.log.Info("资源搜索：中文名换英文名", "query", q, "english", v)
		} else {
			s.log.Warn("资源搜索：IMDb 没查到英文名，用原词搜种子站", "query", q)
		}
		return v
	case <-time.After(4 * time.Second):
		s.log.Warn("资源搜索：IMDb 换英文名超时，用原词搜种子站", "query", q)
		return ""
	}
}

// imdbSearch 按 key 查 IMDb；配了 Key 但官方接口失败（Key 无效 / 没配额 /
// 接口变更）时自动退回免 Key 的自动建议接口。
//
// 为什么要退回：填错一个 Key 就把「中文片名换英文名」整条链路搞死，
// 代价太大 —— 免 Key 那条路本来就能用。
func imdbSearch(q, key, proxy string) (*imdb.SearchResponse, error) {
	resp, err := imdb.NewClientWithProxy(key, proxy).Search(q)
	if err == nil || strings.TrimSpace(key) == "" {
		return resp, err
	}
	fb, fbErr := imdb.NewClientWithProxy("", proxy).Search(q)
	if fbErr != nil {
		return nil, err // 两条都失败：报官方那条，通常更有信息量
	}
	return fb, nil
}

// pickEnglish 从 IMDb 结果里挑一个英文名，写成「标题 年份」。
//
// 优先挑年份和查询里那个四位年份对得上的 —— 「三体 2023」这种情况，
// 第一条往往是 2024 年的 Netflix 版，不加年份就会搜错片。
func pickEnglish(resp *imdb.SearchResponse, q string) string {
	if resp == nil {
		return ""
	}
	year := queryYear(q)
	var fallback string
	for _, it := range resp.Results {
		t := strings.TrimSpace(it.Title)
		if t == "" || hasCJK(t) || !hasLatin(t) {
			continue
		}
		if y := strings.TrimSpace(it.Year); y != "" && !strings.Contains(t, y) {
			t += " " + y
		}
		if year == "" || strings.TrimSpace(it.Year) == year {
			return t
		}
		if fallback == "" {
			fallback = t
		}
	}
	return fallback
}

// queryYear 抓查询里的四位年份（19xx / 20xx）。没有就返回空。
func queryYear(q string) string {
	for i := 0; i+4 <= len(q); i++ {
		seg := q[i : i+4]
		if (strings.HasPrefix(seg, "19") || strings.HasPrefix(seg, "20")) &&
			isDigits(seg) {
			return seg
		}
	}
	return ""
}

func isDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(s) > 0
}

// ──────────────────────────────────────────────
// IMDb 检测（设置 → 电影墙数据源）
// ──────────────────────────────────────────────

// imdbProbeQuery 是检测用的固定查询：一个中文片名。
// 用它而不是 "Inception"，是因为这条链路真正要证明的是「中文名能换出英文名」，
// 而不仅仅是「接口能连上」。
const imdbProbeQuery = "沙丘2"

type imdbCheckResult struct {
	OK      bool   `json:"ok"`
	Mode    string `json:"mode"` // official / suggestion
	KeySet  bool   `json:"key_set"`
	MS      int64  `json:"ms"`
	Sample  string `json:"sample,omitempty"`  // 第一条命中的标题（调试用）
	English string `json:"english,omitempty"` // 中文名换出来的英文名
	Error   string `json:"error,omitempty"`
	Note    string `json:"note,omitempty"`
}

// handleIMDbCheck 检测 IMDb：用前端刚输入的 Key（留空则用存的那份）真查一次。
//
// 为什么不并进 /api/net/check：那条只证明「网络能到」，分不清是断网、
// 还是 Key 无效、还是配额用光。这里拿 Key 打一次真实查询，三种情况能分开。
func (s *Server) handleIMDbCheck(w http.ResponseWriter, r *http.Request) {
	key := strings.TrimSpace(r.URL.Query().Get("key"))
	if key == "" {
		key = s.setting(r.Context(), settingIMDbKey)
	}
	proxy, ok := validProxy(s.netProxy(r.Context()))
	if !ok {
		proxy = ""
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	done := make(chan imdbCheckResult, 1)
	go func() { done <- imdbProbe(key, proxy) }()

	select {
	case res := <-done:
		writeJSON(w, http.StatusOK, res)
	case <-ctx.Done():
		writeJSON(w, http.StatusOK, imdbCheckResult{
			Mode: modeOf(key), KeySet: key != "", Error: "检测超时（20 秒没响应）",
		})
	}
}

func modeOf(key string) string {
	if strings.TrimSpace(key) != "" {
		return "official"
	}
	return "suggestion"
}

// imdbProbe 真打一次 IMDb。key 非空时先试官方接口，失败再试免 Key 接口，
// 并把这个「降级」如实写进 Note —— 用户需要知道 Key 坏了但搜索还能用。
func imdbProbe(key, proxy string) imdbCheckResult {
	started := time.Now()
	out := imdbCheckResult{Mode: modeOf(key), KeySet: strings.TrimSpace(key) != ""}

	resp, err := imdbSearch(imdbProbeQuery, key, proxy)
	out.MS = time.Since(started).Milliseconds()

	if err != nil {
		out.Error = err.Error()
		if out.KeySet {
			out.Note = "Key 不可用；免 Key 接口也不通，资源搜索只能按中文搜种子站"
		} else {
			out.Note = "免 Key 接口不通，资源搜索没法把中文名换成英文名"
		}
		return out
	}

	out.OK = true
	if out.KeySet && resp != nil && resp.Source == "suggestion" {
		// imdbSearch 内部降级了：官方接口没成，但免 Key 那条通了。
		out.Mode = "suggestion"
		out.Note = "Key 不可用（或已失效），资源搜索会自动退回免 Key 接口"
	}
	if resp != nil {
		if len(resp.Results) > 0 {
			out.Sample = strings.TrimSpace(resp.Results[0].Title)
		}
		out.English = pickEnglish(resp, imdbProbeQuery)
	}
	if out.OK && out.Note == "" && !out.KeySet {
		out.Note = "当前没配 Key，用的是免 Key 的自动建议接口"
	}
	return out
}
