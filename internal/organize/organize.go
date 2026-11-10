// Package organize 是「自动整理」的识别与落位规则。
//
// 它只做纯计算：给一个网盘条目名，算出租该进哪个目录、把握有多大、要不要跳过。
// 真正调 115 移动文件的部分在 httpapi/organize.go —— 这样规则可以单测，
// 也不会让「整理规则」和「网盘调用」互相纠缠。
//
// 识别直接复用 scrape.Parse：它已经在处理 `沙丘2.2024.2160p` / `某剧 第2季 S02E05`
// 这类命名，而且被刮削链路验证过几万次。再写一套「整理专用」的解析器，
// 结果就是同一个文件在刮削里认成剧、在整理里认成电影。
package organize

import (
	"fmt"
	"path"
	"regexp"
	"strings"

	"strm115/internal/scrape"
	"strm115/internal/strm"
)

// 识别结果分类。
const (
	KindMovie   = "movie"
	KindSeries  = "series"
	KindUnknown = "unknown"
)

// Rule 是一条整理规则：从哪个网盘目录搬，搬进哪两个库。
//
// 一个库同时挂 115 和夸克之后，「从哪来」就不止一处了：115 的待整理目录、
// 夸克的转存目录各自要有一套来源与目标。以前这些字段直接长在 Config 上，
// 只能配一条；现在拆成列表，Config 里保留同名字段只为兼容老配置。
type Rule struct {
	// ID 是前端生成的稳定标识，用于「只跑这条」和删除时对号。
	ID string `json:"id"`
	// Name 是显示名，留空时界面用源目录名。
	Name string `json:"name,omitempty"`
	// Enabled 为 nil 表示启用；指针是为了让老配置（没有这个字段）照旧工作。
	Enabled *bool `json:"enabled,omitempty"`
	// AccountID 是源目录所在的网盘账号。
	AccountID int64 `json:"account_id"`
	// SourceCID / SourceName 是「待整理」目录。
	SourceCID  string `json:"source_cid"`
	SourceName string `json:"source_name"`
	// MovieLibID / TVLibID 是目标媒体库。用库而不是裸目录，
	// 因为整理完要触发的同步、要写的 .strm 都是按库走的。
	MovieLibID int64 `json:"movie_lib_id"`
	TVLibID    int64 `json:"tv_lib_id"`
	// TargetCID / TargetName 是「搬进库里的哪个目录」，留空表示库的根目录。
	//
	// 需要它是因为库可以是一个混合目录：库根是 `/影视`、下面再分
	// `/影视/电影` 与 `/影视/剧集`，而 Emby 的两个库分别扫这两层。
	// 没有这个字段时，电影会被搬到 `/影视/片名`，落在两个 Emby 库的外面。
	TargetCID  string `json:"target_cid,omitempty"`
	TargetName string `json:"target_name,omitempty"`
}

// On 判断这条规则是否启用（缺省启用）。
func (r Rule) On() bool { return r.Enabled == nil || *r.Enabled }

// Label 给界面/日志用的名字。
func (r Rule) Label() string {
	if strings.TrimSpace(r.Name) != "" {
		return r.Name
	}
	if strings.TrimSpace(r.SourceName) != "" {
		return r.SourceName
	}
	return r.SourceCID
}

// Valid 判断这条规则能不能跑：有源目录、至少一个目标库。
func (r Rule) Valid() bool {
	return strings.TrimSpace(r.SourceCID) != "" && (r.MovieLibID != 0 || r.TVLibID != 0)
}

