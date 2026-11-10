package tmdb

// 单部作品的元数据：给「刮削」和「影视库海报墙」用。
//
// 和榜单（Discover）分开的原因：刮削要的是「一部片的全字段」，而且要能
//  1. 按中文片名搜（/search/movie、/search/tv，不是榜单的 discover）；
//  2. 按文件名里的 ttID 反查（/find，顺带拿到 TMDB 自己的 ID）；
//  3. 一次拿全 external_ids（IMDb ID）、genres、runtime、seasons。
//
// 所有请求都走 c.get()，也就是进程级上游（官方或用户反代），图片地址走
// imageURL() 拼同一个图片根 —— 刮削下载海报时不会偷偷绕回官方 CDN。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// DefaultKey 返回「设置页没填 Key」时用的兜底 TMDB v3 Key。
//
// 源码里不内置任何 Key：这是公共配额的凭证，一旦进公开仓库就会被爬虫
// 抓走、被别人刷到限流。默认从 STRM115_TMDB_KEY 环境变量读（部署者可以
// 在自己的 .env 里注入），没设就是空 —— 此时请在「设置 → 电影数据源」
// 填自己的 Key，电影墙 / 刮削 / 订阅照常工作。
//
// 放在 tmdb 包里是为了让电影墙、刮削、海报墙三处共用同一份兜底，
// 不要各写一份常量。做成函数而不是包级变量：测试可以直接用 t.Setenv
// 注入占位 Key，不必留一个可变的全局状态。
func DefaultKey() string {
	return strings.TrimSpace(os.Getenv("STRM115_TMDB_KEY"))
}

// MetaHit 是搜索结果里的一条候选。
//
// Title 是 zh-CN 的本地化标题，Original 是 original_title / original_name。
// 打分时两个都要比：中文文件名对得上 Title，英文文件名对得上 Original。
type MetaHit struct {
	ID         int     `json:"id"`
	Kind       string  `json:"kind"` // movie / tv
	Title      string  `json:"title"`
	Original   string  `json:"original,omitempty"`
	Year       string  `json:"year,omitempty"`
	Popularity float64 `json:"popularity,omitempty"`
	Poster     string  `json:"poster,omitempty"`
	Backdrop   string  `json:"backdrop,omitempty"`
	Vote       float64 `json:"vote,omitempty"`
	Overview   string  `json:"overview,omitempty"`
}

// MetaDetail 是一部作品的全量元数据。
//
// IMDbID 只作标识用（nfo 的 uniqueid、缓存键、海报墙的 IMDb 链接），
// 数据本身全部来自 TMDB。Seasons 是剧集的正式季号（已滤掉第 0 季花絮）。
type MetaDetail struct {
	ID       int      `json:"id"`
	Kind     string   `json:"kind"` // movie / tv
	IMDbID   string   `json:"imdb_id,omitempty"`
	Title    string   `json:"title"`    // zh-CN
	Original string   `json:"original"` // 原名
	Year     string   `json:"year,omitempty"`
	Rating   string   `json:"rating,omitempty"`
	Runtime  string   `json:"runtime,omitempty"` // "166 min"
	Genres   []string `json:"genres,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Poster   string   `json:"poster,omitempty"`
	Backdrop string   `json:"backdrop,omitempty"`
	Seasons  []int    `json:"seasons,omitempty"`
	// SeasonEpisodes 是每一季各有几集。它和 Seasons 来自**同一次** /tv/{id}
	// 响应（seasons[].episode_count），不额外发请求。
	//
	// 用途：详情抽屉里那条「起播线」滑杆要把「全剧第 N 集」换算成季/集。
	// 只给季号不给每季集数的话，多季剧的滑杆就只能瞎猜每季长度。
	SeasonEpisodes []SeasonCount `json:"season_episodes,omitempty"`
}

// SeasonCount 是一季的名义集数（TMDB 口径，含已定档未播出的）。
type SeasonCount struct {
	Season   int `json:"season"`
	Episodes int `json:"episodes"`
}

type metaSearchResp struct {
	Results []struct {
		ID            int     `json:"id"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		Name          string  `json:"name"`
		OriginalName  string  `json:"original_name"`
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		Popularity    float64 `json:"popularity"`
		PosterPath    string  `json:"poster_path"`
		BackdropPath  string  `json:"backdrop_path"`
		VoteAverage   float64 `json:"vote_average"`
		Overview      string  `json:"overview"`
	} `json:"results"`
}

