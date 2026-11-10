// Package tmdb 查 The Movie DB 的榜单，给「电影墙」提供海报、简介和评分。
//
// 之所以用 TMDB 一家：数据量大，排序有三个维度（热门 / 高分 / 新片），
// 图片走 image.tmdb.org，中文标题和简介直接由 language=zh-CN 给出。
// 之前那套 JustWatch 数据源已经下线 —— 它只有热门一种排序、游标分页，
// 而且接口偶尔拦请求，维护两份收益不大。
//
// 两个接口的坑记在这里：
//
//  1. discover 的返回**没有 imdb_id**，也没有每部片挂了哪些平台。前者要逐条打
//     /movie/{id}/external_ids（这里做了并发补全 + 进程内缓存），后者只在详情里
//     查一次 /movie/{id}/watch/providers。
//  2. with_watch_providers 必须配 watch_region，否则参数被忽略 —— 不报错，
//     只是平台筛选悄悄失效，很难查。
//  3. 图片是相对路径：poster_path 要拼 https://image.tmdb.org/t/p/<size>。
//  4. 电影和剧集的类型 ID 不是一套（剧集没有「恐怖」「音乐」，但有「真人秀」），
//     所以 genreID 分 kind 查表。
package tmdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"strm115/internal/netproxy"
)

const (
	// DefaultBase 是 v3 接口的根。想走反代就换这个。
	DefaultBase = "https://api.themoviedb.org/3"
	// ImageBase 是图片 CDN，路径要自己拼尺寸。
	ImageBase = "https://image.tmdb.org/t/p"

	// DefaultLanguage 直接要简体中文，标题和简介都是中文，省一次翻译。
	DefaultLanguage = "zh-CN"
	// DefaultRegion 对应 watch_region=US（和 justwatch.com/us 的美国区一致）。
	DefaultRegion = "US"

	// PosterSize / BackdropSize 是图片档位。w342 大致等于卡片宽度，
	// w1280 是详情页背景 —— 再大就是浪费流量。
	PosterSize   = "w342"
	BackdropSize = "w1280"

	KindMovie = "movie"
	KindTV    = "tv"
)

// Sort 是榜单的排序方式。
const (
	SortPopular = "popular" // 现在最热
	SortTop     = "top"     // 高分（评分降序 + 票数门槛，接近 IMDb 高分榜的观感）
	SortNew     = "new"     // 新上线
)

// upstream 是进程级的 TMDB 上游地址。
//
// 之所以放在包里而不是给每个调用点传参：电影墙、演员刮削、订阅三个包各自
// NewClient，穿一遍参数要把 scrape.New / subscribe.Deps 的签名全改一遍；
// 而「这一刻用哪个上游」本来就是整个进程共享的一件事（像 http.DefaultTransport）。
// 用 atomic.Value 是因为用户可以在设置页运行时改地址，而上述三个包会在
// 不同 goroutine 里同时构造 Client，普通变量会 data race。
var upstream atomic.Value // 里面存 upstreamConfig

type upstreamConfig struct{ api, image string }

func init() {
	upstream.Store(upstreamConfig{api: DefaultBase, image: ImageBase})
}

// SetUpstream 切换 TMDB 上游，立即对之后构造的 Client 生效。
//
// apiBase 是 v3 接口根，例如 https://proxy.example/3；空串恢复官方。
// imageBase 是图片 CDN 根，例如 https://proxy.example/t/p；留空时，
// 若 apiBase 以 /3 结尾，就按 imaliang/tmdb-proxy 的 vercel.json 路由推导
// （去掉 /3 再拼 /t/p）—— 它把 /t/p/:path* rewrite 到 image.tmdb.org。
// apiBase 不是这个形状时保持官方图片 CDN，不去猜用户自建反代的路径。
func SetUpstream(apiBase, imageBase string) {
	raw := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	api := raw
	if api == "" {
		api = DefaultBase
	}
	image := strings.TrimRight(strings.TrimSpace(imageBase), "/")
	if image == "" {
		image = ImageBase
		// 只有显式填了反代地址、且形状是 /3 结尾才推导图片根。
		// 恢复官方（raw 为空）时不能套：官方接口是 api.themoviedb.org/3，
		// 图片却是另一个域名 image.tmdb.org/t/p，硬推会拼出一个不存在的地址。
		if raw != "" && strings.HasSuffix(api, "/3") {
			image = strings.TrimSuffix(api, "/3") + "/t/p"
		}
	}
	upstream.Store(upstreamConfig{api: api, image: image})
}

// CurrentUpstream 返回当前生效的接口根与图片根。
func CurrentUpstream() (api, image string) {
	c := upstream.Load().(upstreamConfig)
	return c.api, c.image
}

// Client 是一个很薄的 REST 客户端。零值不可用，请用 NewClient。
type Client struct {
	key  string
	hc   *http.Client
	Base string
}

