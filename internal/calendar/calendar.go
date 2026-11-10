// Package calendar 把 TMDB 的在播剧单 + 分集播出日期，和「时刻增强源」的
// 播出时刻 / 电视网合成一张「七列周历」（截图里 Trakt Calendar 那种版式）。
//
// 剧单和日期永远以 TMDB 为主：/tv/on_the_air 拿在播剧、/tv/{id}/season/{n}
// 拿每集 air_date、集标题、剧照、/discover/movie 拿电影发行日，
// /tv/{id}/external_ids 给 tvdb_id 和 imdb_id 用于对齐。
//
// 时刻增强是可插拔的，由设置项 calendar_source 决定：
//   - tvmaze（默认）：TVMaze 公共 API 免 Key，/schedule 直接给「哪天几点在哪台
//     播哪一集」。TheTVDB v4 已不再向个人发免费项目 Key，所以默认换成它。
//   - tvdb：原 v4 路径，留给已经拿到项目 Key 的用户。
//   - off：只显示 TMDB 的日期，不补时刻。
//
// 任何增强源失败都只写 Notice，不把整张日历弄没。
// 结果按「起始日期」整周缓存到 meta_cache，本周 30 分钟、历史/未来周 12 小时；
// refresh=1 才强制重抓。
package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"strm115/internal/douban"
	"strm115/internal/store"
	"strm115/internal/tmdb"
	"strm115/internal/tvdb"
	"strm115/internal/tvmaze"
)

const (
	// maxShows 控制逐剧请求的量。前两页在播剧 + 今天播出，去重后按热度取头部，
	// 再多的长尾剧对「这周看什么」没有帮助，只会把冷启动拖到几十秒。
	maxShows      = 48
	maxTVDB       = 24 // TVDB 增强的剧数上限（没配 Key 时直接跳过）
	detailWorkers = 8
	seasonWorkers = 6
	tvdbWorkers   = 4
	currentTTL    = 30 * time.Minute
	archiveTTL    = 12 * time.Hour
	// doubanWeekTimeout 是豆瓣 4 个榜单共享的短超时：豆瓣挂住时不能让整段
	// 无限等。它和 /api/calendar/douban handler 的 5s 上限是内外两道闸门，
	// 4 个请求共用这一个 ctx，谁先到点就一起收尾（见 doubanWeek）。
	doubanWeekTimeout = 4500 * time.Millisecond
)

// Entry 是周历上的一张卡。剧集和电影共用结构，用 Kind 区分。
type Entry struct {
	Key          string  `json:"key"`
	Kind         string  `json:"kind"` // episode / movie
	Date         string  `json:"date"`
	TMDbID       int     `json:"tmdb_id,omitempty"`
	TVDBID       int     `json:"tvdb_id,omitempty"`
	IMDbID       string  `json:"imdb_id,omitempty"`
	Title        string  `json:"title"`
	Original     string  `json:"original,omitempty"`
	Poster       string  `json:"poster,omitempty"`
	Backdrop     string  `json:"backdrop,omitempty"`
	Season       int     `json:"season,omitempty"`
	Episode      int     `json:"episode,omitempty"`
	Code         string  `json:"code,omitempty"` // S02E05
	EpisodeTitle string  `json:"episode_title,omitempty"`
	AirTime      string  `json:"air_time,omitempty"` // 20:00
	Network      string  `json:"network,omitempty"`
	Runtime      int     `json:"runtime,omitempty"`
	Rating       float64 `json:"rating,omitempty"`
	Popularity   float64 `json:"popularity,omitempty"`
	Overview     string  `json:"overview,omitempty"`
	Subscribed   bool    `json:"subscribed,omitempty"`
	Premiere     bool    `json:"premiere,omitempty"` // 本季第一集
	NewShow      bool    `json:"new_show,omitempty"` // 第一季第一集 / 本周开播
	Source       string  `json:"source,omitempty"`   // tmdb / douban
	PageURL      string  `json:"page_url,omitempty"` // TMDB / 豆瓣详情页
	// DoubanID 只在 Source=douban 时有：这类条目在 TMDB 上可能根本没有记录，
	// 界面不能拿它去开 TMDB 详情，只能给「豆瓣 ↗」或按片名去 TMDB 找一次。
	DoubanID int `json:"douban_id,omitempty"`
	// EpisodesInfo 是豆瓣原话「更新至28集」，国产剧不在周历上按天排，
	// 靠它告诉用户更到哪了。
	EpisodesInfo string `json:"episodes_info,omitempty"`
}

// Day 是一列。Weekday / Label 由后端算好，前端不用引日期库。
type Day struct {
	Date    string  `json:"date"`
	Weekday string  `json:"weekday"`
	Label   string  `json:"label"`
	Items   []Entry `json:"items"`
}

// Sources 让界面如实说明「这张表是谁给的」，而不是假装数据齐全。
// Attribution 是 TVMaze 的 CC BY-SA 署名，前端要渲染成可点链接。
type Sources struct {
	TMDB           bool   `json:"tmdb"`
	Source         string `json:"source,omitempty"` // tvmaze / tvdb / off
	TVMaze         bool   `json:"tvmaze"`
	TVMazeError    string `json:"tvmaze_error,omitempty"`
	TVDB           bool   `json:"tvdb"`
	TVDBConfigured bool   `json:"tvdb_configured"`
	TVDBError      string `json:"tvdb_error,omitempty"`
	Attribution    string `json:"attribution,omitempty"`
	AttributionURL string `json:"attribution_url,omitempty"`
	Shows          int    `json:"shows"`
}

