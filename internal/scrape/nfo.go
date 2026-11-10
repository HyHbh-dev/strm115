package scrape

import (
	"bytes"
	"encoding/xml"
	"strconv"
	"strings"

	"strm115/internal/tmdb"
)

// NFO 是 Emby / Jellyfin / Kodi 共用的元数据文件，本质就是一份 XML。
//
// 我们只写确实拿到的字段。空标签比没有标签更糟：Emby 看到空的 <title>
// 会覆盖掉它自己从文件名推断的名字，于是界面上出现一片空白。

type nfoMovie struct {
	XMLName   xml.Name    `xml:"movie"`
	Title     string      `xml:"title,omitempty"`
	Original  string      `xml:"originaltitle,omitempty"`
	SortTitle string      `xml:"sorttitle,omitempty"`
	Year      string      `xml:"year,omitempty"`
	Plot      string      `xml:"plot,omitempty"`
	Rating    string      `xml:"rating,omitempty"`
	Runtime   string      `xml:"runtime,omitempty"` // 分钟
	UniqueIDs []nfoUnique `xml:"uniqueid"`
	Genres    []string    `xml:"genre"`
	Directors []string    `xml:"director"`
	Actors    []nfoActor  `xml:"actor"`
}

// nfoTVShow 是剧集根目录的 tvshow.nfo。
//
// 没有它的时候，Emby 只能靠目录名猜剧名：海报没有、简介没有、
// 分季也认不全。它和每集的 episodedetails 是两回事，缺一个都不完整。
type nfoTVShow struct {
	XMLName   xml.Name    `xml:"tvshow"`
	Title     string      `xml:"title,omitempty"`
	Original  string      `xml:"originaltitle,omitempty"`
	SortTitle string      `xml:"sorttitle,omitempty"`
	Year      string      `xml:"year,omitempty"`
	Plot      string      `xml:"plot,omitempty"`
	Rating    string      `xml:"rating,omitempty"`
	UniqueIDs []nfoUnique `xml:"uniqueid"`
	Genres    []string    `xml:"genre"`
	Directors []string    `xml:"director"`
	Actors    []nfoActor  `xml:"actor"`
}

type nfoEpisode struct {
	XMLName   xml.Name    `xml:"episodedetails"`
	Title     string      `xml:"title,omitempty"`
	ShowTitle string      `xml:"showtitle,omitempty"`
	Season    int         `xml:"season"`
	Episode   int         `xml:"episode"`
	Plot      string      `xml:"plot,omitempty"`
	Rating    string      `xml:"rating,omitempty"`
	Runtime   string      `xml:"runtime,omitempty"`
	Aired     string      `xml:"aired,omitempty"`
	UniqueIDs []nfoUnique `xml:"uniqueid"`
	Directors []string    `xml:"director"`
	Actors    []nfoActor  `xml:"actor"`
}

type nfoUnique struct {
	Type    string `xml:"type,attr"`
	Default bool   `xml:"default,attr,omitempty"`
	Value   string `xml:",chardata"`
}

type nfoActor struct {
	Name  string `xml:"name"`
	Role  string `xml:"role,omitempty"`
	Order int    `xml:"order"`
	// Thumb 是演员头像的远程地址。Emby 扫库时会把它下载下来，
	// 这是「演员头像不显示」的根因修复：没有这个标签，Emby 里的
	// 人物就是一个灰底占位图，点进去也没有图。
	Thumb string `xml:"thumb,omitempty"`
}

