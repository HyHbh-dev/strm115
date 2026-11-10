package scrape

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/netproxy"
	"strm115/internal/store"
	"strm115/internal/strm"
	"strm115/internal/tmdb"
)

// Options 是一次刮削的运行参数。
type Options struct {
	// FetchMeta 为 false 时只解析文件名，不联网（调试/离线场景用）。
	FetchMeta bool
	// WriteNFO 是否生成 .nfo。
	WriteNFO bool
	// FetchImages 是否把海报/背景图下载到本地。
	//
	// 这是「避免风控」的关键一环：图片一旦落盘，Emby 刷新多少次都不会
	// 回头去请求 TMDB；没落盘的话每次刷库都会重新拉一遍。
	FetchImages bool
	// Overwrite 覆盖已存在的 nfo / 图片（默认不动，用户可能手改过）。
	Overwrite bool
	// RefreshCache 忽略本地 meta 缓存重新查 TMDB。
	RefreshCache bool
	// Rate 两次 TMDB 请求之间的最小间隔。
	Rate time.Duration
	// MinSize 小于这个体积的视频跳过（预告片、样片）。
	MinSize int64
	// ImageSize 图片下载上限，防止某个异常的巨图把磁盘吃掉。
	ImageSize int64
	// Language 元数据语言。"zh"（默认）表示中文优先：TMDB 用 zh-CN 取标题与简介；
	// "original" 表示保留原文（TMDB 的 original_title / 英文简介）。
	Language string
	// Translate 是否调用翻译接口。关掉后只有中文片名会生效，英文简介保持原样。
	Translate bool
	// APIBase / Model / Workers 是翻译接口的参数，与字幕翻译共用一套配置。
	APIBase string
	Model   string
	Workers int
}

// DefaultOptions 是推荐参数。Rate 特意保守：TMDB 公共配额按 IP 限流，
// 1.2 秒一次相当于每分钟 50 次，一个 500 部的库十几分钟能刮完，
// 又不会看起来像爬虫。
func DefaultOptions() Options {
	return Options{
		FetchMeta:   true,
		WriteNFO:    true,
		FetchImages: true,
		Rate:        1200 * time.Millisecond,
		MinSize:     50 << 20,
		ImageSize:   12 << 20,
		Language:    "zh",
		Translate:   true,
		Workers:     2,
	}
}

var ErrRunning = errors.New("这个库正在刮削")