// DoubanBlock 是豆瓣那一半的数据。
//
// 为什么国产剧不按天排：豆瓣只给「更新至第 N 集」，没有每一集的播出日期
// （TMDB 对国产剧也常常缺）。硬按天摊开等于编日期，用户按日期找片会扑空。
// 所以剧集放在单独的列表里，电影（有明确上映日）才进周历格子。
type DoubanBlock struct {
	// Enabled 说明这一轮到底有没有去问豆瓣（设置页关了就是 false）。
	// 没有它的话，前端分不清「关了」和「问了但一条都没匹配上」。
	Enabled bool          `json:"enabled"`
	OK      bool          `json:"ok"`
	Error   string        `json:"error,omitempty"`
	TV      []douban.Item `json:"tv,omitempty"` // 国产剧 / 港台剧：更新中
}

// Week 是一次完整的七日结果，整段 JSON 进缓存。
type Week struct {
	Start    string      `json:"start"`
	End      string      `json:"end"`
	Days     []Day       `json:"days"`
	Total    int         `json:"total"`
	Cached   bool        `json:"cached"`
	CachedAt string      `json:"cached_at,omitempty"`
	Sources  Sources     `json:"sources"`
	Douban   DoubanBlock `json:"douban"`
	Notice   string      `json:"notice,omitempty"`
}

type Deps struct {
	Store *store.Store
	// TMDb 每轮取一次 Client：上游反代是进程级配置，设置页改完下次就生效。
	TMDb func() *tmdb.Client
	// Credentials 返回当前 TVDB Key / PIN；Service 按值缓存 Client，
	// 用户改配置会自动重建（Token 也跟着重换）。
	Credentials func() (apiKey, pin string)
	// TVDBBase 可选：留空用官方 v4 地址；测试指到本地假服务器。
	TVDBBase string
	// TVDBBaseFunc 每轮现读设置里的 tvdb_base，改完不用重启容器就生效。
	// 优先于静态的 TVDBBase；两个都为空时 tvdb.New 用官方默认地址。
	TVDBBaseFunc func() string
	// SourceFunc 每轮现读 calendar_source：tvmaze / tvdb / off。
	// 不传时默认 tvmaze —— TheTVDB 已不再向个人发免费 v4 Key。
	SourceFunc func() string
	// TVMazeBaseFunc 每轮现读 tvmaze_base（可选反代根），留空用官方地址。
	TVMazeBaseFunc func() string
	// NetProxy 让 TVMaze 跟着全局代理走，直连不通的环境不用单独配。
	NetProxy func() string
	// Douban 返回豆瓣客户端；返回 nil 表示没启用（设置页关掉了，或没接）。
	// 每轮重新取：用户在设置页改完开关 / 反代地址，下一次刷日历就生效。
	Douban func() *douban.Client
	Log    *slog.Logger
	Now    func() time.Time
}

type Service struct {
	d      Deps
	mu     sync.Mutex
	mem    map[string]Week
	tvMu   sync.Mutex
	tv     *tvdb.Client
	tvKey  string
	tvPin  string
	tvBase string
	zMu    sync.Mutex
	z      *tvmaze.Client
	zBase  string
	zProxy string
}

// New 构造聚合服务。依赖缺失时不 panic，Week 会返回明确错误。
func New(d Deps) *Service {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{d: d, mem: map[string]Week{}}
}

// ParseStart 解析周起始日：留空 = 本周一，否则必须是 YYYY-MM-DD。
// 不强行把用户给的日期折到周一 —— 截图那张是「week of March 4」（周三起），
// 允许任意起点 7 天，前端翻周就是 ±7 天。
func ParseStart(raw string, now time.Time) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		d := now.In(time.Local)
		day := int(d.Weekday())
		if day == 0 {
			day = 7
		}
		return time.Date(d.Year(), d.Month(), d.Day()-day+1, 0, 0, 0, 0, time.Local), nil
	}
	start, err := time.ParseInLocation("2006-01-02", raw, time.Local)
	if err != nil {
		return time.Time{}, errors.New("起始日期要像 2026-11-02")
	}
	if start.After(now.AddDate(2, 0, 0)) || start.Before(now.AddDate(-2, 0, 0)) {
		return time.Time{}, errors.New("只支持前后两年内的周")
	}
	return start, nil
}