// Meta 是一次刮削拿到的全部内容，nfo 与图片都以它为准。
type Meta struct {
	IMDbID string `json:"imdb_id"`
	// TMDbID 是刮削命中的 TMDB 条目 ID。纯 TMDB 之后它是真正的主键：
	// IMDbID 只是 TMDB external_ids 顺带带回的标识，可能为空。
	TMDbID  int      `json:"tmdb_id"`
	Title   string   `json:"title"` // 作品原名（TMDB original_title / original_name）
	Year    string   `json:"year"`
	Rating  string   `json:"rating"`
	Runtime string   `json:"runtime"` // 原始文本，如 "166 min"
	Genres  []string `json:"genres"`
	Cast    []string `json:"cast"`
	// CastDetail 是带角色名和头像的演员表，来自 TMDB。
	// Cast 保留成逗号串是给搜索/列表用的；写 nfo 用这份。
	CastDetail []tmdb.CastMember `json:"cast_detail,omitempty"`
	Director   []string          `json:"director"`
	Overview   string            `json:"overview"`
	Poster     string            `json:"poster"`   // 远程 URL
	Backdrop   string            `json:"backdrop"` // 远程 URL
	Source     string            `json:"source"`   // tmdb / cache
	// Kind 是 TMDB 声明的类型（movie / series）。
	// 文件名猜错类型时（比如剧集被当成电影），以它为准。
	Kind string `json:"kind,omitempty"`
	// TitleCN 是中文名。优先取文件名里的中文（发行方译名），
	// 没有才拿 Title 机翻。它决定 nfo 的 <title>，而 Title 进 <originaltitle>。
	TitleCN string `json:"title_cn,omitempty"`
	// Episodes 是剧集的单集清单。整季一次性拿到，写 nfo 时按 S/E 取。
	Episodes []Episode `json:"episodes,omitempty"`
	// EpisodesFetched 标记「单集清单这件事已经问过了」。
	// 区分「老缓存没这个字段」和「查了但确实没有单集」——
	// 否则前者会永远拿到一季空标题，后者会被反复重查。
	EpisodesFetched bool `json:"episodes_fetched,omitempty"`
	// EpisodesSeasons 记下这份缓存里实际拉过哪几季。
	// 后来新增了 S02 的文件时，缓存里只有 S01 的集标题会让新一季全退回剧名，
	// 所以缓存命中要按「需要的季是否都覆盖」来判断，而不是只看 EpisodesFetched。
	EpisodesSeasons []int `json:"episodes_seasons,omitempty"`
}

// displayTitle 是写进 <title> 的名字：中文优先，没有中文才退回原名。
func (m Meta) displayTitle() string {
	if cn := strings.TrimSpace(m.TitleCN); cn != "" {
		return cn
	}
	return strings.TrimSpace(m.Title)
}

// Episode 是剧集里的一集。
type Episode struct {
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Title    string `json:"title"`
	Overview string `json:"overview"`
	Aired    string `json:"aired"`
}

// episodeOf 按季集号取单集信息。没有清单时返回零值，调用方退回剧级信息。
func (m Meta) episodeOf(season, episode int) Episode {
	for _, e := range m.Episodes {
		if e.Season == season && e.Episode == episode {
			return e
		}
	}
	return Episode{}
}

// BuildMovieNFO 生成电影 nfo。poster/backdrop 传本地文件名（与 nfo 同目录），
// uniqueIDs 给 nfo 写两份 uniqueid：TMDB 是这次的主源，IMDb 保留是为了
// Emby 里已有的 IMDb 关联、外部插件和老库的 nfo 不失效。
//
// default=true 只能有一个：优先 IMDb（老条目都靠它认），TMDB 没登记
// external_ids 时才让 TMDB ID 当默认。TMDB ID 一定写 —— 海报墙要靠它
// 直查详情，不能只存在我们自己的台账里。
func uniqueIDs(m Meta) []nfoUnique {
	out := make([]nfoUnique, 0, 2)
	if id := strings.TrimSpace(m.IMDbID); id != "" {
		out = append(out, nfoUnique{Type: "imdb", Default: true, Value: id})
	}
	if m.TMDbID > 0 {
		out = append(out, nfoUnique{Type: "tmdb", Default: len(out) == 0,
			Value: strconv.Itoa(m.TMDbID)})
	}
	return out
}

// 空串表示没下载到图片，这时不写标签，让 Emby 自己去联网找。
func BuildMovieNFO(m Meta) ([]byte, error) {
	n := nfoMovie{
		Title:     m.displayTitle(),
		Original:  strings.TrimSpace(m.Title),
		SortTitle: m.displayTitle(),
		Year:      strings.TrimSpace(m.Year),
		Plot:      strings.TrimSpace(m.Overview),
		Rating:    strings.TrimSpace(m.Rating),
		Runtime:   runtimeMinutes(m.Runtime),
		UniqueIDs: uniqueIDs(m),
		Genres:    cleanList(m.Genres),
		Directors: cleanList(m.Director),
		Actors:    toActors(m),
	}
	return marshalNFO(n)
}

// BuildTVShowNFO 生成剧集根目录的 tvshow.nfo。
//
// 与单集 nfo 的分工：这里只写「剧」级别的标题/简介/海报，
// 集标题和集简介在每集的 episodedetails 里，重复写会让 Emby 用剧简介
// 覆盖掉每一集的简介。
func BuildTVShowNFO(m Meta) ([]byte, error) {
	n := nfoTVShow{
		Title:     m.displayTitle(),
		Original:  strings.TrimSpace(m.Title),
		SortTitle: m.displayTitle(),
		Year:      strings.TrimSpace(m.Year),
		Plot:      strings.TrimSpace(m.Overview),
		Rating:    strings.TrimSpace(m.Rating),
		UniqueIDs: uniqueIDs(m),
		Genres:    cleanList(m.Genres),
		Directors: cleanList(m.Director),
		Actors:    toActors(m),
	}
	return marshalNFO(n)
}