// SearchMeta 按片名搜 TMDB。
//
// kind 传 "movie" 或 "series" 时只搜对应的那一个端点；传空则两个都搜
// （文件名解析不出类型时，宁可多一次请求也别漏掉）。带年份先查一次，
// 没结果再去掉年份查一遍 —— 片源标错年份、跨年上映都很常见。
func (c *Client) SearchMeta(ctx context.Context, query, year, kind string) ([]MetaHit, error) {
	return c.SearchMetaLang(ctx, query, year, kind, DefaultLanguage)
}

// SearchMetaLang 和 SearchMeta 一样，只是能指定返回标题的语言。
//
// 用途：设置里「查英文名」要的是 language=en-US 下的 title —— 对日本片、
// 韩国片来说，original_title 是日文/韩文，不是用户要的英文名。
func (c *Client) SearchMetaLang(ctx context.Context, query, year, kind, lang string) ([]MetaHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	var kinds []string
	switch kind {
	case KindMovie:
		kinds = []string{KindMovie}
	case KindTV, "series":
		kinds = []string{KindTV}
	default:
		kinds = []string{KindMovie, KindTV}
	}

	if strings.TrimSpace(lang) == "" {
		lang = DefaultLanguage
	}
	var out []MetaHit
	for _, k := range kinds {
		hits, err := c.searchMetaOnce(ctx, query, year, k, lang)
		if err != nil {
			return nil, err
		}
		if len(hits) == 0 && year != "" {
			if hits, err = c.searchMetaOnce(ctx, query, "", k, lang); err != nil {
				return nil, err
			}
		}
		out = append(out, hits...)
	}
	// 混排片名整串搜是搜不到的，拆成中文段、英文段再各搜一次。
	if len(out) == 0 {
		out = c.searchBilingual(ctx, query, year, kinds, lang)
	}
	return out, nil
}

// searchBilingual 是混排片名的兜底：中文段、英文段各搜一遍再合并。
//
// 单段请求失败只当作没结果 —— 这条路是兜底，主路径已经走完了，
// 不该因为兜底的一次抖动把整次搜索变成错误。
func (c *Client) searchBilingual(ctx context.Context, query, year string, kinds []string, lang string) []MetaHit {
	zh, en := splitBilingual(query)
	if zh == "" || en == "" {
		return nil
	}
	var out []MetaHit
	for _, q := range []string{zh, en} {
		for _, k := range kinds {
			hits, err := c.searchMetaOnce(ctx, q, year, k, lang)
			if err != nil {
				continue
			}
			if len(hits) == 0 && year != "" {
				hits, _ = c.searchMetaOnce(ctx, q, "", k, lang)
			}
			out = append(out, hits...)
		}
	}
	return dedupeHits(out)
}

// splitBilingual 把「极速：伪装者 Drive The Pretenders」拆成中文段与英文段。
//
// 整理前的 TMDB 校验、刮削都会把文件名解析出的片名整串丢给 TMDB。国内发布组
// 很爱写「中文名 + 英文原名」，而 TMDB 的搜索是单字段全文匹配 —— 拼在一起
// 就等于一个不存在的片名，结果恒为空。《极速：伪装者》就是这么被判成
// 「TMDB 里没找到」，一直躺在暂存目录里；它的英文原名 Drive The Pretenders
// 一搜就有（TMDB 的中文名是《假面恋人：换档追爱》，中文名确实对不上）。
//
// 数字跟着前一个字符走：「碟中谍8：最终清算」里的 8 留在中文段，
// 「... The Final Reckoning 2026」里的 2026 留在英文段。
// 两段都要求「像片名」才返回，免得拿 2160p 这种技术串去搜。
func splitBilingual(q string) (zh, en string) {
	var z, e strings.Builder
	const (
		modeNone = iota
		modeHan
		modeLatin
	)
	mode := modeNone
	for _, r := range q {
		switch {
		case unicode.Is(unicode.Han, r), r >= 0x3000 && r <= 0x303f, r >= 0xff01 && r <= 0xff60:
			mode = modeHan
			z.WriteRune(r)
		case r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z'):
			mode = modeLatin
			e.WriteRune(r)
		case r < 0x80 && (unicode.IsDigit(r) || r == ' ' || r == '\'' || r == '-' || r == '&' ||
			r == '.' || r == ':' || r == '!' || r == ','):
			// 数字和空格归给「当前这一段」，不让年份跑到另一段里。
			if mode == modeLatin {
				e.WriteRune(r)
			} else {
				z.WriteRune(r)
			}
		}
	}
	zh = strings.TrimSpace(z.String())
	en = strings.TrimSpace(e.String())
	if hanCount(zh) < 2 {
		zh = ""
	}
	if latinCount(en) < 3 {
		en = ""
	}
	return zh, en
}