// Week 返回一周数据。refresh 跳过缓存读，但结果仍写回缓存。
func (s *Service) Week(ctx context.Context, start time.Time, refresh bool) (*Week, error) {
	if s.d.Store == nil || s.d.TMDb == nil {
		return nil, errors.New("日历服务没初始化")
	}
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
	end := start.AddDate(0, 0, 6)
	key := "calendar:" + start.Format("2006-01-02")
	ttl := cacheTTL(start, end, s.d.Now())
	if !refresh {
		if w, ok := s.cached(ctx, key, ttl); ok {
			return w, nil
		}
	}
	w, err := s.build(ctx, start, end)
	if err != nil {
		return nil, err
	}
	w.Cached = false
	w.CachedAt = s.d.Now().Format(time.RFC3339)
	// 只在真的排出了条目时缓存。空结果几乎总是「抓到一半客户端把页面关了」：
	// ctx 取消后并发任务全跳过，剧单拉到了（Sources.Shows > 0）、条目却是空的；
	// build 里各增强源又都是「失败只记原因、不把日历弄没」，于是不报错地返回一份空周历。
	//
	// 这份空结果一旦被缓存就很顽固：saveCache 先写内存再写库，而 cached() 优先读内存，
	// 所以哪怕 DB 那次写入也因 ctx 取消失败，内存里仍然留着空白 —— 接下来 30 分钟
	// 所有人看到的都是空日历，「我的订阅」筛选自然也恒空。宁可下次重抓一次。
	if w.Total > 0 && ctx.Err() == nil {
		s.saveCache(ctx, key, *w)
	}
	return w, nil
}

func cacheTTL(start, end, now time.Time) time.Duration {
	if !now.Before(start) && !now.After(end.Add(24*time.Hour)) {
		return currentTTL
	}
	return archiveTTL
}

func (s *Service) cached(ctx context.Context, key string, ttl time.Duration) (*Week, bool) {
	now := s.d.Now()
	s.mu.Lock()
	w, ok := s.mem[key]
	s.mu.Unlock()
	if !ok {
		_, payload, found, err := s.d.Store.GetMetaCache(ctx, key)
		if err == nil && found && json.Unmarshal([]byte(payload), &w) == nil {
			ok = true
			s.mu.Lock()
			s.mem[key] = w
			s.mu.Unlock()
		}
	}
	if !ok || w.CachedAt == "" {
		return nil, false
	}
	at, err := time.Parse(time.RFC3339, w.CachedAt)
	if err != nil || now.Sub(at) >= ttl {
		return nil, false
	}
	out := w
	out.Cached = true
	return &out, true
}

func (s *Service) saveCache(ctx context.Context, key string, w Week) {
	s.mu.Lock()
	s.mem[key] = w
	s.mu.Unlock()
	if payload, err := json.Marshal(w); err == nil {
		if err := s.d.Store.PutMetaCache(ctx, key, "", string(payload)); err != nil {
			s.d.Log.Warn("播出日历写缓存失败", "err", err)
		}
	}
}

// tvdbClient 只在凭据变化时重建，否则进程内的 Bearer Token 每次都被丢掉，
// 等于每个请求都去 /login 一次。
func (s *Service) tvdbClient() *tvdb.Client {
	if s.d.Credentials == nil {
		return nil
	}
	key, pin := s.d.Credentials()
	base := s.tvdbBase()
	s.tvMu.Lock()
	defer s.tvMu.Unlock()
	// 接口根换了也要换 Client：Bearer Token 是那个域签发的，
	// 拿旧 Token 打新反代会一律 401。
	if s.tv == nil || s.tvKey != key || s.tvPin != pin || s.tvBase != base {
		c := tvdb.New(key, pin)
		if base != "" {
			c.Base = base
		}
		s.tv, s.tvKey, s.tvPin, s.tvBase = c, key, pin, base
	}
	return s.tv
}

// tvdbBase 返回这一轮该用的 v4 接口根，留空交给 tvdb.New 的官方默认值。
func (s *Service) tvdbBase() string {
	base := s.d.TVDBBase
	if s.d.TVDBBaseFunc != nil {
		if v := strings.TrimSpace(s.d.TVDBBaseFunc()); v != "" {
			base = v
		}
	}
	return strings.TrimRight(strings.TrimSpace(base), "/")
}

// Ping 给设置页的「测试 TVDB」按钮用。
func (s *Service) Ping(ctx context.Context) error {
	c := s.tvdbClient()
	if c == nil || !c.Configured() {
		return tvdb.ErrNoKey
	}
	return c.Ping(ctx)
}

// source 返回本轮生效的增强源。设置值不在白名单里一律按 tvmaze 处理：
// 手改数据库写错一个字母，不该让日历静默退化成「没有时刻」。
func (s *Service) source() string {
	if s.d.SourceFunc == nil {
		return "tvmaze"
	}
	switch v := strings.ToLower(strings.TrimSpace(s.d.SourceFunc())); v {
	case "tvmaze", "tvdb", "off":
		return v
	default:
		return "tvmaze"
	}
}

func (s *Service) tvmazeBase() string {
	if s.d.TVMazeBaseFunc != nil {
		return strings.TrimRight(strings.TrimSpace(s.d.TVMazeBaseFunc()), "/")
	}
	return ""
}

// tvmazeClient 按「反代根 + 代理」缓存 Client：按天缓存挂在 Client 上，
// 每轮重建会把刚抓的排期全丢掉，白白多打十几个请求。
func (s *Service) tvmazeClient() *tvmaze.Client {
	base, proxy := s.tvmazeBase(), ""
	if s.d.NetProxy != nil {
		proxy = strings.TrimSpace(s.d.NetProxy())
	}
	s.zMu.Lock()
	defer s.zMu.Unlock()
	if s.z == nil || s.zBase != base || s.zProxy != proxy {
		s.z, s.zBase, s.zProxy = tvmaze.New(base, proxy), base, proxy
	}
	return s.z
}

// PingTVMaze 给设置页的「测试 TVMaze」按钮用，返回当日排期条数。
func (s *Service) PingTVMaze(ctx context.Context) (int, error) {
	return s.tvmazeClient().Ping(ctx)
}