// BuildEpisodeNFO 生成单集 nfo。
func BuildEpisodeNFO(m Meta, season, episode int) ([]byte, error) {
	// 有单集标题就用单集的，没有才退回剧名。
	//
	// 这看着是小事，但一季 24 集全叫同一个名字时，Emby 的剧集列表就是
	// 24 行一模一样的字，挑集全靠序号——体验很差，而补全它的成本是零：
	// Cinemeta 的剧级 meta 本来就带着全部单集的 name / overview / released。
	title := m.displayTitle()
	plot := strings.TrimSpace(m.Overview)
	aired := ""
	if ep := m.episodeOf(season, episode); ep.Title != "" || ep.Overview != "" {
		if t := strings.TrimSpace(ep.Title); t != "" {
			title = t
		}
		if o := strings.TrimSpace(ep.Overview); o != "" {
			plot = o
		}
		aired = airedDate(ep.Aired)
	}

	n := nfoEpisode{
		Title:     title,
		ShowTitle: m.displayTitle(),
		Season:    season,
		Episode:   episode,
		Plot:      plot,
		Aired:     aired,
		Rating:    strings.TrimSpace(m.Rating),
		Runtime:   runtimeMinutes(m.Runtime),
		UniqueIDs: uniqueIDs(m),
		Directors: cleanList(m.Director),
		Actors:    toActors(m),
	}
	return marshalNFO(n)
}

// airedDate 把 "2022-09-13T07:00:00.000Z" 变成 Emby 认的 "2022-09-13"。
func airedDate(s string) string {
	s = strings.TrimSpace(s)
	if len(s) < 10 {
		return ""
	}
	d := s[:10]
	for i, c := range d {
		if i == 4 || i == 7 {
			if c != '-' {
				return ""
			}
			continue
		}
		if c < '0' || c > '9' {
			return ""
		}
	}
	return d
}

func marshalNFO(v any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	enc := xml.NewEncoder(&buf)
	enc.Indent("", "  ")
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	buf.WriteByte('\n')
	return buf.Bytes(), nil
}

// runtimeMinutes 把 "166 min" / "2h 46min" / "166" 统一成分钟数。
// Emby 只认整数分钟，原样写 "166 min" 会被当成 0，界面上显示"-"。
func runtimeMinutes(s string) string {
	s = strings.TrimSpace(strings.ToLower(s))
	if s == "" {
		return ""
	}
	// 纯数字直接就是分钟
	if n, err := strconv.Atoi(s); err == nil && n > 0 {
		return strconv.Itoa(n)
	}
	total := 0
	found := false
	// 2h 46min / 2h46m / 166min
	if i := strings.Index(s, "h"); i > 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(s[:i])); err == nil {
			total += n * 60
			found = true
			s = s[i+1:]
		}
	}
	// 剩下的部分里找第一段数字
	num := ""
	for _, r := range s {
		if r >= '0' && r <= '9' {
			num += string(r)
			found = true
			continue
		}
		if num != "" {
			break
		}
	}
	if num != "" {
		if n, err := strconv.Atoi(num); err == nil {
			total += n
		}
	}
	if !found || total <= 0 {
		return ""
	}
	return strconv.Itoa(total)
}

func cleanList(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// toActors 生成 nfo 的演员表。
//
// 有 TMDB 的明细就写全（中文名 + 角色 + 头像），没有就退回只有名字的老格式。
// 老格式不是「兼容」而是「降级」：没配 TMDB Key 时 Emby 里仍然没有头像，
// 但至少标题、简介、海报是齐的，不该因为缺一个 Key 就整部片刮不出来。
func toActors(m Meta) []nfoActor {
	if len(m.CastDetail) > 0 {
		out := make([]nfoActor, 0, len(m.CastDetail))
		for _, p := range m.CastDetail {
			name := strings.TrimSpace(p.Name)
			if name == "" {
				continue
			}
			out = append(out, nfoActor{
				Name:  name,
				Role:  strings.TrimSpace(p.Role),
				Order: len(out),
				Thumb: strings.TrimSpace(p.Image),
			})
		}
		if len(out) > 0 {
			return out
		}
	}
	names := cleanList(m.Cast)
	out := make([]nfoActor, 0, len(names))
	for i, n := range names {
		out = append(out, nfoActor{Name: n, Order: i})
	}
	return out
}
