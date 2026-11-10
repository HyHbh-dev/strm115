package httpapi

// 电影墙：把「某地区 + 某几个流媒体平台上有什么」变成一面海报墙。
//
// 数据源只有 TMDB（internal/tmdb）：热门 / 高分 / 新片三种排序，图片走
// image.tmdb.org，评分是 TMDB 的（10 分制）。discover 不返回 imdb_id，
// 逐条并发补一次 —— 那是「已入库」标记和 IMDb 链接的依据。
//
// 这个页面只做「发现」：不碰网盘、不写库、不下载图片。
//
// 缓存：整页榜单按「类型+平台+类型+排序+页码+地区」存进 meta_cache 的 JSON。
// 平时进页面只读这份缓存（不联网），只有点「刷新」带 refresh=1 才穿透去重抓
// TMDB。翻页、切筛选、来回切页签都不会反复吃 TMDB 配额，也不会每次都等 2 秒。

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/tmdb"
)

// wallTTL 是榜单缓存寿命。过期后第一次普通访问也会重抓一次并回写，
// 但「点刷新」永远穿透，用户想立刻看新片时不用等过期。
const wallTTL = 6 * time.Hour

// wallItem 是一张海报卡片需要的全部字段。
type wallItem struct {
	IMDbID      string   `json:"imdb_id,omitempty"`
	Name        string   `json:"name"`
	Original    string   `json:"original,omitempty"`
	Type        string   `json:"type"`
	Year        string   `json:"year,omitempty"`
	Rating      string   `json:"rating,omitempty"`
	Runtime     string   `json:"runtime,omitempty"`
	Genres      []string `json:"genres,omitempty"` // 中文
	Description string   `json:"description,omitempty"`
	Poster      string   `json:"poster,omitempty"`
	Background  string   `json:"background,omitempty"`
	// Providers 是这部片挂的平台（Netflix / Prime Video …），卡片上打标。
	// TMDB 的列表接口不返回这个，只在详情里查（见 /api/wall/watch）。
	Providers []string `json:"providers,omitempty"`
	// Link 是数据源的详情页，TMDB 模式下是 themoviedb.org。
	Link string `json:"link,omitempty"`
	// TMDBID 是详情弹窗查平台用的。
	TMDBID int `json:"tmdb_id,omitempty"`
	// InLibrary 表示这部片子在本地刮削台账里已经有 —— 卡片上打个「已入库」。
	InLibrary bool `json:"in_library"`
}

// wallPayload 是真正进缓存的东西。
//
// 刻意不把 provider_options / genre_options 放进缓存：这两份清单是后端常量，
// 加平台后不该因为旧缓存还压着 6 小时才在筛选条上出现。每次响应都现算。
type wallPayload struct {
	Items     []wallItem `json:"items"`
	Source    string     `json:"source"`
	Page      int        `json:"page"`
	HasMore   bool       `json:"has_more"`
	Type      string     `json:"type"`
	Genre     string     `json:"genre"`
	Providers []string   `json:"providers"`
	Country   string     `json:"country"`
	Sort      string     `json:"sort"`
	CachedAt  string     `json:"cached_at"`
}

// providerCodes 是允许出现在查询里的平台短名。
//
// 做成白名单有两个原因：URL 参数直接进查询变量，不能让任意字符串透传；
// 另外前端筛选条要用同一份清单，从后端拿就不会两边对不上。
// TMDB 那一列是 watch providers 的数字 ID（8=Netflix，9=Prime Video…）。
//
// 国内平台分两种：
//   - TMDB 收录了 provider ID 的（腾讯视频 623 / 爱奇艺 581 / WeTV 509），
//     按 ID 真查片单。TMDB 的 CN 区 watch providers 是空的，这几家在
//     US/HK/SG 等区有数据，所以仍按默认 US 区查。
//   - TMDB 完全没有 provider 数据的（优酷 / 芒果TV / 哔哩哔哩 / 咪咕），
//     TMDB=0，选中时退回「国产内容」榜（with_origin_country=CN）。
//     Note 会显示在筛选条上，明确这不是该平台的真实上架片单，避免误导。
var providerCodes = []struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	TMDB  int    `json:"tmdb"`
	Group string `json:"group"`          // overseas / cn
	Note  string `json:"note,omitempty"` // 兜底说明，显示在 chip 上
}{
	{"nfx", "Netflix", 8, "overseas", ""},
	{"amp", "Prime Video", 9, "overseas", ""},
	{"dnp", "Disney+", 337, "overseas", ""},
	{"mxx", "HBO Max", 1899, "overseas", ""},
	{"atp", "Apple TV+", 350, "overseas", ""},
	{"hlu", "Hulu", 15, "overseas", ""},
	{"ppm", "Paramount+", 531, "overseas", ""},
	{"pcp", "Peacock", 386, "overseas", ""},

	{"tx", "腾讯视频", 623, "cn", ""},
	{"iqy", "爱奇艺", 581, "cn", ""},
	{"wetv", "WeTV", 509, "cn", ""},
	{"yku", "优酷", 0, "cn", "TMDB 没有优酷片单，按国产内容推荐"},
	{"mgtv", "芒果TV", 0, "cn", "TMDB 没有芒果TV片单，按国产内容推荐"},
	{"bili", "哔哩哔哩", 0, "cn", "TMDB 没有哔哩哔哩片单，按国产内容推荐"},
	{"migu", "咪咕视频", 0, "cn", "TMDB 没有咪咕视频片单，按国产内容推荐"},
}