type showWork struct {
	show tmdb.AiringShow
	det  *tmdb.ShowDetail
}

// build 是真正的聚合：剧单 → 详情 → 分季 → 电影 → 订阅 → TVDB 增强 → 按天分组。
func (s *Service) build(ctx context.Context, start, end time.Time) (*Week, error) {
	client := s.d.TMDb()
	if client == nil {
		return nil, errors.New("没有可用的 TMDB 客户端")
	}
	from, to := start.Format("2006-01-02"), end.Format("2006-01-02")

	// 1) 剧单：三页并发，去重后按热度取头部。
	type listJob struct {
		list string
		page int
	}
	jobs := []listJob{{"on_the_air", 1}, {"on_the_air", 2}, {"airing_today", 1}}
	lists := make([][]tmdb.AiringShow, len(jobs))
	listOK := make([]bool, len(jobs))
	parallel(ctx, len(jobs), 3, func(_ context.Context, i int) {
		j := jobs[i]
		got, err := client.Airing(ctx, j.list, j.page)
		if err != nil {
			// 单页失败不致命，还有另外两页；全失败才报错。
			s.d.Log.Warn("在播剧单拉取失败", "list", j.list, "page", j.page, "err", err)
			return
		}
		lists[i] = got
		listOK[i] = true
	})
	anyList := false
	for _, ok := range listOK {
		anyList = anyList || ok
	}
	if !anyList {
		return nil, errors.New("TMDB 在播剧单一条都没拉到，检查网络或 TMDB 反代配置")
	}
	shows := map[int]tmdb.AiringShow{}
	for _, l := range lists {
		for _, sh := range l {
			shows[sh.ID] = sh
		}
	}
	uniq := make([]tmdb.AiringShow, 0, len(shows))
	for _, sh := range shows {
		uniq = append(uniq, sh)
	}
	sort.Slice(uniq, func(i, j int) bool { return uniq[i].Popularity > uniq[j].Popularity })
	if len(uniq) > maxShows {
		uniq = uniq[:maxShows]
	}

	// 2) 逐剧详情，挑出「这周可能有集」的季。
	works := make([]showWork, len(uniq))
	for i, sh := range uniq {
		works[i].show = sh
	}
	parallel(ctx, len(uniq), detailWorkers, func(_ context.Context, i int) {
		det, err := client.ShowDetail(ctx, uniq[i].ID)
		if err != nil {
			s.d.Log.Warn("剧集详情失败", "id", uniq[i].ID, "err", err)
			return
		}
		works[i].det = det
	})

	type seasonJob struct {
		idx    int
		season int
	}
	seasonJobs := []seasonJob{}
	for i, w := range works {
		if w.det == nil {
			continue
		}
		for _, n := range candidateSeasons(w.det, from, to) {
			seasonJobs = append(seasonJobs, seasonJob{idx: i, season: n})
		}
	}
	type episodeResult struct {
		idx int
		eps []tmdb.SeasonEpisode
	}
	results := make([]episodeResult, len(seasonJobs))
	parallel(ctx, len(seasonJobs), seasonWorkers, func(_ context.Context, i int) {
		j := seasonJobs[i]
		w := works[j.idx]
		if w.det == nil {
			return
		}
		eps, err := client.SeasonEpisodes(ctx, w.det.ID, j.season)
		if err != nil {
			s.d.Log.Warn("分集拉取失败", "id", w.det.ID, "season", j.season, "err", err)
			return
		}
		results[i] = episodeResult{idx: j.idx, eps: eps}
	})

	// 3) 订阅标记：只在展示层打星，不改订阅逻辑。
	subscribed := map[int]bool{}
	if subs, err := s.d.Store.ListSubscriptions(ctx, ""); err == nil {
		for _, sub := range subs {
			if sub.TMDbID > 0 {
				subscribed[sub.TMDbID] = true
			}
		}
	} else {
		s.d.Log.Warn("读取订阅清单失败", "err", err)
	}

	entries := []Entry{}
	for _, r := range results {
		w := works[r.idx]
		if w.det == nil || len(r.eps) == 0 {
			continue
		}
		for _, ep := range r.eps {
			if !within(ep.AirDate, from, to) {
				continue
			}
			entries = append(entries, Entry{
				Key:          fmt.Sprintf("ep:%d:%s:%d:%d", w.det.ID, ep.AirDate, ep.Season, ep.Episode),
				Kind:         "episode",
				Date:         ep.AirDate,
				TMDbID:       w.det.ID,
				TVDBID:       w.det.TVDBID,
				IMDbID:       w.det.IMDb,
				Title:        firstNonEmpty(w.show.Name, w.det.Name),
				Original:     firstNonEmpty(w.show.Original, w.det.Original),
				Poster:       firstNonEmpty(w.show.Poster, w.det.Poster),
				Backdrop:     firstNonEmpty(w.show.Backdrop, w.det.Backdrop),
				Season:       ep.Season,
				Episode:      ep.Episode,
				Code:         fmt.Sprintf("S%02dE%02d", ep.Season, ep.Episode),
				EpisodeTitle: ep.Name,
				Network:      w.det.Network(),
				Runtime:      firstPositive(ep.Runtime, w.det.Runtime),
				Rating:       ep.Rating,
				Popularity:   w.show.Popularity,
				Overview:     firstNonEmpty(ep.Overview, w.show.Overview),
				Subscribed:   subscribed[w.det.ID],
				Premiere:     ep.Episode == 1,
				NewShow:      ep.Season == 1 && ep.Episode == 1,
				Source:       "tmdb",
				PageURL:      tmdb.PageURL(tmdb.KindTV, w.det.ID),
			})
		}
	}

	// 4) 电影：TMDB 有发行日期区间，一个请求就够。
	if movies, err := client.MovieReleases(ctx, from, to, 1); err == nil {
		for _, m := range movies {
			if !within(m.ReleaseDate, from, to) {
				continue
			}
			entries = append(entries, Entry{
				Key:        fmt.Sprintf("movie:%d:%s", m.ID, m.ReleaseDate),
				Kind:       "movie",
				Date:       m.ReleaseDate,
				TMDbID:     m.ID,
				Title:      m.Title,
				Original:   m.Original,
				Poster:     m.Poster,
				Backdrop:   m.Backdrop,
				Rating:     m.Rating,
				Popularity: m.Popularity,
				Overview:   m.Overview,
				// 电影不是「新剧」：以前这里也标了 NewShow，导致「新剧」筛选
				// 里混进一堆电影，和「电影」筛选看起来一模一样。
				Source:  "tmdb",
				PageURL: tmdb.PageURL(tmdb.KindMovie, m.ID),
			})
		}
	} else {
		s.d.Log.Warn("本周电影拉取失败", "err", err)
	}

	// 5) 时刻增强：tvmaze / tvdb / off 三选一。失败只记录原因，不把日历弄没。
	sources := Sources{TMDB: true, Shows: len(uniq), Source: s.source()}
	switch sources.Source {
	case "tvmaze":
		entries, sources.TVMaze, sources.TVMazeError =
			s.enrichTVMaze(ctx, s.tvmazeClient(), entries, from, to)
		if sources.TVMaze {
			// CC BY-SA：只要用上了数据，就要在界面上给出署名与链接。
			sources.Attribution = tvmaze.Attribution
			sources.AttributionURL = tvmaze.AttributionURL
		}
	case "tvdb":
		if tv := s.tvdbClient(); tv != nil && tv.Configured() {
			sources.TVDBConfigured = true
			entries, sources.TVDB, sources.TVDBError =
				s.enrichTVDB(ctx, tv, works, entries, from, to)
		}
	}

	// 5.5) 豆瓣不再并进这里。
	//
	// 为什么拆出去：豆瓣 4 个榜单串行 + minInterval 节流 + 最长 12s 超时，
	// 慢的时候十几秒；以前同步塞在 build() 末尾，整张 /api/calendar 要等它
	// 全跑完才返回，TMDB 明明早就好了也被拖住。现在豆瓣改由独立接口
	// /api/calendar/douban 异步提供（见 DoubanWeek），前端先渲染 TMDB 主体，
	// 豆瓣慢/挂只影响「国产剧」那一栏，不再拖累主响应。
	//
	// 6) 按天分组。
	w := &Week{
		Start: from,
		End:   to,
		Days:  make([]Day, 7),
		Total: len(entries),
	}
	weekdays := [...]string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	for i := 0; i < 7; i++ {
		d := start.AddDate(0, 0, i)
		w.Days[i] = Day{
			Date:    d.Format("2006-01-02"),
			Weekday: weekdays[int(d.Weekday())],
			Label:   fmt.Sprintf("%d月%d日", int(d.Month()), d.Day()),
			Items:   []Entry{},
		}
	}
	byDate := map[string]int{}
	for i := range w.Days {
		byDate[w.Days[i].Date] = i
	}
	for _, e := range entries {
		if i, ok := byDate[e.Date]; ok {
			w.Days[i].Items = append(w.Days[i].Items, e)
		}
	}
	for i := range w.Days {
		sort.SliceStable(w.Days[i].Items, func(a, b int) bool {
			x, y := w.Days[i].Items[a], w.Days[i].Items[b]
			if x.AirTime != y.AirTime {
				// 有具体时刻的排前面；都没时刻的比热度。
				if x.AirTime == "" || y.AirTime == "" {
					return x.AirTime != ""
				}
				return x.AirTime < y.AirTime
			}
			return x.Popularity > y.Popularity
		})
	}
	switch {
	case sources.Source == "off":
		w.Notice = "已关闭时刻增强：只显示 TMDB 的播出日期与集标题"
	case sources.Source == "tvdb" && !sources.TVDBConfigured:
		w.Notice = "未配置 TVDB：播出时刻缺失，可在「系统配置」换用免 Key 的 TVMaze"
	case sources.Source == "tvdb" && sources.TVDBError != "":
		w.Notice = "TVDB 增强未完成：" + sources.TVDBError + "（日历主体仍来自 TMDB）"
	case sources.Source == "tvmaze" && sources.TVMazeError != "":
		w.Notice = "TVMaze 增强未完成：" + sources.TVMazeError + "（日历主体仍来自 TMDB）"
	case sources.Source == "tvmaze" && !sources.TVMaze:
		w.Notice = "TVMaze 这周没匹配到播出时刻（日历主体仍来自 TMDB）"
	}
	w.Sources = sources
	// Week.Douban 只留一个 enabled=false 的占位：字段形状保留，前端老代码读
	// week.douban 不会崩；真正的数据由 /api/calendar/douban 返回后由前端覆盖。
	// 千万别在这里填 Enabled=true —— 那样前端会误以为「问过了」，而这一轮
	// 根本没碰豆瓣。
	w.Douban = DoubanBlock{Enabled: false}
	return w, nil
}