// Config 是自动整理的运行参数，存在 settings 的 organize_config 里。
type Config struct {
	Enabled bool `json:"enabled"`
	// Rules 是全部整理规则。为空时由下面那组同名字段合成一条，
	// 老配置（v0.2.8 及以前）不需要迁移数据就能继续跑。
	Rules []Rule `json:"rules,omitempty"`
	// AccountID 源目录所在的 115 账号；0 表示用第一个账号。
	AccountID int64 `json:"account_id"`
	// SourceCID / SourceName 是「待整理」目录。
	SourceCID  string `json:"source_cid"`
	SourceName string `json:"source_name"`
	// MovieLibID / TVLibID 是目标媒体库。用库而不是裸目录，
	// 因为整理完要触发的同步、要写的 .strm 都是按库走的。
	MovieLibID int64 `json:"movie_lib_id"`
	TVLibID    int64 `json:"tv_lib_id"`
	// Batch 一轮最多处理多少条。115 有风控，一次几百个移动必被限。
	Batch int `json:"batch"`
	// MinSizeMB 小于这个体积的文件跳过（样片、花絮、字幕包）。
	MinSizeMB int64 `json:"min_size_mb"`
	// SyncAfter 整理完自动同步受影响的媒体库，让 Emby 立刻能看到。
	SyncAfter bool `json:"sync_after"`
	// AutoIngest 入库后自动整理：TG 转存 / 机器人离线 / 订阅下载一落盘，
	// 就自动把「资源保存目录」里的新条目搬进媒体库，接着同步、刮削。
	// 用指针是为了区分「老配置里没这个字段」（当作开）和「用户主动关掉」。
	AutoIngest *bool `json:"auto_ingest"`
	// VerifyTMDB 表示「TMDB 找得到才搬」。
	//
	// 识别只看文件名，认得出「像个片名」并不等于真能刮到元数据：广告壳、
	// 合集目录、名字对不上的枪版都会被认成电影搬进库里，然后以一张
	// 没有海报的灰卡片躺在 Emby 里。开着这个开关时，搬之前先问一次 TMDB，
	// 认不出来的就原样留在暂存目录（/StrmStation），等人工处理。
	// 网络错误不算「认不出」：TMDB 挂了不该把整理整条停掉。
	VerifyTMDB *bool `json:"verify_tmdb"`
}

// AutoIngestOn 判断「入库后自动整理」是否开着。老配置没这个字段时默认开：
// 这条链本来就是整理功能最该有的样子，不默认开等于新用户永远踩不到。
func (c Config) AutoIngestOn() bool {
	return c.AutoIngest == nil || *c.AutoIngest
}

// VerifyTMDBOn 判断「TMDB 找得到才搬」是否开着。老配置默认开：
// 库里躺一张没海报的灰卡片，比暂存目录里多一个待处理文件夹难收拾得多。
func (c Config) VerifyTMDBOn() bool {
	return c.VerifyTMDB == nil || *c.VerifyTMDB
}

// Filled 补齐默认值。
func (c Config) Filled() Config {
	if c.Batch <= 0 {
		c.Batch = 30
	}
	if c.Batch > 200 {
		c.Batch = 200
	}
	if c.MinSizeMB < 0 {
		c.MinSizeMB = 0
	}
	if c.AutoIngest == nil {
		on := true
		c.AutoIngest = &on
	}
	if c.VerifyTMDB == nil {
		on := true
		c.VerifyTMDB = &on
	}
	c.Rules = c.normalizedRules()
	// 老字段跟上第一条规则：入库后自动整理、重试这些老路径还在读它们。
	if len(c.Rules) > 0 {
		r := c.Rules[0]
		c.AccountID = r.AccountID
		c.SourceCID = r.SourceCID
		c.SourceName = r.SourceName
		c.MovieLibID = r.MovieLibID
		c.TVLibID = r.TVLibID
	}
	return c
}