type wallProvider struct {
	Code  string
	Name  string
	TMDB  int
	Group string
	Note  string
}

func providerByCode(code string) *wallProvider {
	for _, p := range providerCodes {
		if p.Code == code {
			return &wallProvider{p.Code, p.Name, p.TMDB, p.Group, p.Note}
		}
	}
	return nil
}

func knownProvider(code string) bool { return providerByCode(code) != nil }

func tmdbProviderID(code string) int {
	if p := providerByCode(code); p != nil {
		return p.TMDB
	}
	return 0
}

func (s *Server) handleWall(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	typ := strings.TrimSpace(q.Get("type"))
	kind := tmdb.KindMovie
	if typ == "series" {
		kind = tmdb.KindTV
	}

	// providers=nfx,amp → ["nfx","amp"]；不认识的值直接丢掉，不做报错，
	// 免得用户手改 URL 之后整面墙空白。
	var providers []string
	for _, code := range strings.Split(q.Get("providers"), ",") {
		code = strings.ToLower(strings.TrimSpace(code))
		if code != "" && knownProvider(code) {
			providers = append(providers, code)
		}
	}

	genre := strings.ToLower(strings.TrimSpace(q.Get("genre")))

	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 60 {
		limit = 30
	}

	// 只有显式点「刷新」才穿透缓存去重抓 TMDB；普通请求一律先读缓存。
	refresh := q.Get("refresh") == "1"

	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// source 参数只为兼容旧书签保留：数据源已经只有 TMDB 一家。
	s.wallFromTMDB(w, r, ctx, kind, typ, genre, providers, limit, refresh)
}

// wallCacheKey 把影响结果的参数全塞进键里，少一个就会串榜单。
func wallCacheKey(kind string, providers []string, genre, sort string,
	page int, region, origin string, limit int) string {
	return strings.Join([]string{
		"wall", kind, strings.Join(providers, ","), genre, sort,
		strconv.Itoa(page), region, origin, strconv.Itoa(limit),
	}, ":")
}

func (s *Server) readWallCache(ctx context.Context, key string) (*wallPayload, bool) {
	_, raw, ok, err := s.st.GetMetaCache(ctx, key)
	if err != nil || !ok || raw == "" {
		return nil, false
	}
	var p wallPayload
	if json.Unmarshal([]byte(raw), &p) != nil {
		return nil, false
	}
	at, err := time.Parse(time.RFC3339, p.CachedAt)
	if err != nil || time.Since(at) > wallTTL {
		return nil, false
	}
	return &p, true
}

func (s *Server) saveWallCache(ctx context.Context, key string, p wallPayload) {
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	// 缓存写失败不影响这一次的结果展示，只留个日志。
	if err := s.st.PutMetaCache(ctx, key, "", string(raw)); err != nil {
		s.log.Warn("电影墙缓存写入失败", "err", err)
	}
}