// DoubanWeek 单独返回某一周的豆瓣数据：国产剧列表（DoubanBlock.TV）与
// 有明确上映日的国内电影（日历 Entry 切片，前端按 date 并进周历格子）。
//
// 为什么做成公开方法：/api/calendar 主体要尽快返回，豆瓣拆到独立接口后由
// handler 调这里；这样 build() 不再等豆瓣，而豆瓣的失败/超时也彻底和 TMDB
// 那一半解耦。测试直接钉这个方法，覆盖「电影进日历 / 国产剧进度 / 降级」。
func (s *Service) DoubanWeek(ctx context.Context, start time.Time) (DoubanBlock, []Entry) {
	// 和 Week() 一样按「起始日 + 6 天」算窗口，前后端用同一套日期边界。
	from := start.Format("2006-01-02")
	to := start.AddDate(0, 0, 6).Format("2006-01-02")
	return s.doubanWeek(ctx, from, to)
}

// doubanWeek 拉豆瓣的「即将上映 / 正在上映」与「国产剧」，把有明确上映日的
// 电影并进周历，剧集整块交给前端单列。
//
// 任何一个子请求失败都只写 Error，不影响 TMDB 那一半：豆瓣被墙、限流、
// 改了页面结构，都不该让整张日历变空。
func (s *Service) doubanWeek(ctx context.Context, from, to string) (DoubanBlock, []Entry) {
	block := DoubanBlock{}
	if s.d.Douban == nil {
		return block, nil
	}
	c := s.d.Douban()
	if c == nil {
		return block, nil
	}
	block.Enabled = true
	// 4 个榜单保持顺序、共用这一个短超时 ctx：internal/douban 客户端内部有
	// minInterval=200ms 的串行节流，真并发只会把 418 风控打出来（之后连正常
	// 请求也进不来）。顺序跑 + 共享 ctx，既避开风控，又保证任一请求挂住时
	// 整段在 doubanWeekTimeout 内一起收尾，不会 4×12s 地拖下去。
	ctx, cancel := context.WithTimeout(ctx, doubanWeekTimeout)
	defer cancel()
	var errs []string
	var entries []Entry
	seen := map[string]bool{}

	// 电影：即将上映（有档期）+ 正在上映（热映，档期可能已经过了本周）。
	for _, key := range []string{douban.CollMovieSoon, douban.CollMovieShowing} {
		coll, err := c.Collection(ctx, key, 0, douban.MaxPageSize)
		if err != nil {
			errs = append(errs, key+"："+err.Error())
			continue
		}
		for _, it := range coll.Items {
			if it.ReleaseDate == "" || it.ReleaseDate < from || it.ReleaseDate > to {
				continue
			}
			if seen[it.Title] {
				continue
			}
			seen[it.Title] = true
			block.OK = true
			// 豆瓣电影绝不能标 NewShow / Premiere：这两个字段只给「第一季
			// 第一集」的剧集用。以前 TMDB 电影被误标成新剧，导致「新剧」筛选
			// 和「电影」筛选看起来一模一样（Bug 2）。这里整条路径都不写它们，
			// 默认零值 false，并由 TestDoubanMoviesAreNotNewShow 钉住。
			entries = append(entries, Entry{
				Key:        fmt.Sprintf("dbmovie:%d:%s", it.ID, it.ReleaseDate),
				Kind:       "movie",
				Date:       it.ReleaseDate,
				Title:      it.Title,
				Original:   it.Original,
				Poster:     it.Poster,
				Rating:     it.Rating,
				Overview:   it.Intro,
				Source:     "douban",
				DoubanID:   it.ID,
				PageURL:    it.URL,
				Popularity: it.Rating, // 没有热度字段，用评分顶着排序
			})
		}
	}

	// 剧集：国产剧 + 热播剧里的国别是中国的那些。豆瓣不给逐集日期，
	// 所以只取「更新中」的（EpisodesInfo 非空），整块交给前端。
	for _, key := range []string{douban.CollTVDomestic, douban.CollTVHot} {
		coll, err := c.Collection(ctx, key, 0, douban.MaxPageSize)
		if err != nil {
			errs = append(errs, key+"："+err.Error())
			continue
		}
		for _, it := range coll.Items {
			if it.Kind != "tv" || it.EpisodesInfo == "" {
				continue
			}
			if !strings.Contains(it.CardSubtitle, "中国大陆") &&
				!strings.Contains(it.CardSubtitle, "中国香港") &&
				!strings.Contains(it.CardSubtitle, "中国台湾") &&
				key == douban.CollTVHot {
				continue // 热播榜里混着美剧日剧，那个 TMDB 已经覆盖了
			}
			block.OK = true
			block.TV = append(block.TV, it)
		}
	}
	block.Error = strings.Join(errs, "；")
	return block, entries
}