// Progress 是刮削任务的实时状态。
type Progress struct {
	LibraryID  int64     `json:"library_id"`
	Phase      string    `json:"phase"` // parse / meta / write / subs / done / error / canceled
	Total      int       `json:"total"`
	Groups     int       `json:"groups"`
	Done       int       `json:"done"`
	Matched    int       `json:"matched"`
	Unmatched  int       `json:"unmatched"`
	Failed     int       `json:"failed"`
	Fetched    int       `json:"fetched"` // 实际请求 TMDB 的次数
	Cached     int       `json:"cached"`  // 命中本地缓存的次数
	Current    string    `json:"current"`
	Running    bool      `json:"running"`
	Message    string    `json:"message"`
	Err        string    `json:"err"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Job 是一次刮削任务。
type Job struct {
	mu   sync.Mutex
	p    Progress
	ctx  context.Context
	cnl  context.CancelFunc
	done chan struct{}
}

// Done 在任务结束时被关闭。轮询方可以用它代替 sleep 等待。
func (j *Job) Done() <-chan struct{} { return j.done }

func (j *Job) update(fn func(*Progress)) {
	j.mu.Lock()
	fn(&j.p)
	j.mu.Unlock()
}

func (j *Job) Snapshot() Progress {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p
}

func (j *Job) Running() bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p.Running
}

func (j *Job) Cancel() { j.cnl() }

// Service 负责刮削编排。
type Service struct {
	st  *store.Store
	log *slog.Logger
	// tmdbKey 从设置里读 TMDB Key；留空时 tmdbClient 读 STRM115_TMDB_KEY 兜底。
	// 纯 TMDB 之后刮削只有这一个上游，不再有 IMDb / Cinemeta 请求。
	tmdbKey func(ctx context.Context) string
	// mediaRoot 是媒体根目录。库的 local_path 不是绝对路径时（跨环境搬库），
	// 用它兜底拼出真实目录，见 ResolveRoot。
	mediaRoot func() string

	// TMDB 客户端按 Key 复用，见 tmdbClient。
	tmdbMu  sync.Mutex
	tmdbC   *tmdb.Client
	tmdbFor string

	mu   sync.Mutex
	jobs map[int64]*Job
}

func New(st *store.Store, tmdbKey func(context.Context) string, mediaRoot func() string, log *slog.Logger) *Service {
	if log == nil {
		log = slog.Default()
	}
	if tmdbKey == nil {
		tmdbKey = func(context.Context) string { return "" }
	}
	if mediaRoot == nil {
		mediaRoot = func() string { return "" }
	}
	return &Service{st: st, tmdbKey: tmdbKey, mediaRoot: mediaRoot, log: log,
		jobs: map[int64]*Job{}}
}

// Job 返回某库当前的任务（没有则 nil）。
func (s *Service) Job(libraryID int64) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[libraryID]
}

// Start 后台跑一次刮削。
func (s *Service) Start(lib store.Library, opt Options) (*Job, error) {
	s.mu.Lock()
	if j, ok := s.jobs[lib.ID]; ok && j.Running() {
		s.mu.Unlock()
		return nil, ErrRunning
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ctx: ctx, cnl: cancel, done: make(chan struct{})}
	j.p = Progress{
		LibraryID: lib.ID,
		Phase:     "parse",
		Running:   true,
		StartedAt: time.Now(),
		Message:   "正在解析文件名",
	}
	s.jobs[lib.ID] = j
	s.mu.Unlock()

	go func() {
		defer cancel()
		defer close(j.done)
		err := s.run(ctx, j, lib, opt)
		j.update(func(p *Progress) {
			p.Running = false
			p.FinishedAt = time.Now()
			p.Current = ""
			switch {
			case err == nil:
				p.Phase = "done"
				p.Message = fmt.Sprintf("完成：匹配 %d 部，未匹配 %d 部", p.Matched, p.Unmatched)
			case errors.Is(err, context.Canceled):
				p.Phase = "canceled"
				p.Message = "已取消"
			default:
				p.Phase = "error"
				p.Err = err.Error()
				p.Message = "刮削失败"
			}
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			s.log.Error("刮削失败", "lib", lib.Name, "err", err)
		}
	}()
	return j, nil
}

type workItem struct {
	file   store.File
	parsed Parsed
	// folderSolo 表示这个视频所在目录只对应一部作品。
	// 只有「一部片一个文件夹」的布局才写目录级 movie.nfo / poster.jpg；
	// 一个目录里塞了好几部片时写进去会互相覆盖，只能退回逐文件命名。
	folderSolo bool
}

// keyOf 把解析结果折叠成一个查询分组：IMDb ID 优先，没 ID 才按片名+年份。
// 同一部剧的 24 集会归到同一个分组，只查一次 IMDb。
func keyOf(p Parsed) groupKey {
	return groupKey{kind: p.Kind, title: normalizeTitle(p.Title), year: p.Year, imdb: p.IMDbID}
}

type groupKey struct {
	kind  string
	title string
	year  string
	// imdb 非空表示这个分组有明确的 IMDb ID（来自文件名），在纯 TMDB
	// 链路里只作 /find 的反查键。
	imdb string
	// tmdb 非空表示用户/台账直接指定了 TMDB 条目，优先级最高：
	// 不搜索也不反查，直接按 ID 取详情。
	tmdb int
}

func (s *Service) run(ctx context.Context, j *Job, lib store.Library, opt Options) error {
	if opt.Rate <= 0 {
		opt.Rate = DefaultOptions().Rate
	}
	if opt.ImageSize <= 0 {
		opt.ImageSize = 12 << 20
	}

	// 库路径可能是从别的环境（容器）搬过来的，先规整成这台机器上真能写的目录。
	root := ResolveRoot(lib.LocalPath, s.mediaRoot())
	if root != lib.LocalPath {
		s.log.Info("媒体库路径已按媒体根重算", "lib", lib.Name, "from", lib.LocalPath, "to", root)
	}

	files, err := s.st.ListFilesAll(ctx, lib.ID)
	if err != nil {
		return fmt.Errorf("读台账: %w", err)
	}

	// —— 阶段一：解析文件名（纯本地）——
	var items []workItem
	seenPath := make(map[string]struct{}, len(files))
	for _, f := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !strm.IsVideo(f.Name) {
			continue
		}
		if opt.MinSize > 0 && f.Size > 0 && f.Size < opt.MinSize {
			continue
		}
		p := Parse(f.RelPath)
		items = append(items, workItem{file: f, parsed: p})
		seenPath[f.RelPath] = struct{}{}
		j.update(func(pp *Progress) {
			pp.Total = len(items)
			pp.Current = f.RelPath
		})
	}

	// 目录里到底有几部作品：决定能不能写目录级元数据（movie.nfo / poster.jpg）。
	// 顺带记下每部作品的原始片名——归一化后的 key 掉了标点，拿它写标题会缺字符。
	dirGroups := make(map[string]map[groupKey]struct{}, 64)
	rawTitle := make(map[groupKey]string, 64)
	for _, it := range items {
		if it.parsed.Title == "" {
			continue
		}
		k := keyOf(it.parsed)
		if _, ok := rawTitle[k]; !ok {
			rawTitle[k] = it.parsed.Title
		}
		dir := path.Dir(it.file.RelPath)
		if dirGroups[dir] == nil {
			dirGroups[dir] = map[groupKey]struct{}{}
		}
		dirGroups[dir][k] = struct{}{}
	}
	for i := range items {
		dir := path.Dir(items[i].file.RelPath)
		// 目录名要能认出来是这部片（`空枪.2026.{tmdbid=…}` 这种）。
		// 像 `电影/` 这种纯分类目录里也可能只放了一部片，但把 movie.nfo
		// 写进去等于给整个分类目录安了一个片名，之后再加片就全乱了。
		items[i].folderSolo = len(dirGroups[dir]) == 1 &&
			folderMatchesTitle(path.Base(dir), items[i].parsed.Title)
	}

	// —— 阶段二：按片名分组查元数据 ——
	//
	// 分组是这一整段存在的唯一理由：一季 24 集如果逐集查 TMDB，就是 24 次
	// 请求；按「类型+片名+年份」归并后只查 1 次，剩下的从缓存里取。
	var keys []groupKey
	seen := make(map[groupKey]struct{}, 64)
	// 每组真正出现的季号。TMDB 的分集要按季逐个问，只问库里有的那几季，
	// 别给 60 季的长寿剧一次打 60 个接口。
	seasonNeeds := make(map[groupKey]map[int]struct{}, 64)
	for _, it := range items {
		if it.parsed.Title == "" {
			continue
		}
		k := keyOf(it.parsed)
		if _, ok := seen[k]; !ok {
			seen[k] = struct{}{}
			keys = append(keys, k)
		}
		if it.parsed.Season > 0 {
			if seasonNeeds[k] == nil {
				seasonNeeds[k] = map[int]struct{}{}
			}
			seasonNeeds[k][it.parsed.Season] = struct{}{}
		}
	}

	j.update(func(p *Progress) {
		p.Phase = "meta"
		p.Groups = len(keys)
		p.Message = fmt.Sprintf("需要查询 %d 部作品", len(keys))
	})

	metas := make(map[groupKey]Meta, len(keys))
	groupErr := make(map[groupKey]string, len(keys))

	pace := &pacer{gap: opt.Rate}

	for idx, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		j.update(func(p *Progress) {
			p.Current = k.title
			p.Message = fmt.Sprintf("查询元数据 %d/%d", idx+1, len(keys))
		})

		m, fromCache, err := s.resolveOne(ctx, pace, k, rawTitle[k], neededSeasons(seasonNeeds[k]), opt, j)
		if err != nil {
			groupErr[k] = err.Error()
			s.log.Warn("查元数据失败", "title", k.title, "year", k.year, "err", err)
			continue
		}
		metas[k] = m
		if fromCache {
			j.update(func(p *Progress) { p.Cached++ })
		}
	}

	// —— 阶段三：逐条落盘 ——
	j.update(func(p *Progress) { p.Phase = "write"; p.Message = "写入元数据与图片" })

	keep := make(map[string]struct{}, len(items))
	// 目录级文件（movie.nfo / tvshow.nfo / poster.jpg）只写一次，
	// 靠这个集合去重：同一部剧 24 集共用一份 tvshow.nfo。
	done := make(map[string]bool, len(items))
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		keep[it.file.RelPath] = struct{}{}

		m := store.Media{
			LibraryID: lib.ID,
			RelPath:   it.file.RelPath,
			Kind:      it.parsed.Kind,
			Title:     it.parsed.Title,
			Year:      it.parsed.Year,
			Season:    it.parsed.Season,
			Episode:   it.parsed.Episode,
			Status:    "unmatched",
		}

		k := keyOf(it.parsed)
		switch {
		case it.parsed.Title == "":
			m.Status = "unmatched"
			m.Message = "文件名里没有可用的片名"
			j.update(func(p *Progress) { p.Unmatched++ })
		case groupErr[k] != "":
			m.Status = "error"
			m.Message = groupErr[k]
			j.update(func(p *Progress) { p.Failed++ })
		default:
			if meta, ok := metas[k]; ok {
				// 类型纠偏只做「安全」的那一半：元数据说是电影就按电影写；
				// 说是剧集但这集没有季集号时保持原判——否则会写出 S00E00
				// 这种假剧集，Emby 里反而更乱。
				switch {
				case meta.Kind == KindMovie:
					it.parsed.Kind = KindMovie
					m.Kind = KindMovie
				case meta.Kind == KindSeries && it.parsed.Season > 0 && it.parsed.Episode > 0:
					it.parsed.Kind = KindSeries
					m.Kind = KindSeries
				}
				m = applyMeta(m, meta)
				m.Status = "ok"
				j.update(func(p *Progress) { p.Matched++ })
			} else {
				m.Status = "unmatched"
				m.Message = "TMDB 里没找到足够接近的结果"
				j.update(func(p *Progress) { p.Unmatched++ })
			}
		}

		// 单集清单只存在于 Meta 里（不进数据库），写 nfo 时得原样带过去，
		// 否则每一集又会退回剧名。
		if err := s.writeArtifacts(ctx, root, metas[k], &m, it, opt, done); err != nil {
			m.Status = "error"
			m.Message = err.Error()
			j.update(func(p *Progress) { p.Failed++ })
			attrs := []any{"rel", it.file.RelPath, "err", err}
			if hint := writeHint(err, root); hint != "" {
				attrs = append(attrs, "hint", hint)
			}
			s.log.Warn("写元数据文件失败", attrs...)
		}

		if err := s.st.UpsertMedia(ctx, m); err != nil {
			s.log.Warn("写刮削结果失败", "rel", it.file.RelPath, "err", err)
		}

		j.update(func(p *Progress) {
			p.Done++
			p.Current = it.file.RelPath
		})
	}

	// 网盘里已经不存在的文件，刮削结果也一并清掉
	if _, err := s.st.DeleteMediaNotIn(ctx, lib.ID, keep); err != nil {
		s.log.Warn("清理过期刮削记录失败", "err", err)
	}
	_ = seenPath
	return nil
}

// RescrapeOne 用指定的 IMDb ID 重刮一条记录，产物写到 .strm 旁边。
//
// 两种场景：
//  1. 自动匹配错了，用户手动填一个正确的 ID；
//  2. 文件名里既没有 ID、片名也搜不到，用户自己找到 ID 再补。
//
// 单条重刮只碰这一条：不扫库、不重新遍历网盘；请求数取决于缓存命中，
// 正常情况是 1 次 meta（命中本地缓存时是 0 次）。
// RescrapeOne 用 IMDb ID 重刮一条：ID 只作为 TMDB /find 的反查键，
// 元数据本身全部来自 TMDB。保留这个入口是因为文件名与老 nfo 里都是 ttID。
func (s *Service) RescrapeOne(ctx context.Context, lib store.Library, m store.Media,
	imdbID string, opt Options) (store.Media, error) {

	imdbID = NormalizeIMDbID(imdbID)
	if imdbID == "" {
		return m, errors.New("IMDb ID 不合法，应该形如 tt15239678")
	}
	k := rescrapeKey(m)
	k.imdb = imdbID
	return s.rescrapeWith(ctx, lib, m, k, imdbID, opt)
}

// RescrapeOneByTMDbID 直接按 TMDB ID 重刮一条。
//
// 纯 TMDB 之后台账里的 tmdb_id 才是主键：海报墙上认错片时，改这个 ID
// 比去翻 IMDb 链接直观得多。它连 /find 都不用，一次详情请求就够。
func (s *Service) RescrapeOneByTMDbID(ctx context.Context, lib store.Library, m store.Media,
	tmdbID int, opt Options) (store.Media, error) {

	if tmdbID <= 0 {
		return m, errors.New("TMDB ID 不合法，应该是正整数")
	}
	k := rescrapeKey(m)
	k.imdb = ""
	k.tmdb = tmdbID
	return s.rescrapeWith(ctx, lib, m, k, "", opt)
}

// rescrapeKey 按文件名解析类型与季集号，台账补缺。
func rescrapeKey(m store.Media) groupKey {
	p := Parse(m.RelPath)
	k := keyOf(p)
	if k.kind == "" {
		k.kind = m.Kind
	}
	if k.kind == "" {
		k.kind = KindMovie
	}
	return k
}

// rescrapeWith 是两种单条重刮共用的落盘逻辑。
// fallbackIMDb 只在 TMDB 没返回 external_ids 时补上，不再无条件覆盖。
func (s *Service) rescrapeWith(ctx context.Context, lib store.Library, m store.Media,
	k groupKey, fallbackIMDb string, opt Options) (store.Media, error) {

	if opt.Rate <= 0 {
		opt.Rate = DefaultOptions().Rate
	}
	if opt.ImageSize <= 0 {
		opt.ImageSize = 12 << 20
	}
	if opt.Language == "" {
		opt.Language = "zh"
	}

	root := ResolveRoot(lib.LocalPath, s.mediaRoot())
	p := Parse(m.RelPath)
	if p.Kind == "" {
		p.Kind = k.kind
	}
	if p.Season == 0 && m.Season > 0 {
		p.Season = m.Season
	}
	if p.Episode == 0 && m.Episode > 0 {
		p.Episode = m.Episode
	}

	item := workItem{
		file:       store.File{LibraryID: lib.ID, RelPath: m.RelPath},
		parsed:     p,
		folderSolo: s.soloDir(ctx, lib.ID, m.RelPath, p),
	}
	// 剧集要按真正出现的季去拉分集清单，否则单集 nfo 会退回剧名。
	seasons := []int{p.Season}

	job := &Job{} // 单条重刮没有进度条，但 resolveOne 会往上记「实际请求数」
	pace := &pacer{gap: opt.Rate}
	meta, _, err := s.resolveOne(ctx, pace, k, p.Title, seasons, opt, job)
	if err != nil {
		return m, err
	}

	// 以元数据声明的类型为准，但只做安全的那一半：
	// ID 指向剧集、这条却解析不出季集号时保持原判，避免写 S00E00 的假剧集。
	switch {
	case meta.Kind == KindMovie:
		p.Kind = KindMovie
	case meta.Kind == KindSeries && p.Season > 0 && p.Episode > 0:
		p.Kind = KindSeries
	}
	out := m
	out.Kind = p.Kind
	out.Season, out.Episode = p.Season, p.Episode
	// 先把旧字段清干净：换成另一部片时，残留的导演/演员/简介会串味。
	out.MetaTitle, out.MetaYear = "", ""
	out.Rating, out.Runtime = "", ""
	out.Genres, out.Cast, out.Director, out.Overview = "", "", "", ""
	out.PosterURL, out.BackdropURL = "", ""
	out = applyMeta(out, meta)
	if out.IMDbID == "" {
		out.IMDbID = fallbackIMDb
	}
	out.Status = "ok"
	out.Message = ""

	done := map[string]bool{}
	if err := s.writeArtifacts(ctx, root, meta, &out, item, opt, done); err != nil {
		out.Status = "error"
		out.Message = err.Error()
	}
	if err := s.st.UpsertMedia(ctx, out); err != nil {
		return out, err
	}
	return out, nil
}

// soloDir 判断这条视频所在目录是不是「只放这一部作品」。
// 只有这种目录才能写 movie.nfo / poster.jpg，否则会盖掉同目录的其他片子。
func (s *Service) soloDir(ctx context.Context, libID int64, relPath string, p Parsed) bool {
	if !folderMatchesTitle(path.Base(path.Dir(relPath)), p.Title) {
		return false
	}
	files, err := s.st.ListFilesAll(ctx, libID)
	if err != nil {
		return false
	}
	dir := path.Dir(relPath)
	keys := map[groupKey]struct{}{}
	for _, f := range files {
		if path.Dir(f.RelPath) != dir || !strm.IsVideo(f.Name) {
			continue
		}
		keys[keyOf(Parse(f.RelPath))] = struct{}{}
	}
	return len(keys) == 1
}

// neededSeasons 把 map 里的季号排序输出，让缓存键与请求顺序稳定。
func neededSeasons(m map[int]struct{}) []int {
	if len(m) == 0 {
		return nil
	}
	out := make([]int, 0, len(m))
	for n := range m {
		out = append(out, n)
	}
	sort.Ints(out)
	return out
}

// seasonsCovered 判断缓存里的分集清单是否覆盖了这次需要的全部季。
//
// 后来往库里加了 S02，旧缓存只有 S01 的集标题；只看 EpisodesFetched
// 会让新一季全部退回剧名，必须按季比对。
func seasonsCovered(m Meta, need []int) bool {
	if !m.EpisodesFetched {
		return false
	}
	have := make(map[int]bool, len(m.EpisodesSeasons))
	for _, n := range m.EpisodesSeasons {
		have[n] = true
	}
	for _, n := range need {
		if n > 0 && !have[n] {
			return false
		}
	}
	return true
}

// resolveOne 取一部作品的元数据：先查本地缓存，再查 TMDB。
//
// rawTitle 是文件名里的原始片名（未归一化）。归一化会去掉标点，
// 而中文片名恰恰靠标点分词，所以中文名必须用原始串。
//
// 分组里带 IMDb ID 时走 /find 反查 TMDB 条目，否则按片名搜。这是纯 TMDB
// 链路：Cinemeta / IMDb 一条请求都不发，IMDb ID 只由 TMDB 的 external_ids
// 顺带带回，用作标识、缓存键与 nfo 的 uniqueid，不再是数据源。
func (s *Service) resolveOne(ctx context.Context, pace *pacer,
	k groupKey, rawTitle string, seasons []int, opt Options, j *Job) (Meta, bool, error) {

	cacheKey := metaCacheKey(k)

	if !opt.RefreshCache {
		if _, payload, found, err := s.st.GetMetaCache(ctx, cacheKey); err == nil && found {
			var m Meta
			// TMDbID 是「这份缓存是不是新链路写的」的唯一判据：老缓存是
			// IMDb/Cinemeta 时代的产物，不能拿来冒充 TMDB 结果，必须重刮。
			if json.Unmarshal([]byte(payload), &m) == nil && m.TMDbID > 0 {
				if k.kind != KindSeries || seasonsCovered(m, seasons) {
					m.Source = "cache"
					loc := s.localize(ctx, m, rawTitle, opt)
					loc = s.enrichCast(ctx, loc, opt)
					if loc.TitleCN != m.TitleCN || loc.Overview != m.Overview ||
						len(loc.CastDetail) != len(m.CastDetail) {
						if payload, err := json.Marshal(loc); err == nil {
							if err := s.st.PutMetaCache(ctx, cacheKey, loc.IMDbID, string(payload)); err != nil {
								s.log.Warn("回写本地化缓存失败", "key", cacheKey, "err", err)
							}
						}
					}
					return loc, true, nil
				}
				s.log.Info("缓存缺少本轮需要的分集季，重查一次", "title", k.title, "seasons", seasons)
			}
		}
	}

	if !opt.FetchMeta {
		return Meta{}, false, errors.New("未开启元数据查询")
	}

	c := s.tmdbClient(ctx)
	if c == nil {
		return Meta{}, false, errors.New("没有可用的 TMDB 客户端")
	}

	var d *tmdb.MetaDetail
	var err error
	if k.tmdb > 0 {
		if err = pace.wait(ctx); err != nil {
			return Meta{}, false, err
		}
		j.update(func(p *Progress) { p.Fetched++ })
		kind := tmdb.KindMovie
		if k.kind == KindSeries || strings.EqualFold(k.kind, tmdb.KindTV) {
			kind = tmdb.KindTV
		}
		d, err = c.MetaByID(ctx, kind, k.tmdb)
	} else if k.imdb != "" {
		if err = pace.wait(ctx); err != nil {
			return Meta{}, false, err
		}
		j.update(func(p *Progress) { p.Fetched++ })
		d, err = c.MetaByIMDbID(ctx, k.imdb, k.kind)
	} else {
		if err = pace.wait(ctx); err != nil {
			return Meta{}, false, err
		}
		j.update(func(p *Progress) { p.Fetched++ })
		hits, e := c.SearchMeta(ctx, firstNonEmpty(rawTitle, k.title), k.year, k.kind)
		if e != nil {
			return Meta{}, false, e
		}
		parsed := Parsed{Title: k.title, Year: k.year, Kind: k.kind}
		hit, ok := Pick(parsed, hits)
		if !ok {
			// 中文名对不上不等于没有这部片：国创 / 日番的发布名常常只有英文名
			// （`A Record of a Mortal’s Journey to Immortality` == 凡人修仙传），
			// 而 TMDB 的 zh-CN 名字只有中文。用 en-US 再搜一次，那边返回的
			// 就是英文名。只在第一次没认出来时多花这一次请求。
			if e := pace.wait(ctx); e == nil {
				j.update(func(p *Progress) { p.Fetched++ })
				if en, err := c.SearchMetaLang(ctx, firstNonEmpty(rawTitle, k.title), k.year, k.kind, "en-US"); err == nil {
					hit, ok = Pick(parsed, en)
				}
			}
		}
		if !ok {
			return Meta{}, false, errors.New("TMDB 里没找到足够接近的结果")
		}
		if err = pace.wait(ctx); err != nil {
			return Meta{}, false, err
		}
		j.update(func(p *Progress) { p.Fetched++ })
		d, err = c.MetaByID(ctx, hit.Kind, hit.ID)
	}
	if err != nil {
		return Meta{}, false, fmt.Errorf("查 TMDB 元数据失败: %w", err)
	}

	m := metaFromDetail(d)

	// 「original」语言下不要中文标题，nfo 的 title 就写原名。
	if opt.Language != "" && !strings.EqualFold(opt.Language, "zh") {
		m.TitleCN = ""
	}

	// 剧集单集清单：只问库里真正出现的季，单季失败不影响其它季。
	if d.Kind == tmdb.KindTV {
		m.EpisodesFetched = true
		fetched := make([]int, 0, len(seasons))
		for _, n := range seasons {
			if n <= 0 {
				continue
			}
			if e := pace.wait(ctx); e != nil {
				return Meta{}, false, e
			}
			j.update(func(p *Progress) { p.Fetched++ })
			eps, e := c.SeasonEpisodes(ctx, d.ID, n)
			if e != nil {
				s.log.Debug("取分集失败，这一季退回剧名", "id", d.ID, "season", n, "err", e)
				continue
			}
			fetched = append(fetched, n)
			for _, ep := range eps {
				m.Episodes = append(m.Episodes, Episode{
					Season: ep.Season, Episode: ep.Episode, Title: ep.Name,
					Overview: ep.Overview, Aired: ep.AirDate,
				})
			}
		}
		m.EpisodesSeasons = fetched
	}

	// 中文优先：TMDB 已经用 zh-CN 给了标题与简介，localize 只补文件名里
	// 更准的中文译名；翻译接口基本不会再被调用。
	m = s.localize(ctx, m, rawTitle, opt)
	// 演员表用 TMDbID 直接取 credits（不再绕 /find），中文名 / 角色 / 头像一次到位。
	m = s.enrichCast(ctx, m, opt)

	s.cacheMeta(ctx, cacheKey, m)
	// 另几条键也补一份，让「按 ID 重刮」和「按片名搜」互相预热：
	// 之后不管文件名里有没有 ttID，都能零请求命中。
	if m.IMDbID != "" {
		s.cacheMeta(ctx, "imdb|"+m.IMDbID, m)
	}
	if m.TMDbID > 0 {
		s.cacheMeta(ctx, "tmdb|"+strconv.Itoa(m.TMDbID), m)
	}
	if k.title != "" {
		titleKey := store.MetaCacheKey(firstNonEmpty(m.Kind, k.kind), k.title,
			firstNonEmpty(m.Year, k.year))
		s.cacheMeta(ctx, titleKey, m)
	}
	return m, false, nil
}

// metaFromDetail 把 TMDB 详情映射成刮削链路用的 Meta。
//
// Title 放原名（nfo 的 originaltitle），TitleCN 放 TMDB 的 zh-CN 标题
// （nfo 的 title）。以前 TitleCN 只能靠文件名或机翻凑，现在有官方中文名了。
func metaFromDetail(d *tmdb.MetaDetail) Meta {
	m := Meta{
		IMDbID:   d.IMDbID,
		TMDbID:   d.ID,
		Title:    firstNonEmpty(d.Original, d.Title),
		TitleCN:  d.Title,
		Year:     d.Year,
		Rating:   d.Rating,
		Runtime:  d.Runtime,
		Genres:   d.Genres,
		Overview: d.Overview,
		Poster:   d.Poster,
		Backdrop: d.Backdrop,
		Source:   "tmdb",
		Kind:     KindMovie,
	}
	if d.Kind == tmdb.KindTV {
		m.Kind = KindSeries
	}
	return m
}

// metaCacheKey 是按 ID 优先的缓存键：同一部片改名、换目录都不会重查。
func metaCacheKey(k groupKey) string {
	if k.tmdb > 0 {
		return "tmdb|" + strconv.Itoa(k.tmdb)
	}
	if k.imdb != "" {
		return "imdb|" + k.imdb
	}
	return store.MetaCacheKey(k.kind, k.title, k.year)
}

func (s *Service) cacheMeta(ctx context.Context, key string, m Meta) {
	if key == "" || (m.IMDbID == "" && m.TMDbID <= 0) {
		return
	}
	payload, err := json.Marshal(m)
	if err != nil {
		return
	}
	if err := s.st.PutMetaCache(ctx, key, m.IMDbID, string(payload)); err != nil {
		s.log.Warn("写元数据缓存失败", "key", key, "err", err)
	}
}

func applyMeta(m store.Media, meta Meta) store.Media {
	m.IMDbID = meta.IMDbID
	m.TMDbID = meta.TMDbID
	m.MetaTitle = meta.displayTitle()
	m.MetaYear = meta.Year
	m.Rating = meta.Rating
	m.Runtime = meta.Runtime
	m.Genres = strings.Join(meta.Genres, ", ")
	m.Cast = strings.Join(meta.Cast, ", ")
	m.Director = strings.Join(meta.Director, ", ")
	m.Overview = meta.Overview
	m.PosterURL = meta.Poster
	m.BackdropURL = meta.Backdrop
	return m
}

// ==================== 落盘 ====================

// writeArtifacts 写 nfo 与本地图片。全部失败都只是「少一份文件」，不中断刮削。
//
// 文件名不是随便定的，Emby 有一套固定约定，错一个字母就等于没写：
//
//	电影（独占一个目录）   movie.nfo + poster.jpg + fanart.jpg
//	电影（同目录多部）      <视频名>.nfo + <视频名>-poster.jpg
//	剧集（剧根目录）        tvshow.nfo + poster.jpg + fanart.jpg
//	剧集（每一集）          <视频名>.nfo
//
// 之前只写 <视频名>.nfo 和 <视频名>-poster.jpg，目录级的三件套一个都没有，
// 结果就是 Emby 里标题是文件名、简介空白、海报是灰块——「没有影视信息」。
func (s *Service) writeArtifacts(ctx context.Context, root string, src Meta, m *store.Media,
	it workItem, opt Options, done map[string]bool) error {

	videoPath := filepath.Join(root, filepath.FromSlash(it.file.RelPath))
	dir := filepath.Dir(videoPath)
	base := strings.TrimSuffix(filepath.Base(videoPath), filepath.Ext(videoPath))

	meta := nfoMeta(src, m)

	if it.parsed.Kind == KindSeries {
		return s.writeSeriesArtifacts(ctx, root, meta, m, it, opt, done, dir, base)
	}
	return s.writeMovieArtifacts(ctx, root, meta, m, it, opt, done, dir, base)
}

// nfoMeta 把「元数据」和「台账行」拼成写 nfo 用的那份 Meta。
//
// 单独抽出来是因为这里漏一个字段不会报错，只会让某样东西凭空消失：
// 之前 CastDetail 没被复制过来，结果 nfo 里的 <actor> 只剩名字，
// Emby 里的演员头像一直是一片灰 —— 线上跑了才发现，测试全绿。
func nfoMeta(src Meta, m *store.Media) Meta {
	out := Meta{
		IMDbID:     m.IMDbID,
		TMDbID:     m.TMDbID,
		Title:      firstNonEmpty(src.Title, m.MetaTitle),
		TitleCN:    src.TitleCN,
		Year:       m.MetaYear,
		Rating:     m.Rating,
		Runtime:    m.Runtime,
		Genres:     splitList(m.Genres),
		Cast:       splitList(m.Cast),
		CastDetail: src.CastDetail,
		Director:   splitList(m.Director),
		Overview:   m.Overview,
		// 单集清单来自元数据，不落库（一季几十条，进 SQLite 只是白占地方）。
		Episodes:        src.Episodes,
		EpisodesFetched: src.EpisodesFetched,
	}
	// 老缓存里没有 TitleCN，但 MetaTitle 已经是中文名了，别把它弄丢。
	if out.TitleCN == "" && hasChinese(m.MetaTitle) {
		out.TitleCN = m.MetaTitle
	}
	return out
}

func (s *Service) writeMovieArtifacts(ctx context.Context, root string, meta Meta, m *store.Media,
	it workItem, opt Options, done map[string]bool, dir, base string) error {

	// 1) 每个 .strm 一份同名 nfo。一个目录里放了好几个版本时（1080p/2160p），
	//    Emby 把它们当成不同条目，只有同名 nfo 才能各自生效。
	if opt.WriteNFO && m.IMDbID != "" {
		data, err := BuildMovieNFO(meta)
		if err != nil {
			return fmt.Errorf("生成 nfo: %w", err)
		}
		nfoPath := filepath.Join(dir, base+".nfo")
		if err := s.writeNFO(nfoPath, data, opt); err != nil {
			return fmt.Errorf("写 nfo: %w", err)
		}
		m.NFOFile = relTo(root, nfoPath)
	}

	solo := it.folderSolo && filepath.Clean(dir) != filepath.Clean(root)

	// 2) 目录级三件套：Emby 认的就是这三个固定名字，与视频文件名叫什么无关。
	if solo {
		if opt.WriteNFO && m.IMDbID != "" && !done["nfo:"+dir] {
			data, err := BuildMovieNFO(meta)
			if err == nil {
				if err := s.writeNFO(filepath.Join(dir, "movie.nfo"), data, opt); err != nil {
					s.log.Debug("写 movie.nfo 失败", "dir", dir, "err", err)
				}
			}
			done["nfo:"+dir] = true
		}
		if opt.FetchImages && m.IMDbID != "" && !done["img:"+dir] {
			s.savePair(ctx, root, m, dir, "poster.jpg", "fanart.jpg", opt)
			done["img:"+dir] = true
		}
		return nil
	}

	// 3) 平铺布局（视频直接放在库根目录，或一个目录里有好几部片）：
	//    没有「目录」可挂，只能按文件名区分。
	if opt.FetchImages && m.IMDbID != "" {
		s.savePair(ctx, root, m, dir, base+"-poster.jpg", base+"-fanart.jpg", opt)
	}
	return nil
}

func (s *Service) writeSeriesArtifacts(ctx context.Context, root string, meta Meta, m *store.Media,
	it workItem, opt Options, done map[string]bool, dir, base string) error {

	showDir := filepath.Join(root, filepath.FromSlash(showRoot(it.file.RelPath)))

	// 剧级元数据只写一次。24 集各写一遍内容一样，纯属多几次磁盘 IO，
	// 还会在同步日志里刷出 24 条「写入」。
	if opt.WriteNFO && m.IMDbID != "" && !done["tv:"+showDir] {
		if data, err := BuildTVShowNFO(meta); err == nil {
			if err := s.writeNFO(filepath.Join(showDir, "tvshow.nfo"), data, opt); err != nil {
				s.log.Debug("写 tvshow.nfo 失败", "dir", showDir, "err", err)
			}
		}
		done["tv:"+showDir] = true
	}
	if opt.FetchImages && m.IMDbID != "" && !done["img:"+showDir] {
		s.savePair(ctx, root, m, showDir, "poster.jpg", "fanart.jpg", opt)
		done["img:"+showDir] = true
	}

	// 单集 nfo 必须每集一份：Emby 的剧集信息来自这里，不是 tvshow.nfo。
	if opt.WriteNFO && m.IMDbID != "" {
		data, err := BuildEpisodeNFO(meta, it.parsed.Season, it.parsed.Episode)
		if err != nil {
			return fmt.Errorf("生成 nfo: %w", err)
		}
		nfoPath := filepath.Join(dir, base+".nfo")
		if err := s.writeNFO(nfoPath, data, opt); err != nil {
			return fmt.Errorf("写 nfo: %w", err)
		}
		m.NFOFile = relTo(root, nfoPath)
	}
	return nil
}

// savePair 下载海报与背景图，并把落盘位置记到台账里。
func (s *Service) savePair(ctx context.Context, root string, m *store.Media, dir, poster, fanart string, opt Options) {
	posterPath := filepath.Join(dir, poster)
	fanartPath := filepath.Join(dir, fanart)

	if err := s.saveImage(ctx, m.PosterURL, posterPath, opt); err != nil {
		s.log.Debug("海报下载失败", "url", m.PosterURL, "err", err)
	} else if fileExists(posterPath) {
		m.PosterFile = relTo(root, posterPath)
	}
	if err := s.saveImage(ctx, m.BackdropURL, fanartPath, opt); err != nil {
		s.log.Debug("背景图下载失败", "url", m.BackdropURL, "err", err)
	} else if fileExists(fanartPath) {
		m.BackdropFile = relTo(root, fanartPath)
	}
}

// writeNFO 按「不覆盖用户手改」的原则落盘，但会自作主张地补齐不完整的 nfo。
//
// 判断依据很简单：旧版本写出来的 nfo 没有 <plot>，那正是 Emby 里
// 「有片子没简介」的原因。这种文件重写不会丢任何用户数据，
// 却能让升级后的第一次刮削就把老库修好——不需要用户记得勾「覆盖」。
func (s *Service) writeNFO(path string, data []byte, opt Options) error {
	if old, err := os.ReadFile(path); err == nil && !opt.Overwrite {
		if string(old) == string(data) || strings.Contains(string(old), "<plot>") {
			// 例外：老 nfo 有演员但一个 <actor><thumb> 都没有，说明是
			// 「演员头像」这次改动之前写的。它不会丢任何用户数据（只是
			// 给每个人补一个头像地址），不重写的话，用户升级后看到的
			// 仍然是一片灰头像，还以为功能没生效。
			if !needsCastThumb(string(old), data) {
				return nil
			}
		}
	}
	return writeFileAtomic(path, data)
}

// needsCastThumb 判断这次重写是不是「只为补演员头像」。
//
// 只在旧文件已经有 <actor> 而缺 <thumb>、而新内容确实带了 <thumb> 时成立。
// 其它情况一律尊重「不覆盖用户手改」的原则。
func needsCastThumb(old string, data []byte) bool {
	if !strings.Contains(old, "<actor>") || strings.Contains(old, "<thumb>") {
		return false
	}
	return bytes.Contains(data, []byte("<thumb>"))
}

// folderMatchesTitle 判断目录名是不是「这部片自己的目录」。
//
// 归一化之后比包含关系即可：`空枪.2026.{tmdbid=1567937}` 含「空枪」，
// 而 `电影` / `华语电影` / `Season 1` 都不含片名。宁可少写一次目录级
// 元数据（退回逐文件命名，Emby 一样认），也不要把整层分类目录当成一部片。
func folderMatchesTitle(dirName, title string) bool {
	d := normalizeTitle(dirName)
	t := normalizeTitle(title)
	if d == "" || t == "" {
		return false
	}
	return strings.Contains(d, t) || strings.Contains(t, d)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// saveImage 下载图片。已存在且不覆盖时直接返回成功。
func (s *Service) saveImage(ctx context.Context, url, dest string, opt Options) error {
	if strings.TrimSpace(url) == "" {
		return errors.New("没有图片地址")
	}
	if fileExists(dest) && !opt.Overwrite {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36")
	resp, err := imageClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("图片返回 %d", resp.StatusCode)
	}
	limit := opt.ImageSize
	if limit <= 0 {
		limit = 12 << 20
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return err
	}
	if len(data) < 512 {
		return errors.New("图片内容过小，可能是错误页")
	}
	return writeFileAtomic(dest, data)
}

var imageClient = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: netproxy.URL}}

// ==================== 辅助 ====================

// pacer 保证两次请求之间有最小间隔。简单、够用，不需要令牌桶。
type pacer struct {
	mu   sync.Mutex
	last time.Time
	gap  time.Duration
}

func (p *pacer) wait(ctx context.Context) error {
	if p.gap <= 0 {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.last.IsZero() {
		if d := p.gap - time.Since(p.last); d > 0 {
			t := time.NewTimer(d)
			select {
			case <-ctx.Done():
				t.Stop()
				return ctx.Err()
			case <-t.C:
			}
		}
	}
	p.last = time.Now()
	return nil
}

// showRoot 找剧集根目录：`剧名/Season 01/X.strm` → `剧名`。
// ShowRoot 返回剧集在媒体库里的「剧根目录」（相对路径）。
//
// 集数通常放在 Season 1 / S01 这类子目录里，而 tvshow.nfo 与 poster.jpg
// 写在上一级。海报墙、总览面板要拿剧级信息（一张卡、一张海报）时，
// 都得先回到这一级，所以把内部逻辑导出，别让调用方各写一份。
func ShowRoot(relPath string) string { return showRoot(relPath) }

func showRoot(relPath string) string {
	dir := path.Dir(relPath)
	if dir == "." {
		return ""
	}
	if base := path.Base(dir); isSeasonDir(base) {
		parent := path.Dir(dir)
		if parent == "." {
			return ""
		}
		return parent
	}
	return dir
}

func splitList(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir() && st.Size() > 0
}

func relTo(root, p string) string {
	r, err := filepath.Rel(root, p)
	if err != nil {
		return p
	}
	return filepath.ToSlash(r)
}

// writeFileAtomic 先写临时文件再改名，避免媒体服务器读到半截文件。
func writeFileAtomic(dest string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp := dest + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// writeHint 把「写不进去」翻译成一句能直接照抄的命令。
//
// 这是部署上最常见的一类失败：媒体目录是 root 建的（或者迁移时用 sudo 搬过），
// 而服务以普通用户跑，于是刮削能查到元数据、海报和 nfo 却一个字都写不下去，
// 页面上只显示「刮削失败」。日志里给出 uid 和 chown 命令，比让人去猜快得多。
func writeHint(err error, root string) string {
	if !errors.Is(err, fs.ErrPermission) {
		return ""
	}
	return fmt.Sprintf("媒体目录对当前用户（uid %d）不可写，可在宿主机执行：chown -R %d:%d %s",
		os.Getuid(), os.Getuid(), os.Getgid(), root)
}