// wallFromTMDB 拉一页榜单并补上 IMDb ID。缓存命中时根本不会调用 TMDB。
func (s *Server) wallFromTMDB(w http.ResponseWriter, r *http.Request, ctx context.Context,
	kind, typ, genre string, providers []string, limit int, refresh bool) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	region := strings.ToUpper(strings.TrimSpace(q.Get("country")))
	if region == "" {
		region = tmdb.DefaultRegion
	}
	sortBy := defaultString(strings.TrimSpace(q.Get("sort")), tmdb.SortPopular)

	ids := make([]int, 0, len(providers))
	originCountry := ""
	for _, code := range providers {
		if id := tmdbProviderID(code); id > 0 {
			ids = append(ids, id)
		} else if p := providerByCode(code); p != nil && p.TMDB == 0 {
			// 没有 provider 数据的国内平台：退回国产内容榜。
			originCountry = "CN"
		}
	}

	key := wallCacheKey(kind, providers, genre, sortBy, page, region, originCountry, limit)
	if !refresh {
		if cached, ok := s.readWallCache(ctx, key); ok {
			s.writeWall(w, cached, true, kind)
			return
		}
	}

	client := tmdb.NewClient(s.tmdbKey(ctx))
	pg, err := client.Discover(ctx, tmdb.Query{
		Kind:          kind,
		Providers:     ids,
		Genre:         tmdb.GenreID(kind, genre),
		Sort:          sortBy,
		Page:          page,
		Region:        region,
		Language:      tmdb.DefaultLanguage,
		OriginCountry: originCountry,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}

	// discover 不返回 imdb_id：并发补一轮，约一两秒。
	client.FillIMDb(ctx, pg.Items, 6)

	items := make([]wallItem, 0, len(pg.Items))
	for _, it := range pg.Items {
		if it.Title == "" {
			continue
		}
		item := wallItem{
			IMDbID:      it.IMDb,
			Name:        it.Title,
			Original:    it.Original,
			Type:        it.Kind,
			Genres:      it.Genres,
			Description: it.Desc,
			Poster:      it.Poster,
			Background:  it.Backdrop,
			Link:        tmdb.PageURL(kind, it.ID),
			TMDBID:      it.ID,
		}
		if it.Year > 0 {
			item.Year = strconv.Itoa(it.Year)
		}
		if it.Rating > 0 {
			item.Rating = strconv.FormatFloat(it.Rating, 'f', 1, 64)
		}
		items = append(items, item)
	}

	if len(items) > 0 {
		if known, err := s.st.KnownIMDbIDs(ctx, imdbIDsOf(items)); err == nil {
			for i := range items {
				items[i].InLibrary = known[items[i].IMDbID]
			}
		}
	}

	payload := &wallPayload{
		Items:     items,
		Source:    "tmdb",
		Page:      pg.Page,
		HasMore:   pg.HasMore,
		Type:      typOrMovie(typ),
		Genre:     genre,
		Providers: providers,
		Country:   region,
		Sort:      sortBy,
		CachedAt:  time.Now().Format(time.RFC3339),
	}
	// 强制刷新也要回写，否则「刷新完再切回筛选」又会拿到上一版。
	s.saveWallCache(ctx, key, *payload)
	s.writeWall(w, payload, false, kind)
}

// handleWallProviders 只回平台清单，不打 TMDB、不吃缓存。
// 「每个平台一行」的海报墙要先把所有行标题画出来，再逐行懒加载内容；
// 以前 provider_options 只藏在 /api/wall 的响应里，页面得先等一整页榜单。
func (s *Server) handleWallProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"provider_options": providerCodes})
}

// writeWall 把平台清单 / 类型清单现算拼上，不让它们被旧缓存冻结。
func (s *Server) writeWall(w http.ResponseWriter, p *wallPayload, cached bool, kind string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"items":            p.Items,
		"source":           p.Source,
		"page":             p.Page,
		"has_more":         p.HasMore,
		"type":             p.Type,
		"genre":            p.Genre,
		"providers":        p.Providers,
		"country":          p.Country,
		"sort":             p.Sort,
		"cached":           cached,
		"cached_at":        p.CachedAt,
		"provider_options": providerCodes,
		"genre_options":    tmdb.Genres(kind),
	})
}

// tmdbKey 读用户配的 Key；没配就用 tmdb.DefaultKey 兜底。
//
// 兜底值来自 STRM115_TMDB_KEY 环境变量（源码里不内置任何 Key），
// 两者都没有时返回空串 —— 界面会提示去设置页填自己的 Key。
func (s *Server) tmdbKey(ctx context.Context) string {
	if k := strings.TrimSpace(s.setting(ctx, "tmdb_api_key")); k != "" {
		return k
	}
	return tmdb.DefaultKey()
}

func imdbIDsOf(items []wallItem) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if it.IMDbID != "" {
			out = append(out, it.IMDbID)
		}
	}
	return out
}

func typOrMovie(t string) string {
	if t == "series" {
		return "series"
	}
	return "movie"
}

func defaultString(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// 兜底 Key 定义在 tmdb.DefaultKey（来自 STRM115_TMDB_KEY 环境变量）：
// 电影墙、刮削、海报墙三处共用同一份，这里不要再写第二份常量。