// enrichTVMaze 用整周排期给 TMDB 条目补「几点播 / 哪个台 / 集标题」。
//
// 只补字段、不新增条目：日历卡片由 TMDB 的在播剧单决定，TVMaze 里还有大量
// 综艺、日播和没有 TMDB ID 的节目，塞进来只会多出一堆对不上的卡。
// 对齐按 tvdb_id → imdb_id → 剧名+S/E 三级，命中任意一级就停；
// 只填空字段，TMDB 已有的中文集标题和电视网优先。
func (s *Service) enrichTVMaze(ctx context.Context, c *tvmaze.Client,
	entries []Entry, from, to string) ([]Entry, bool, string) {

	dates, err := weekDates(from, to)
	if err != nil || len(dates) == 0 {
		return entries, false, "日历日期区间不合法"
	}
	eps, weekErr := c.Week(ctx, dates, "US")
	errText := ""
	if weekErr != nil {
		errText = weekErr.Error()
	}
	if len(eps) == 0 {
		if errText == "" {
			errText = "整周排期为空"
		}
		return entries, false, errText
	}

	byTVDB := map[string]tvmaze.Episode{}
	byIMDb := map[string]tvmaze.Episode{}
	byName := map[string]tvmaze.Episode{}
	for _, e := range eps {
		span := fmt.Sprintf("%d/%d", e.Season, e.Number)
		if e.Season <= 0 || e.Number <= 0 {
			continue
		}
		if e.TVDBID > 0 {
			k := fmt.Sprintf("%d/%s", e.TVDBID, span)
			if _, ok := byTVDB[k]; !ok {
				byTVDB[k] = e
			}
		}
		if e.IMDbID != "" {
			k := e.IMDbID + "/" + span
			if _, ok := byIMDb[k]; !ok {
				byIMDb[k] = e
			}
		}
		if e.ShowKey != "" {
			k := e.ShowKey + "/" + span
			if _, ok := byName[k]; !ok {
				byName[k] = e
			}
		}
	}

	used := false
	for i := range entries {
		e := &entries[i]
		if e.Kind != "episode" {
			continue
		}
		span := fmt.Sprintf("%d/%d", e.Season, e.Episode)
		hit, found := tvmaze.Episode{}, false
		if e.TVDBID > 0 {
			hit, found = byTVDB[fmt.Sprintf("%d/%s", e.TVDBID, span)]
		}
		if !found && e.IMDbID != "" {
			hit, found = byIMDb[e.IMDbID+"/"+span]
		}
		if !found {
			for _, name := range []string{e.Original, e.Title} {
				if k := tvmaze.NormalizeShow(name); k != "" {
					if h, ok := byName[k+"/"+span]; ok {
						hit, found = h, true
						break
					}
				}
			}
		}
		if !found {
			continue
		}
		if e.AirTime == "" && hit.AirTime != "" {
			e.AirTime = hit.AirTime
			used = true
		}
		if e.Network == "" && hit.Channel != "" {
			e.Network = hit.Channel
			used = true
		}
		if e.EpisodeTitle == "" && hit.Title != "" {
			e.EpisodeTitle = hit.Title
			used = true
		}
		if e.Runtime == 0 && hit.Runtime > 0 {
			e.Runtime = hit.Runtime
			used = true
		}
	}
	return entries, used, errText
}