func hanCount(s string) int {
	n := 0
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			n++
		}
	}
	return n
}

func latinCount(s string) int {
	n := 0
	for _, r := range s {
		if r < 0x80 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z') {
			n++
		}
	}
	return n
}

// dedupeHits 按 kind+id 去重，保持先后顺序：中文段的命中排在前面。
func dedupeHits(in []MetaHit) []MetaHit {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, h := range in {
		k := h.Kind + ":" + strconv.Itoa(h.ID)
		if _, ok := seen[k]; ok {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, h)
	}
	return out
}

func (c *Client) searchMetaOnce(ctx context.Context, query, year, kind, lang string) ([]MetaHit, error) {
	v := url.Values{
		"query":         {query},
		"language":      {lang},
		"include_adult": {"false"},
		"page":          {"1"},
	}
	if year != "" {
		if kind == KindTV {
			v.Set("first_air_date_year", year)
		} else {
			v.Set("year", year)
		}
	}
	endpoint := "/search/movie"
	if kind == KindTV {
		endpoint = "/search/tv"
	}
	raw, err := c.get(ctx, endpoint, v)
	if err != nil {
		return nil, fmt.Errorf("TMDB 搜索失败: %w", err)
	}
	var resp metaSearchResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 搜索结果失败: %w", err)
	}
	out := make([]MetaHit, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.ID <= 0 {
			continue
		}
		h := MetaHit{
			ID: r.ID, Kind: kind, Popularity: r.Popularity,
			Vote: r.VoteAverage, Overview: strings.TrimSpace(r.Overview),
			Poster: PosterURL(r.PosterPath), Backdrop: BackdropURL(r.BackdropPath),
		}
		date := r.ReleaseDate
		if kind == KindTV {
			h.Title = strings.TrimSpace(r.Name)
			h.Original = strings.TrimSpace(r.OriginalName)
			date = r.FirstAirDate
		} else {
			h.Title = strings.TrimSpace(r.Title)
			h.Original = strings.TrimSpace(r.OriginalTitle)
		}
		if len(date) >= 4 {
			h.Year = date[:4]
		}
		out = append(out, h)
	}
	return out, nil
}

// MetaByID 按 TMDB ID 取全量元数据。
// append_to_response=external_ids 把 IMDb ID 一起带回来，省一次请求。
func (c *Client) MetaByID(ctx context.Context, kind string, id int) (*MetaDetail, error) {
	if id <= 0 {
		return nil, fmt.Errorf("缺少 TMDB ID")
	}
	if kind == "" {
		kind = KindMovie
	}
	v := url.Values{
		"language":               {DefaultLanguage},
		"append_to_response":     {"external_ids"},
		"include_image_language": {"zh,en,null"},
	}
	raw, err := c.get(ctx, "/"+kind+"/"+strconv.Itoa(id), v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ID            int     `json:"id"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		Name          string  `json:"name"`
		OriginalName  string  `json:"original_name"`
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		VoteAverage   float64 `json:"vote_average"`
		Runtime       int     `json:"runtime"`
		EpisodeRun    []int   `json:"episode_run_time"`
		Overview      string  `json:"overview"`
		PosterPath    string  `json:"poster_path"`
		BackdropPath  string  `json:"backdrop_path"`
		Genres        []struct {
			Name string `json:"name"`
		} `json:"genres"`
		Seasons []struct {
			Number       int `json:"season_number"`
			EpisodeCount int `json:"episode_count"`
		} `json:"seasons"`
		External struct {
			IMDb string `json:"imdb_id"`
		} `json:"external_ids"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 详情失败: %w", err)
	}

	d := &MetaDetail{
		ID: id, Kind: kind, IMDbID: strings.TrimSpace(resp.External.IMDb),
		Overview: strings.TrimSpace(resp.Overview),
		Poster:   PosterURL(resp.PosterPath), Backdrop: BackdropURL(resp.BackdropPath),
	}
	date := resp.ReleaseDate
	if kind == KindTV {
		d.Title = strings.TrimSpace(resp.Name)
		d.Original = strings.TrimSpace(resp.OriginalName)
		date = resp.FirstAirDate
	} else {
		d.Title = strings.TrimSpace(resp.Title)
		d.Original = strings.TrimSpace(resp.OriginalTitle)
	}
	if len(date) >= 4 {
		d.Year = date[:4]
	}
	if resp.VoteAverage > 0 {
		d.Rating = fmt.Sprintf("%.1f", resp.VoteAverage)
	}
	mins := resp.Runtime
	if kind == KindTV && mins == 0 && len(resp.EpisodeRun) > 0 {
		mins = resp.EpisodeRun[0]
	}
	if mins > 0 {
		d.Runtime = strconv.Itoa(mins) + " min"
	}
	for _, g := range resp.Genres {
		if n := strings.TrimSpace(g.Name); n != "" {
			d.Genres = append(d.Genres, n)
		}
	}
	for _, s := range resp.Seasons {
		// 第 0 季是花絮 / 特辑，不写进正片清单。
		if s.Number <= 0 {
			continue
		}
		d.Seasons = append(d.Seasons, s.Number)
		// 集数可能没填（老剧、刚定档的新季），这种季只记季号，不记 0 集 ——
		// 前端拿到 0 会把滑杆长度算错。
		if s.EpisodeCount > 0 {
			d.SeasonEpisodes = append(d.SeasonEpisodes, SeasonCount{
				Season: s.Number, Episodes: s.EpisodeCount,
			})
		}
	}
	sort.Ints(d.Seasons)
	return d, nil
}