// normalizedRules 清掉空规则、补 ID，并在没有规则时用老字段合成一条。
//
// 「空规则」是前端加了一行却没选目录就保存留下的：留着它会让
// Ready() 误判成「配置好了」，实际一条都跑不动。
func (c Config) normalizedRules() []Rule {
	out := make([]Rule, 0, len(c.Rules)+1)
	for i, r := range c.Rules {
		r.Name = strings.TrimSpace(r.Name)
		r.SourceCID = strings.TrimSpace(r.SourceCID)
		r.SourceName = strings.TrimSpace(r.SourceName)
		if r.SourceCID == "" && r.MovieLibID == 0 && r.TVLibID == 0 {
			continue
		}
		if r.ID == "" {
			r.ID = fmt.Sprintf("r%d", i+1)
		}
		out = append(out, r)
	}
	if len(out) == 0 && strings.TrimSpace(c.SourceCID) != "" {
		out = append(out, Rule{
			ID:         "r1",
			AccountID:  c.AccountID,
			SourceCID:  c.SourceCID,
			SourceName: c.SourceName,
			MovieLibID: c.MovieLibID,
			TVLibID:    c.TVLibID,
		})
	}
	return out
}

// ActiveRules 返回真正能跑的规则（启用 + 有源目录 + 有目标库）。
func (c Config) ActiveRules() []Rule {
	rules := c.Rules
	if len(rules) == 0 {
		// 老配置只有同名字段：这里现合成一条，省掉一次数据迁移。
		rules = c.normalizedRules()
	}
	out := make([]Rule, 0, len(rules))
	for _, r := range rules {
		if r.On() && r.Valid() {
			out = append(out, r)
		}
	}
	return out
}

// RuleByID 按 id 取规则。
func (c Config) RuleByID(id string) (Rule, bool) {
	rules := c.Rules
	if len(rules) == 0 {
		rules = c.normalizedRules()
	}
	for _, r := range rules {
		if r.ID == id {
			return r, true
		}
	}
	return Rule{}, false
}

// Ready 判断配置是否够跑一轮：至少一条启用的规则。
func (c Config) Ready() error {
	if len(c.ActiveRules()) == 0 {
		if len(c.Rules) == 0 {
			return fmt.Errorf("还没配置整理规则：先选「待整理」目录和目标媒体库")
		}
		return fmt.Errorf("没有一条规则是完整的：每条都要选源目录，并至少选一个目标媒体库（电影 / 剧集）")
	}
	return nil
}