// weekDates 展开 from..to（含两端）成 YYYY-MM-DD，封顶 7 天。
func weekDates(from, to string) ([]string, error) {
	a, err := time.ParseInLocation("2006-01-02", from, time.Local)
	if err != nil {
		return nil, err
	}
	b, err := time.ParseInLocation("2006-01-02", to, time.Local)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for d := a; !d.After(b) && len(out) < 7; d = d.AddDate(0, 0, 1) {
		out = append(out, d.Format("2006-01-02"))
	}
	return out, nil
}

// enrichTVDB 给命中的剧补时刻/电视网，并把 TVDB 独有的集补进来。
func (s *Service) enrichTVDB(ctx context.Context, c *tvdb.Client, works []showWork,
	entries []Entry, from, to string) ([]Entry, bool, string) {

	// 只查日历里真正出现过的剧，且按 tvdb_id 去重。
	seen := map[int]bool{}
	byID := map[int]showWork{}
	ids := []int{}
	for _, e := range entries {
		if e.Kind != "episode" || e.TVDBID <= 0 || seen[e.TVDBID] {
			continue
		}
		seen[e.TVDBID] = true
		ids = append(ids, e.TVDBID)
		if len(ids) >= maxTVDB {
			break
		}
	}
	for _, w := range works {
		if w.det != nil && seen[w.det.TVDBID] {
			byID[w.det.TVDBID] = w
		}
	}
	if len(ids) == 0 {
		return entries, false, ""
	}

	series := make([]*tvdb.Series, len(ids))
	var failMu sync.Mutex
	firstErr := ""
	var used flag
	parallel(ctx, len(ids), tvdbWorkers, func(_ context.Context, i int) {
		got, err := c.SeriesExtended(ctx, ids[i])
		if err != nil {
			failMu.Lock()
			if firstErr == "" {
				firstErr = err.Error()
			}
			failMu.Unlock()
			return
		}
		series[i] = got
		used.set()
	})
	if !used.ok() {
		return entries, false, firstErr
	}

	// 现有 (剧, 季, 集) 索引：TMDB 已有的不重复加，只补字段。
	index := map[string]bool{}
	for _, e := range entries {
		if e.Kind == "episode" && e.TVDBID > 0 {
			index[fmt.Sprintf("%d/%d/%d", e.TVDBID, e.Season, e.Episode)] = true
		}
	}
	for i, got := range series {
		if got == nil {
			continue
		}
		id := ids[i]
		w := byID[id]
		// range 用的是进入循环时的长度：本轮新 append 的 TVDB 条目不会再被补一遍。
		for j := range entries {
			if entries[j].TVDBID != id {
				continue
			}
			if entries[j].AirTime == "" {
				entries[j].AirTime = got.AirTime()
			}
			if entries[j].Network == "" {
				entries[j].Network = got.Network()
			}
			if entries[j].EpisodeTitle == "" {
				for _, ep := range got.Episodes {
					if ep.SeasonNumber == entries[j].Season && ep.EpisodeNumber == entries[j].Episode {
						entries[j].EpisodeTitle = ep.Name
						break
					}
				}
			}
			if entries[j].Runtime == 0 {
				for _, ep := range got.Episodes {
					if ep.SeasonNumber == entries[j].Season && ep.EpisodeNumber == entries[j].Episode && ep.Runtime > 0 {
						entries[j].Runtime = ep.Runtime
						break
					}
				}
			}
		}
		if w.det == nil {
			continue
		}
		for _, ep := range got.Episodes {
			date := ep.Date()
			if !within(date, from, to) || ep.SeasonNumber <= 0 || ep.EpisodeNumber <= 0 {
				continue
			}
			k := fmt.Sprintf("%d/%d/%d", id, ep.SeasonNumber, ep.EpisodeNumber)
			if index[k] {
				continue // TMDB 已经有这一集，中文集标题优先
			}
			index[k] = true
			entries = append(entries, Entry{
				Key:          fmt.Sprintf("ep:%d:%s:%d:%d:tvdb", w.det.ID, date, ep.SeasonNumber, ep.EpisodeNumber),
				Kind:         "episode",
				Date:         date,
				TMDbID:       w.det.ID,
				TVDBID:       id,
				Title:        firstNonEmpty(w.show.Name, w.det.Name, got.Name),
				Original:     firstNonEmpty(w.show.Original, w.det.Original),
				Poster:       firstNonEmpty(w.show.Poster, w.det.Poster),
				Backdrop:     firstNonEmpty(w.show.Backdrop, w.det.Backdrop, got.Image),
				Season:       ep.SeasonNumber,
				Episode:      ep.EpisodeNumber,
				Code:         fmt.Sprintf("S%02dE%02d", ep.SeasonNumber, ep.EpisodeNumber),
				EpisodeTitle: ep.Name,
				AirTime:      got.AirTime(),
				Network:      got.Network(),
				Runtime:      ep.Runtime,
				Popularity:   w.show.Popularity,
				Overview:     firstNonEmpty(ep.Overview, w.show.Overview),
				Premiere:     ep.EpisodeNumber == 1,
				NewShow:      ep.SeasonNumber == 1 && ep.EpisodeNumber == 1,
				Source:       "tvdb",
				PageURL:      tmdb.PageURL(tmdb.KindTV, w.det.ID),
			})
		}
	}
	return entries, true, firstErr
}