// MetaByIMDbID 用文件名里的 ttID 反查 TMDB 条目，再取详情。
//
// wantKind 是文件名解析出的类型；/find 同时返回电影和剧集结果，
// 按我们认出来的类型优先挑（IMDb ID 在 TMDB 偶尔挂错类型，见
// credits.go 里赛博朋克那个例子）。挑不到才退回另一种。
func (c *Client) MetaByIMDbID(ctx context.Context, imdbID, wantKind string) (*MetaDetail, error) {
	imdbID = strings.TrimSpace(imdbID)
	if imdbID == "" {
		return nil, fmt.Errorf("缺少 IMDb ID")
	}
	raw, err := c.get(ctx, "/find/"+url.PathEscape(imdbID), url.Values{
		"external_source": {"imdb_id"},
		"language":        {DefaultLanguage},
	})
	if err != nil {
		return nil, err
	}
	var out struct {
		Movies []struct {
			ID int `json:"id"`
		} `json:"movie_results"`
		TV []struct {
			ID int `json:"id"`
		} `json:"tv_results"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解析 TMDB find 结果失败: %w", err)
	}
	wantTV := wantKind == KindTV || wantKind == "tv" || wantKind == "series"
	var first, second int
	var firstKind, secondKind string
	if wantTV {
		if len(out.TV) > 0 {
			first, firstKind = out.TV[0].ID, KindTV
		}
		if len(out.Movies) > 0 {
			second, secondKind = out.Movies[0].ID, KindMovie
		}
	} else {
		if len(out.Movies) > 0 {
			first, firstKind = out.Movies[0].ID, KindMovie
		}
		if len(out.TV) > 0 {
			second, secondKind = out.TV[0].ID, KindTV
		}
	}
	if first > 0 {
		return c.MetaByID(ctx, firstKind, first)
	}
	if second > 0 {
		return c.MetaByID(ctx, secondKind, second)
	}
	return nil, fmt.Errorf("TMDB 里没有对应的条目（%s）", imdbID)
}

// SeasonEpisodesMeta 拉若干季的分集清单，返回按「季 → 集」排好的结果。
//
// 只按调用方真正需要的季去请求（文件名里的 S 号），不给 60 季的老剧
// 一次打 60 个接口。单季失败不影响其它季。
func (c *Client) SeasonEpisodesMeta(ctx context.Context, tvID int, seasons []int) ([]SeasonEpisode, error) {
	var all []SeasonEpisode
	var firstErr error
	for _, n := range seasons {
		if n <= 0 {
			continue
		}
		eps, err := c.SeasonEpisodes(ctx, tvID, n)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		all = append(all, eps...)
	}
	return all, firstErr
}