// sharedHTTP 复用连接池。电影墙、订阅、刮削、详情各自 NewClient，
// 以前每次都新建一个 http.Client，等于每轮请求重新做 TCP + TLS 握手；
// 上游地址（Base）仍存在各自的 Client 上，换反代不受影响。
//
// Proxy 必须是函数而不是固定 URL：这个 Client 是包级单例，电影墙/日历/刮削
// 启动时就握着它。以前写死 http.ProxyFromEnvironment，只认环境变量，
// 设置页填的 net_proxy 完全到不了这里 —— 「代理不起作用」正是这个洞。
var sharedHTTP = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		Proxy:                 netproxy.URL,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	},
}

// NewClient 返回带超时的客户端。Key 为空时所有请求都会返回 ErrNoKey。
func NewClient(key string) *Client {
	api, _ := CurrentUpstream()
	return &Client{
		key:  strings.TrimSpace(key),
		hc:   sharedHTTP,
		Base: api,
	}
}

// ErrNoKey 表示没配 API Key。调用方据此决定是回退到别的数据源还是报错。
var ErrNoKey = errors.New("没有配置 TMDB API Key")

// Query 是一次榜单查询。
type Query struct {
	Kind      string // movie / tv
	Providers []int  // TMDB 平台 ID：8 = Netflix、9 = Prime Video …
	Genre     int    // TMDB 类型 ID；0 表示不限
	Sort      string // popular / top / new
	Page      int    // 1 起
	Region    string // 留空用 DefaultRegion
	Language  string // 留空用 DefaultLanguage
	// OriginCountry 只给「TMDB 没有该平台片单」的国内平台兜底用：
	// 设成 CN 时按国产内容出榜，而不是假装能查到该平台的上架片单。
	OriginCountry string
}

// Item 是一部片子在墙上要用的全部信息。
type Item struct {
	ID       int      `json:"id"`
	Kind     string   `json:"kind"` // movie / series
	Title    string   `json:"title"`
	Original string   `json:"original,omitempty"`
	Year     int      `json:"year,omitempty"`
	Rating   float64  `json:"rating,omitempty"` // TMDB 评分（10 分制）
	Votes    int      `json:"votes,omitempty"`
	Desc     string   `json:"description,omitempty"`
	Poster   string   `json:"poster,omitempty"`
	Backdrop string   `json:"backdrop,omitempty"`
	Genres   []string `json:"genres,omitempty"` // 中文
	// IMDb 由 FillIMDb 补上。discover 本身不返回它。
	IMDb string `json:"imdb,omitempty"`
	// Lang 是原始语言，前端用它判断要不要拿英文原名去搜种子站。
	Lang string `json:"lang,omitempty"`
}

// Page 是一页结果。
type Page struct {
	Items      []Item
	Page       int
	TotalPages int
	HasMore    bool
}

// Discover 拉一页榜单。
func (c *Client) Discover(ctx context.Context, q Query) (*Page, error) {
	if c.key == "" {
		return nil, ErrNoKey
	}
	if q.Kind != KindTV {
		q.Kind = KindMovie
	}
	if q.Page <= 0 {
		q.Page = 1
	}
	if q.Region == "" {
		q.Region = DefaultRegion
	}
	if q.Language == "" {
		q.Language = DefaultLanguage
	}

	v := url.Values{}
	v.Set("language", q.Language)
	v.Set("page", strconv.Itoa(q.Page))
	v.Set("include_adult", "false")
	// 平台筛选必须带地区，否则 with_watch_providers 会被静默忽略。
	if len(q.Providers) > 0 {
		v.Set("watch_region", q.Region)
		v.Set("with_watch_providers", joinInts(q.Providers, "|"))
	}
	if q.Genre > 0 {
		v.Set("with_genres", strconv.Itoa(q.Genre))
	}
	if q.OriginCountry != "" {
		v.Set("with_origin_country", q.OriginCountry)
	}

	dateField := "primary_release_date"
	if q.Kind == KindTV {
		dateField = "first_air_date"
	}
	switch q.Sort {
	case SortTop:
		// 只按评分排会把「1 个人打了 10 分」的片顶到最上面，
		// 所以压一个票数门槛 —— 这就是「IMDb 高分榜」的观感来源。
		v.Set("sort_by", "vote_average.desc")
		v.Set("vote_count.gte", "300")
	case SortNew:
		v.Set("sort_by", dateField+".desc")
		v.Set("vote_count.gte", "5")
		// 不设上限的话会混进一堆只有预告片的未来影片。
		v.Set(dateField+".lte", time.Now().Format("2006-01-02"))
	default:
		v.Set("sort_by", "popularity.desc")
	}

	raw, err := c.get(ctx, "/discover/"+q.Kind, v)
	if err != nil {
		return nil, err
	}
	var out discoverResp
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 TMDB 响应失败: %w", err)
	}

	res := &Page{Page: out.Page, TotalPages: out.TotalPages}
	if res.Page <= 0 {
		res.Page = q.Page
	}
	for _, r := range out.Results {
		res.Items = append(res.Items, r.toItem(q.Kind))
	}
	res.HasMore = res.Page < out.TotalPages
	return res, nil
}