// candidateSeasons 决定一部剧要查哪几季，避免对每部剧把所有季都翻一遍。
//
// 命中条件：最新/下一集的播出日在这周内，或某一季的首播日在这周内
// （新剧 / 新季），再兜一个 last_air_date 落周内但 last/next 都缺的情况。
func candidateSeasons(d *tmdb.ShowDetail, from, to string) []int {
	set := map[int]bool{}
	if d.LastAired != nil && within(d.LastAired.AirDate, from, to) {
		set[d.LastAired.Season] = true
	}
	if d.NextAiring != nil && within(d.NextAiring.AirDate, from, to) {
		set[d.NextAiring.Season] = true
	}
	for _, se := range d.Seasons {
		if within(se.AirDate, from, to) {
			set[se.Season] = true
		}
	}
	if len(set) == 0 && within(d.LastAirDate, from, to) && len(d.Seasons) > 0 {
		set[d.Seasons[len(d.Seasons)-1].Season] = true
	}
	out := []int{}
	for n := range set {
		if n > 0 {
			out = append(out, n)
		}
	}
	sort.Ints(out)
	if len(out) > 2 {
		out = out[len(out)-2:]
	}
	return out
}

func within(date, from, to string) bool {
	if len(date) < 10 {
		return false
	}
	d := date[:10]
	return d >= from && d <= to
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

func firstPositive(v ...int) int {
	for _, n := range v {
		if n > 0 {
			return n
		}
	}
	return 0
}

// parallel 限并发跑 n 个任务，回调拿到索引。
// 结果切片各写各的下标，不用加锁，也不会出现「按值反查索引」的歧义。
func parallel(ctx context.Context, n, workers int, fn func(context.Context, int)) {
	if workers <= 0 {
		workers = 1
	}
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			if ctx.Err() == nil {
				fn(ctx, idx)
			}
		}(i)
	}
	wg.Wait()
}

// flag 汇总并发任务「至少成功一个」，比各 goroutine 直接改 bool 安全。
type flag struct {
	mu sync.Mutex
	v  bool
}

func (f *flag) set() {
	f.mu.Lock()
	f.v = true
	f.mu.Unlock()
}
func (f *flag) ok() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.v
}