// Entry 是网盘条目的最小信息。刻意不直接用 pan.Entry：
// 规则层不该依赖网盘客户端，测试里也不用造一个假的 115。
type Entry struct {
	ID    string `json:"file_id"`
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// Item 是一个条目的识别结果 + 落位计划。
type Item struct {
	Entry
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Year       string `json:"year"`
	Season     int    `json:"season"`
	Episode    int    `json:"episode"`
	Confidence int    `json:"confidence"`
	Reason     string `json:"reason"`
	// Dir 是目标目录的相对路径（相对媒体库根），File 是移动后的文件名。
	// 目录条目（IsDir）的 File 为空：搬的是整个目录。
	Dir    string `json:"dir"`
	File   string `json:"file"`
	Target string `json:"target"`
	// Lib 由调用方补（电影库 / 剧集库的名字），规则层不知道库叫什么。
	Lib string `json:"lib,omitempty"`
	// Skip 非空表示这条不该动，值是给用户看的原因。
	Skip string `json:"skip,omitempty"`
}

// videoExt 是会被整理的文件扩展名。字幕 / nfo / 图片 / 种子不搬：
// 它们要么跟着视频走（在目录里），要么根本不该进媒体库。
var videoExt = map[string]bool{
	".mkv": true, ".mp4": true, ".avi": true, ".ts": true, ".m2ts": true,
	".rmvb": true, ".rm": true, ".wmv": true, ".mov": true, ".flv": true,
	".webm": true, ".mpg": true, ".mpeg": true, ".iso": true, ".m4v": true,
}

// IsVideo 判断文件名是不是视频。
func IsVideo(name string) bool {
	return videoExt[strings.ToLower(path.Ext(name))]
}

// Recognize 识别一个条目并算出落位。
//
// minSizeMB 对文件与目录一视同仁：115 给目录返回的也是递归体积，
// 只有 Size==0（网盘没给值）时才不参与判断。
func Recognize(e Entry, minSizeMB int64) Item {
	it := Item{Entry: e}

	// 非视频文件直接跳过。扫描结果里仍然列出来，标红让用户知道
	// 「这个没被整理」是因为它不是视频，而不是因为程序瞎了。
	if !e.IsDir && !IsVideo(e.Name) {
		it.Skip = "不是视频文件"
		it.Kind = KindUnknown
		it.Reason = "扩展名 " + strings.ToLower(path.Ext(e.Name))
		return it
	}
	// 发布组塞的广告视频（`【更多无水印高品质资源请访问 www.xxx.com】.mkv`）
	// 既刮不到元数据、也不该进媒体库。体积门槛常常拦不住它们：4K 广告也有几百 MB。
	if !e.IsDir && strm.IsAd(e.Name) {
		it.Skip = "广告文件"
		it.Kind = KindUnknown
		it.Reason = "名字是发布组的宣传语"
		return it
	}
	// 体积门槛对目录同样适用：115 给目录返回的是**递归**体积，
	// 「空枪 (2026)」这种只有一个几百 KB 广告壳的目录正是靠它拦下来的。
	// Size==0 表示网盘没给这个值（老接口 / 统计未完成），此时不能当成
	// 「小于门槛」——否则一整季显示 0 B 的目录会被误杀。
	if minSizeMB > 0 && e.Size > 0 && e.Size < minSizeMB<<20 {
		it.Skip = fmt.Sprintf("小于 %d MB", minSizeMB)
		it.Kind = KindUnknown
		it.Reason = "体积 " + humanSize(e.Size)
		return it
	}

	// 目录要当成「一个带通用文件名的路径」交给 Parse：季目录的识别逻辑
	// （`第2季` / `Season 2`）只在路径的**目录段**上生效，直接拿目录名
	// 当文件名传进去，`某剧 第1季` 会被认成一部叫「某剧 第1季」的电影。
	parseName := e.Name
	if e.IsDir {
		parseName = e.Name + "/movie.mkv"
	}
	// 中文发布名几乎都用全角括号：`片名（2025）4K`。Parse 只认半角，
	// 全角会被当成片名的一部分，落位就成了「片名（ ）4K (2025)」。
	// 这里只归一化分隔符，不改动片名本身的字。
	p := scrape.Parse(normalizeName(parseName))
	title := strings.TrimSpace(p.Title)
	it.Title = title
	it.Year = strings.TrimSpace(p.Year)
	it.Season = p.Season
	it.Episode = p.Episode

	if title == "" {
		it.Kind = KindUnknown
		it.Skip = "认不出片名"
		it.Reason = "文件名既不像电影也不像剧集"
		return it
	}

	// 动漫没有独立的库：番剧在 TMDB 里就是 tv，落位和剧集完全一样。
	// 只靠集号识别会漏掉「某番 合集」这种目录名里没有 S/E 的条目，
	// 所以名字里带番剧标记时也按剧集处理 —— 反过来的错误（把一部电影
	// 认成剧集）会让它落进 Season 目录，比留在暂存目录更糟，因此这里的
	// 关键词只收明确指向连载番剧的词，不收「动画」这种可能出现在片名里的。
	anime := reAnime.MatchString(e.Name)
	if p.Kind == scrape.KindSeries || p.HasEpisode || p.Season > 0 || anime {
		it.Kind = KindSeries
		// Parse 保留了「第1季」「S02」这类标记（刮削时需要它来分季），
		// 但目录名里不能带着——否则同一部剧的第 1、2 季会落进
		// 「某剧 第1季」「某剧 第2季」两个目录，Emby 里就是两部剧。
		// 「新番」「日番」同理：它是发布组的分类词，不是片名。
		title = stripSeason(title)
		if anime {
			title = stripAnime(title)
		}
		it.Title = title
		// 只有集号、没有季号（`Show Episode 199` / `某剧 第08集`）：Emby 的目录
		// 结构是「剧名/Season N/文件」，季号缺省就是第一季。留在 0 的话文件会
		// 落在剧名根目录，媒体库里没有季分组，后续按季刮削也对不上。
		if it.Episode > 0 && it.Season == 0 {
			it.Season = 1
		}
	} else {
		it.Kind = KindMovie
	}

	// 片名里残留的空括号 / 尾部画质词（`( )4K`、`2160p`、`REMUX`）会原样
	// 进目录名，TMDB 那边也更难对上。清一遍，清空了就退回原标题。
	if cleaned := cleanTitle(title); cleaned != "" {
		title = cleaned
		it.Title = cleaned
	}

	// 把握度：年份是最强的信号（`沙丘2.2024` 里 2024 几乎不会是噪音），
	// 季集次之；IMDb ID 直接给满。
	conf := 55
	// reason 是「凭什么认成这样」，不是把片名再抄一遍——片名在界面上有
	// 自己的字段，重复一遍只会把表格撑成四行。
	facts := []string{}
	if it.Year != "" {
		conf += 25
		facts = append(facts, "年份 "+it.Year)
	} else {
		facts = append(facts, "没年份")
	}
	if anime && it.Kind == KindSeries {
		facts = append(facts, "番剧名标记")
	}
	if it.Kind == KindSeries {
		switch {
		case it.Episode > 0:
			conf += 15
			facts = append(facts, fmt.Sprintf("S%02dE%02d", it.Season, it.Episode))
		case it.Season > 0:
			conf += 8
			facts = append(facts, fmt.Sprintf("第 %d 季", it.Season))
		}
	} else {
		conf += 5
	}
	if p.IMDbID != "" {
		conf += 10
		facts = append(facts, p.IMDbID)
	}
	if conf > 99 {
		conf = 99
	}
	it.Confidence = conf
	// 目录没有季集信息时不能给高把握：一个叫「某剧.2022」的目录可能是剧，
	// 也可能是电影合集，只看名字分不出来。此时压到 65 让界面默认不勾选，
	// 由用户确认——把一整部剧搬进电影库，是这一页最贵的错误。
	if e.IsDir && it.Season == 0 && it.Episode == 0 && it.Confidence > 65 {
		it.Confidence = 65
		facts = append(facts, "目录无季集信息，类型需确认")
	}
	if len(facts) == 0 {
		facts = append(facts, "只有片名")
	}
	it.Reason = strings.Join(facts, " · ")

	// 落位
	base := safeName(title)
	if it.Year != "" {
		base += " (" + it.Year + ")"
	}
	switch it.Kind {
	case KindSeries:
		if e.IsDir {
			// 目录本来就带着季结构（某剧 第1季/…），再套一层 Season 1
			// 会变成 Season 1/第1季/第01集.mkv，Emby 认不出。
			it.Dir = base
			it.File = ""
		} else {
			it.Dir = base
			if it.Season > 0 {
				it.Dir = path.Join(base, fmt.Sprintf("Season %d", it.Season))
			}
			it.File = safeFileName(e.Name)
		}
	default:
		it.Dir = base
		if e.IsDir {
			it.File = ""
		} else {
			it.File = safeFileName(e.Name)
		}
	}
	it.Target = it.Dir
	if it.File != "" {
		it.Target = path.Join(it.Dir, it.File)
	}
	return it
}

// NeedLib 返回这条该进哪个库（movie / series）。
func (it Item) NeedLib() string {
	if it.Kind == KindSeries {
		return KindSeries
	}
	return KindMovie
}

var (
	// 季标记有两种写法：第1季 / 第一季。不认汉字的话，
	// 「最后生还者 第一季」会被当成一部叫这个名字的电影。
	reTitleSeasonCN = regexp.MustCompile(`第\s*(?:[0-9]{1,2}|[一二三四五六七八九十]{1,3})\s*季`)
	reTitleSeasonEN = regexp.MustCompile(`(?i)\b(?:season|series|s)\s*\d{1,2}\b`)
	reTitleSep      = regexp.MustCompile(`[\s._\-\[\]()（）]+`)
	// reAnime 认「这是番剧」的名字标记。刻意不收「动画 / 动漫」：
	// 它们经常出现在片名或发布组名里（「动画版」「动漫电影合集」），
	// 拿它当剧集信号会把电影搬进 Season 目录。
	reAnime = regexp.MustCompile(`(?i)(?:日番|新番|番剧|连载|合集番|\bova\b|\boad\b|\banime\b)`)
)

// stripSeason 去掉片名里的季标记并清掉残留分隔符。
func stripSeason(title string) string {
	out := reTitleSeasonCN.ReplaceAllString(title, " ")
	out = reTitleSeasonEN.ReplaceAllString(out, " ")
	out = reTitleSep.ReplaceAllString(out, " ")
	out = strings.TrimSpace(out)
	if out == "" {
		return title
	}
	return out
}

// stripAnime 去掉片名里的番剧分类词（新番 / 日番 / OVA …）。
//
// 这些词只是发布组的分类标签，留在目录名里会让 TMDB 搜不到：
// 「葬送的芙莉莲 新番」不是任何一部作品的名字，去 TMDB 只会空手而归。
func stripAnime(title string) string {
	out := reAnime.ReplaceAllString(title, " ")
	out = reTitleSep.ReplaceAllString(out, " ")
	out = strings.TrimSpace(out)
	if out == "" {
		return title
	}
	return out
}

// normalizeName 把中文发布名里的全角分隔符换成半角。
//
// 只动「分隔用途」的字符：括号、方括号、竖线。`丨` 在片名里是排版符号
// （`狂丨飙丨飞丨车`），直接删掉才能拼回原名。
func normalizeName(s string) string {
	r := strings.NewReplacer(
		"（", "(", "）", ")", "【", "[", "】", "]", "〔", "(", "〕", ")",
		"丨", "", "｜", "", "｜", "",
	)
	return r.Replace(s)
}

var (
	// 空括号：`( )` / `[]` —— 年份被 Parse 拿走之后剩下的壳。
	reEmptyBracket = regexp.MustCompile(`[\(\[【]\s*[\)\]】]`)
	// 尾部画质词：`... 4K`、`... REMUX`、`... 2160p`。
	reQualityTail = regexp.MustCompile(`(?i)[\s._\-]*(?:4k|8k|2160p|1080p|720p|uhd|hdr10\+?|hdr|dovi|dv|remux|bluray|blu-ray|web-?dl|h\.?264|h\.?265|hevc|x265|x264)$`)
)

// cleanTitle 去掉空括号与尾部画质词。
//
// 这两样都不是片名的一部分，留在目录名里只会有两个后果：Emby 里显示成
// 「沙丘2 (2024) 4K」，以及 TMDB 按这个名字搜不到。
func cleanTitle(s string) string {
	s = reEmptyBracket.ReplaceAllString(s, " ")
	for i := 0; i < 4; i++ {
		next := reQualityTail.ReplaceAllString(strings.TrimSpace(s), "")
		if next == s {
			break
		}
		s = next
	}
	s = reTitleSep.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// safeName 清理成 115 里能用的目录名。
//
// 115 的目录名不能带 / 与 \，也不该以点结尾；片名里偶尔会出现
// `:` `?` 这类字符，保留它们没问题（115 允许），只清真正会破坏路径的。
func safeName(s string) string {
	s = strings.TrimSpace(s)
	r := strings.NewReplacer("/", "／", "\\", "＼", "\x00", "")
	s = r.Replace(s)
	s = strings.Trim(s, ". ")
	if s == "" {
		s = "未命名"
	}
	return truncateRunes(s, 80)
}

// safeFileName 保留扩展名地清理文件名。
func safeFileName(name string) string {
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	return safeName(stem) + ext
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max])
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