// FillIMDb 并发把每条的 IMDb ID 补上。
//
// discover 不返回 imdb_id，只能逐条问 /external_ids。30 条并发 4 大约 1 秒多，
// 而且结果按 TMDB ID 缓存在进程里 —— 翻回上一页、换排序再翻，都不会重复请求。
// 单条失败不影响整页（那部片就只是没有 IMDb 链接）。
func (c *Client) FillIMDb(ctx context.Context, items []Item, workers int) {
	if c.key == "" || len(items) == 0 {
		return
	}
	if workers <= 0 {
		workers = 4
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := range items {
		key := items[i].Kind + ":" + strconv.Itoa(items[i].ID)
		if v, ok := imdbCache.Load(key); ok {
			items[i].IMDb, _ = v.(string)
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			id, err := c.imdbID(ctx, items[i].Kind, items[i].ID)
			if err != nil {
				return
			}
			imdbCache.Store(key, id)
			items[i].IMDb = id
		}(i)
	}
	wg.Wait()
}

// imdbCache 是 tmdbID → imdbID 的进程内缓存。
// 放包级是因为 Client 每次请求都新建一个，缓存挂在实例上等于没有。
var imdbCache sync.Map

func (c *Client) imdbID(ctx context.Context, kind string, id int) (string, error) {
	raw, err := c.get(ctx, "/"+kind+"/"+strconv.Itoa(id)+"/external_ids", nil)
	if err != nil {
		return "", err
	}
	var out struct {
		IMDb string `json:"imdb_id"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", err
	}
	return strings.TrimSpace(out.IMDb), nil
}

func (c *Client) get(ctx context.Context, path string, v url.Values) ([]byte, error) {
	if c.key == "" {
		return nil, ErrNoKey
	}
	if v == nil {
		v = url.Values{}
	}
	v.Set("api_key", c.key)
	u := c.Base + path + "?" + v.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "strm115/1.0")

	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求 TMDB 失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		// TMDB 的错误体长这样：{"status_code":7,"status_message":"Invalid API key"}。
		// 把 message 提出来，比一个裸 401 有用得多。
		var e struct {
			Message string `json:"status_message"`
		}
		if json.Unmarshal(body, &e) == nil && e.Message != "" {
			return nil, fmt.Errorf("TMDB 返回 %d: %s", resp.StatusCode, e.Message)
		}
		return nil, fmt.Errorf("TMDB 返回 %d", resp.StatusCode)
	}
	return body, nil
}

func (r discoverResult) toItem(kind string) Item {
	it := Item{
		ID:       r.ID,
		Kind:     kindLabel(kind),
		Desc:     strings.TrimSpace(r.Overview),
		Rating:   round1(r.VoteAverage),
		Votes:    r.VoteCount,
		Poster:   PosterURL(r.PosterPath),
		Backdrop: BackdropURL(r.BackdropPath),
		Lang:     r.OriginalLang,
		Genres:   genreNames(kind, r.GenreIDs),
	}
	if kind == KindTV {
		it.Title = strings.TrimSpace(r.Name)
		it.Original = strings.TrimSpace(r.OriginalName)
		it.Year = yearOf(r.FirstAirDate)
	} else {
		it.Title = strings.TrimSpace(r.Title)
		it.Original = strings.TrimSpace(r.OriginalTitle)
		it.Year = yearOf(r.ReleaseDate)
	}
	if it.Original == it.Title {
		it.Original = ""
	}
	return it
}

// PosterURL 把 poster_path 补成图片地址。空路径（没海报的冷门片）返回空串。
func PosterURL(path string) string { return imageURL(path, PosterSize) }

// BackdropURL 是详情页背景图。
func BackdropURL(path string) string { return imageURL(path, BackdropSize) }

func imageURL(path, size string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	_, base := CurrentUpstream()
	return base + "/" + size + path
}

// PageURL 是 TMDB 上这部片的详情页。
func PageURL(kind string, id int) string {
	if kind == KindTV || kind == "series" {
		return "https://www.themoviedb.org/tv/" + strconv.Itoa(id)
	}
	return "https://www.themoviedb.org/movie/" + strconv.Itoa(id)
}

// Genres 返回该 kind 可用的类型（code → 中文名），给前端画筛选条。
// 电影和剧集不是一套 ID：剧集没有「恐怖」「音乐」，但有「真人秀」「脱口秀」。
func Genres(kind string) [][2]string {
	if kind == KindTV {
		return tvGenres
	}
	return movieGenres
}

// GenreID 把前端传的 code（act / scf …）换成 TMDB 的数字 ID。
// 不认识或者该 kind 没有的类型返回 0，调用方当作「不限」。
func GenreID(kind, code string) int {
	code = strings.ToLower(strings.TrimSpace(code))
	table := movieGenreID
	if kind == KindTV {
		table = tvGenreID
	}
	return table[code]
}

func genreNames(kind string, ids []int) []string {
	if len(ids) == 0 {
		return nil
	}
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if n := genreByID(kind, id); n != "" {
			names = append(names, n)
		}
	}
	return names
}

// genreByID 反向查中文名。表很小，线性扫比维护两张反查表清楚。
func genreByID(kind string, id int) string {
	table := movieGenreID
	if kind == KindTV {
		table = tvGenreID
	}
	for code, gid := range table {
		if gid != id {
			continue
		}
		// 同一个 ID 可能被两个 code 指到（比如 fnt 和 scf 都指向剧集的 10765），
		// 中文名以 Genres() 里的顺序为准。
		for _, g := range Genres(kind) {
			if g[0] == code {
				return g[1]
			}
		}
	}
	return ""
}

func kindLabel(kind string) string {
	if kind == KindTV {
		return "series"
	}
	return "movie"
}

func yearOf(date string) int {
	date = strings.TrimSpace(date)
	if len(date) < 4 {
		return 0
	}
	n, _ := strconv.Atoi(date[:4])
	return n
}

func round1(f float64) float64 {
	return float64(int(f*10+0.5)) / 10
}

func joinInts(v []int, sep string) string {
	parts := make([]string, 0, len(v))
	for _, n := range v {
		parts = append(parts, strconv.Itoa(n))
	}
	return strings.Join(parts, sep)
}

type discoverResp struct {
	Page         int              `json:"page"`
	TotalPages   int              `json:"total_pages"`
	TotalResults int              `json:"total_results"`
	Results      []discoverResult `json:"results"`
}

type discoverResult struct {
	ID            int     `json:"id"`
	Title         string  `json:"title"`
	Name          string  `json:"name"`
	OriginalTitle string  `json:"original_title"`
	OriginalName  string  `json:"original_name"`
	Overview      string  `json:"overview"`
	PosterPath    string  `json:"poster_path"`
	BackdropPath  string  `json:"backdrop_path"`
	VoteAverage   float64 `json:"vote_average"`
	VoteCount     int     `json:"vote_count"`
	GenreIDs      []int   `json:"genre_ids"`
	ReleaseDate   string  `json:"release_date"`
	FirstAirDate  string  `json:"first_air_date"`
	OriginalLang  string  `json:"original_language"`
}

// movieGenres / tvGenres 决定筛选条上的顺序：大片种在前，冷门的沉底。
// code 是短名（act / scf / trl …），前端和 URL 参数都用它，别改。
var movieGenres = [][2]string{
	{"act", "动作"},
	{"scf", "科幻"},
	{"trl", "悬疑"},
	{"crm", "犯罪"},
	{"drm", "剧情"},
	{"cmy", "喜剧"},
	{"hrr", "恐怖"},
	{"rma", "爱情"},
	{"ani", "动画"},
	{"fnt", "奇幻"},
	{"war", "战争"},
	{"hst", "历史"},
	{"doc", "纪录"},
	{"fml", "家庭"},
	{"msc", "音乐"},
	{"adv", "冒险"},
	{"wsn", "西部"},
}

var tvGenres = [][2]string{
	{"act", "动作冒险"},
	{"scf", "科幻奇幻"},
	{"trl", "悬疑"},
	{"crm", "犯罪"},
	{"drm", "剧情"},
	{"cmy", "喜剧"},
	{"ani", "动画"},
	{"doc", "纪录"},
	{"fml", "家庭"},
	{"war", "战争政治"},
	{"rly", "真人秀"},
	{"wsn", "西部"},
}

var movieGenreID = map[string]int{
	"act": 28,
	"adv": 12,
	"ani": 16,
	"cmy": 35,
	"crm": 80,
	"doc": 99,
	"drm": 18,
	"fml": 10751,
	"fnt": 14,
	"hst": 36,
	"hrr": 27,
	"msc": 10402,
	"trl": 9648,
	"rma": 10749,
	"scf": 878,
	"war": 10752,
	"wsn": 37,
}

var tvGenreID = map[string]int{
	"act": 10759,
	"ani": 16,
	"cmy": 35,
	"crm": 80,
	"doc": 99,
	"drm": 18,
	"fml": 10751,
	"trl": 9648,
	"rly": 10764,
	"scf": 10765,
	"war": 10768,
	"wsn": 37,
}
