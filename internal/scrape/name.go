// Package scrape 把「网盘目录里的文件名」变成「一份能用的影视元数据」。
//
// 两条硬规矩：
//
//  1. **只读台账，不重新遍历网盘。** 视频清单来自 files 表（同步时已经落库），
//     刮削只做本地解析 + 查 IMDb。少一次全盘遍历就少一次被 115 风控的机会。
//  2. **查到的元数据一定落库。** 同一部剧的 24 集只查一次 IMDb，第二次刮削
//     零网络请求。这不是性能优化，是账号安全问题——反复拉 IMDb 会被限流。
//
// 解析失败（文件名里没有可用的片名、或 IMDb 匹配不到）时宁可留空，
// 也不猜：错误的元数据会污染 Emby，而且比没有更难发现。
package scrape

import (
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Kind 是影视类别。
const (
	KindMovie  = "movie"
	KindSeries = "series"
)

// Parsed 是一条视频从路径里解析出来的信息。
type Parsed struct {
	Title   string `json:"title"`
	Year    string `json:"year"`
	Kind    string `json:"kind"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	// IMDbID 是从文件名/目录名里直接读到的 IMDb ID（`tt15239678`）。
	//
	// 有它就不用搜片名：一次请求拿到元数据，既不赌匹配准确率，
	// 也少一次会被上游计数的搜索请求。
	IMDbID string `json:"imdb_id,omitempty"`
	// Hint 记录片名是从哪一级拿到的（dir:<名字> / file），排查误判时有用。
	Hint string `json:"hint"`
	// Episode 表示这是一个明确带集号的视频。
	HasEpisode bool `json:"has_episode"`
}

// 季集号有两种写法：阿拉伯数字，以及汉字（第一季、第八集、第二十三集）。
//
// 只认 \d 的时候，「某剧 第一季」这种中文圈最常见的写法会解析失败：
// 季号进不了 Season，整段「第一季」反而被当成片名的一部分留下来，
// 于是这部剧在刮削里搜不到、在整理里被归成电影。
const (
	cnNumPat = `[0-9]{1,2}|[一二三四五六七八九十]{1,3}`
	cnEpPat  = `[0-9]{1,4}|[一二三四五六七八九十]{1,3}`
)

// 季集识别的几种常见写法。顺序有意义：先认最明确的 SxxExx。
var (
	reSeasonEpisode = regexp.MustCompile(`(?i)\bS(\d{1,2})[\s._-]*E(\d{1,4})\b`)
	reSeasonEpAlt   = regexp.MustCompile(`(?i)\b(\d{1,2})x(\d{2,4})\b`)
	reCNSeasonEp    = regexp.MustCompile(`第\s*(` + cnNumPat + `)\s*季[\s._-]*第?\s*(` + cnEpPat + `)\s*[集话話]`)
	reCNEpisode     = regexp.MustCompile(`第\s*(` + cnEpPat + `)\s*[集话話]`)
	reSeasonWord    = regexp.MustCompile(`(?i)(?:^|[\s._\[\(-])(?:season|series|s)\s*(\d{1,2})(?:[\s._\]\)-]|$)`)
	reSeasonCN      = regexp.MustCompile(`第\s*(` + cnNumPat + `)\s*季`)
	// 「E05」「EP05」「Episode 5」「Episode.5」：分隔符可能是空格、点、下划线或横杠。
	reEpisodeWord = regexp.MustCompile(`(?i)\bE(?:P|pisode)?[\s._-]*(\d{1,4})\b`)
	reEP          = regexp.MustCompile(`(?i)\bEP?(\d{1,4})\b`)
)

var (
	reYearParen = regexp.MustCompile(`[\(\[]\s*((?:19|20)\d{2})\s*[\)\]]`)

	// reTechMark 找到发布名里「技术参数」的起点，从那里一刀切掉后面全部。
	//
	// 比逐个剔除标记可靠：发布名永远是 `片名.年份.分辨率.编码.音轨-组` 这个顺序，
	// 找到第一个技术词，后面就不可能是片名的一部分。
	reTechMark = regexp.MustCompile(`(?i)(?:^|[\s._\-\[\(])(` +
		`\d{3,4}[pi]|4k|8k|uhd|hdrip|web[\s._-]?dl|webrip|webrip|blu[\s._-]?ray|bluray|` +
		`bdrip|brrip|bdremux|remux|hdtv|dvdrip|dvdscr|hdcam|tsrip|x264|x265|h\.?264|h\.?265|` +
		`hevc|avc|av1|xvid|divx|vp9|` +
		`aac|ac3|eac3|dts|truehd|atmos|flac|opus|mp3|` +
		`\d{1,2}bit|hdr10\+?|hdr|sdr|dovi|do\.?vi|` +
		`repack|proper|rerip|extended|unrated|uncut|remastered|director'?s[\s._-]?cut|imax|` +
		`internal|limited|complete|multi|dual[\s._-]?audio|hybrid)`)

	// 中文圈常见的后缀标记，出现在片名后面但同样不属于片名。
	reCNJunk = regexp.MustCompile(`(国语|粤语|国粤|国英|中英|中字|简繁|简体|繁体|双语|` +
		`内嵌|外挂|无水印|高清|超清|蓝光|原盘|收藏版|完整版|未删减|导演剪辑版|` +
		`国配|台配|港配|双语字幕|中文字幕|特效字幕)`)

	// 发布组尾缀。`-` 前面必须是分隔符：Spider-Man 的连字符是片名的一部分，
	// 没有这个限制会把它切成 Spider。
	reGroupTail = regexp.MustCompile(`(?:^|[\s.])-[A-Za-z0-9@._]{2,20}$`)
	reBrackets  = regexp.MustCompile(`[\[\{][^\]\}]{0,40}[\]\}]`)

	// 全角括号是中文发布名最常见的「广告位」：
	// `【高清影视之家发布 www.BBQDDQ.com】空枪`。只认 [] {} 的时候，
	// 这整段广告会进片名，整理出来就是个
	// `【高清影视之家发布 www BBQDDQ com】空枪 (2026)` 的目录。
	reFullBrackets = regexp.MustCompile(`【[^】]{0,80}】|〖[^〗]{0,80}〗|〔[^〕]{0,80}〕|「[^」]{0,60}」`)

	// 裸写在名字里的站点域名：`www.BBQDDQ.com` / `HDSKY.me`。
	// 只吃「词.顶级域」这种形状，`Dune.Part.Two` 不会被误伤（Part/Two 不是 TLD）。
	reSiteDomain = regexp.MustCompile(`(?i)\b(?:www\.)?[a-z0-9][a-z0-9-]{1,30}\.(?:com|net|org|cc|tv|me|xyz|top|vip|info|cn|la|io|co)\b`)

	// 结尾的版本孤字：`空枪.V.2026.2160p` 洗完只剩一个 V。
	// 尾部允许残留分隔符：`空枪.V.2026` 切掉年份后还剩 `空枪.V.`，
	// 只认「V 就在结尾」的话这个 V 会留在片名里。
	reVersionTail = regexp.MustCompile(`(?i)(?:^|[\s._-])v\d{0,2}[\s._-]*$`)

	// 文件名里最常见的 IMDb ID 写法：`{imdb-tt15239678}`、`[tt15239678]`、
	// `Dune.2024.tt15239678.2160p`。ID 本身固定是 tt + 7~10 位数字。
	reIMDbID = regexp.MustCompile(`(?i)\btt(\d{7,10})\b`)
)

// 这些目录名是「分类/容器」，不是片名。扫描到它们要往外一层找。
var genericDirs = map[string]bool{}

func init() {
	for _, s := range []string{
		"movie", "movies", "film", "films", "cinema", "video", "videos",
		"tv", "tvshow", "tvshows", "tv shows", "series", "show", "shows",
		"anime", "animation", "documentary", "documentaries", "variety",
		"download", "downloads", "media", "video", "new", "old", "other", "others",
		"movie library", "tv library", "电影", "电影库", "影片", "影视", "电视剧",
		"剧集", "美剧", "英剧", "日剧", "韩剧", "国产剧", "港剧", "台剧", "动漫",
		"动画", "动画片", "纪录片", "综艺", "综艺节目", "下载", "已整理", "待整理",
		"未整理", "收藏", "分类", "其他", "其它", "华语", "欧美", "日韩", "港台",
		"动作", "喜剧", "爱情", "科幻", "恐怖", "惊悚", "悬疑", "犯罪", "剧情",
		"战争", "动画", "冒险", "奇幻", "武侠", "古装", "历史", "传记", "音乐",
		"歌舞", "家庭", "儿童", "运动", "灾难", "纪录", "短片", "4k", "uhd",
		"1080p", "2160p", "蓝光", "bluray", "hdr", "remux", "原盘", "电影频道",
	} {
		genericDirs[strings.ToLower(s)] = true
	}
}

// 季目录：Season 01 / S01 / 第一季 / 第1季 / Specials
var (
	reSeasonDir = regexp.MustCompile(`(?i)^(?:season[\s._-]*\d{1,2}|s\d{1,2}|specials?|` +
		`第\s*(?:` + cnNumPat + `)\s*季|特别篇|番外)$`)
	reDiscDir = regexp.MustCompile(`(?i)^(?:cd|disc|disk|dvd|part)\s*\d{1,2}$`)
)

// Parse 从「相对媒体库根目录的路径」解析影视信息。
//
// relPath 用 "/" 分隔，形如：
//
//	电影/Dune.Part.Two.2024.2160p.mkv
//	电影/沙丘2 (2024)/Dune.Part.Two.2024.mkv
//	剧集/Breaking.Bad/Season 01/Breaking.Bad.S01E01.1080p.mkv
//
// 返回 HasEpisode=false 且 Title 为空表示「认不出来」，调用方应当跳过它。
func Parse(relPath string) Parsed {
	relPath = strings.ReplaceAll(relPath, "\\", "/")
	relPath = strings.TrimPrefix(relPath, "/")
	parts := strings.Split(relPath, "/")
	if len(parts) == 0 {
		return Parsed{}
	}

	fileName := parts[len(parts)-1]
	dirs := parts[:len(parts)-1]
	stem := strings.TrimSuffix(fileName, path.Ext(fileName))

	var out Parsed

	// —— 0. IMDb ID：文件名优先，其次由内向外找目录 ——
	//
	// 这是最省请求的一条路：有了 ID 就直接查详情，不再按片名搜索。
	// 注意要单独存着：下面找片名时会整体覆盖 out（cleanName 返回的是新值）。
	imdbID := findIMDbID(fileName)
	if imdbID == "" {
		for i := len(dirs) - 1; i >= 0; i-- {
			if id := findIMDbID(dirs[i]); id != "" {
				imdbID = id
				break
			}
		}
	}

	// —— 1. 季集：先看文件名，再看目录 ——
	season, episode, hasEp := findEpisode(stem)
	seasonFromDir := 0
	if !hasEp || season == 0 {
		// 目录里可能只有季号（.../Season 2/Some.Episode.mkv），集号在文件名里
		for i := len(dirs) - 1; i >= 0; i-- {
			if s, e, ok := findEpisode(dirs[i]); ok {
				// 文件名只给了集号（第08集）：季号从目录补上
				if season == 0 {
					season = s
				}
				if episode == 0 {
					episode = e
				}
				hasEp = true
				break
			}
			if s, ok := findSeasonOnly(dirs[i]); ok && seasonFromDir == 0 {
				seasonFromDir = s
			}
		}
	}

	// —— 2. 片名候选：从最内层目录向外找第一个不是分类/季目录的 ——
	for i := len(dirs) - 1; i >= 0; i-- {
		dir := strings.TrimSpace(dirs[i])
		if dir == "" || isSeasonDir(dir) || isGenericDir(dir) || reDiscDir.MatchString(dir) {
			continue
		}
		p := cleanName(dir)
		if p.Title == "" {
			continue
		}
		// 目录里能拿到年份的优先：`沙丘2 (2024)` 比文件名 `Dune.Part.Two.2024.2160p` 更准
		if p.Year != "" {
			out = p
			out.Hint = "dir:" + dir
			break
		}
		if out.Title == "" {
			out = p
			out.Hint = "dir:" + dir
		}
	}

	// —— 3. 文件名兜底（同时补上目录里缺的年份）——
	// `电影/movie.mkv` 这种文件名就是通用词，不能拿来当片名。
	if !isGenericDir(stem) {
		if fp := cleanName(stem); fp.Title != "" {
			if out.Title == "" {
				out = fp
				out.Hint = "file"
			} else if out.Year == "" && fp.Year != "" {
				out.Year = fp.Year
			}
		}
	}

	if hasEp && season == 0 && seasonFromDir > 0 {
		season = seasonFromDir
	}
	// 季号哪里都没写（`某剧/… Episode 199.mkv`、整理后落在剧名根目录的文件）：
	// 按第一季算。Emby 对剧集根目录下的文件也是这么处理的；留 0 的话 nfo 会写成
	// <season>0</season>，在 Emby 里就变成「特别篇」，和它自己的解析结果打架。
	if hasEp && season == 0 {
		season = 1
	}

	if hasEp {
		out.Kind = KindSeries
		out.Season = season
		out.Episode = episode
		out.HasEpisode = true
	} else {
		// 目录里只有 Season 2 而文件名没有集号：仍按剧集处理，
		// 集号留 0，交给后续按文件名里的 `EP05` 之类再认一次。
		if seasonFromDir > 0 {
			out.Kind = KindSeries
			out.Season = seasonFromDir
			out.HasEpisode = true
		} else {
			out.Kind = KindMovie
		}
	}

	out.Title = strings.TrimSpace(out.Title)
	out.IMDbID = imdbID
	return out
}

// isSeasonDir 判断目录名是不是季目录。
func isSeasonDir(name string) bool {
	return reSeasonDir.MatchString(strings.TrimSpace(name))
}

func isGenericDir(name string) bool {
	return genericDirs[strings.ToLower(strings.TrimSpace(name))]
}

// findEpisode 从一段文本里认季集号。
func findEpisode(s string) (season, episode int, ok bool) {
	if m := reSeasonEpisode.FindStringSubmatch(s); m != nil {
		return atoi(m[1]), atoi(m[2]), true
	}
	if m := reCNSeasonEp.FindStringSubmatch(s); m != nil {
		return num(m[1]), num(m[2]), true
	}
	if m := reSeasonEpAlt.FindStringSubmatch(s); m != nil {
		return atoi(m[1]), atoi(m[2]), true
	}
	// 只有集号：`Show.EP05.1080p`
	if m := reCNEpisode.FindStringSubmatch(s); m != nil {
		return 0, num(m[1]), true
	}
	if m := reEpisodeWord.FindStringSubmatch(s); m != nil {
		return 0, atoi(m[1]), true
	}
	if m := reEP.FindStringSubmatch(s); m != nil {
		return 0, atoi(m[1]), true
	}
	return 0, 0, false
}

func findSeasonOnly(s string) (int, bool) {
	if m := reSeasonWord.FindStringSubmatch(s); m != nil {
		return atoi(m[1]), true
	}
	if m := reSeasonCN.FindStringSubmatch(s); m != nil {
		return num(m[1]), true
	}
	return 0, false
}

// cleanName 把一段名字洗成「片名 + 年份」。
//
// 步骤和人的直觉一致：先剥掉季集号（它不是片名的一部分），再砍掉发布参数，
// 再拆出年份，剩下的就是片名。
func cleanName(s string) Parsed {
	var out Parsed
	if s == "" {
		return out
	}

	// 年份要从原文里找：`(2024)` 两侧的括号在切词时会丢
	if m := reYearParen.FindStringSubmatch(s); m != nil {
		out.Year = m[1]
	}

	work := s

	// 方括号/花括号整块去掉：`[压制组]`、`[1080p]`、`{tmdb-123}`
	work = reBrackets.ReplaceAllString(work, " ")

	// 中文发布名的广告位：【高清影视之家发布 www.xxx.com】、〖xx字幕组〗
	work = reFullBrackets.ReplaceAllString(work, " ")
	work = reSiteDomain.ReplaceAllString(work, " ")

	// 裸写在文件名里的 IMDb ID 不属于片名。
	work = reIMDbID.ReplaceAllString(work, " ")

	// 季集号不参与片名
	work = reSeasonEpisode.ReplaceAllString(work, " ")
	work = reCNSeasonEp.ReplaceAllString(work, " ")
	work = reSeasonEpAlt.ReplaceAllString(work, " ")
	// 单独的「第N季」（没有集号的目录名）：不同步剥掉的话，
	// 每季都会因为片名里带着「第一季」而被当成另一部剧。
	work = reSeasonCN.ReplaceAllString(work, " ")
	// 只有集号的几种写法也必须剥：findEpisode 认得出来，但名字里留着就会
	// 变成片名的一部分 —— `A Record of a Mortal’s Journey to Immortality
	// Episode 199` 去 TMDB 是搜不到的（整理时会被判成「认不出」而跳过）。
	// 中文的「第199集」同理。
	work = reCNEpisode.ReplaceAllString(work, " ")
	work = reEpisodeWord.ReplaceAllString(work, " ")
	work = reEP.ReplaceAllString(work, " ")

	// 从第一个技术参数开始整段丢弃
	if loc := reTechMark.FindStringIndex(work); loc != nil {
		work = work[:loc[0]]
	}

	work = reCNJunk.ReplaceAllString(work, " ")

	// 年份：先按括号拿，再按独立词拿。
	//
	// 独立词要取**最靠右**的那个：发布名是「片名.年份.技术参数」，
	// 而片名本身可能就是个年份（1917、2012）。取第一个会把《1917》认成 1917 年的片子。
	if out.Year == "" {
		if y, idx := lastYear(work); y != "" {
			out.Year = y
			work = work[:idx] + " " + work[idx+len(y):]
		}
	} else {
		work = strings.Replace(work, out.Year, " ", 1)
	}
	// 括号外残留的年份（`(2024)` 去掉括号后剩 2024）
	work = reYearParen.ReplaceAllString(work, " ")

	// 发布组尾缀：`-RARBG` / `-YTS.MX`
	work = reGroupTail.ReplaceAllString(work, " ")

	// 结尾的版本孤字（`空枪.V.2026` 洗完剩 `空枪 V`）
	work = reVersionTail.ReplaceAllString(work, " ")

	// 分隔符统一成空格
	repl := strings.NewReplacer(".", " ", "_", " ", "·", " ", "　", " ")
	work = repl.Replace(work)

	// 去掉多余标点，但保留片名里有意义的部分（冒号、连字符交给 IMDb 匹配时忽略）
	work = strings.Trim(work, " -–—~|/\\,;+&*#@!?\"'`()[]{}<>")
	work = strings.Join(strings.Fields(work), " ")

	out.Title = work
	return out
}

// lastYear 找出文本里最靠右的、前后都是数字边界的四位年份。
//
// 不用正则：`(?:^|sep)(\d{4})(?:sep|$)` 会吃掉作为分隔符的字符，
// 于是 `1917.2019` 里第二个年份永远匹配不到（前一个匹配已经把点消耗了）。
func lastYear(s string) (year string, idx int) {
	idx = -1
	for i := 0; i+4 <= len(s); i++ {
		if i > 0 && isDigit(s[i-1]) {
			continue
		}
		cand := s[i : i+4]
		if (cand[0] != '1' || cand[1] != '9') && (cand[0] != '2' || cand[1] != '0') {
			continue
		}
		if !isDigit(cand[2]) || !isDigit(cand[3]) {
			continue
		}
		if i+4 < len(s) && isDigit(s[i+4]) {
			continue
		}
		year, idx = cand, i
	}
	if idx < 0 {
		return "", 0
	}
	return year, idx
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// findIMDbID 从一段文本里找 IMDb ID，返回小写的 `ttXXXXXXXX`。
func findIMDbID(s string) string {
	m := reIMDbID.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return "tt" + m[1]
}

// NormalizeIMDbID 把用户输入收敛成标准 ID。
//
// 用户可能粘整条链接（https://www.imdb.com/title/tt15239678/），
// 也可能只给数字。统一在这里处理，后面的缓存键和请求参数才是干净的。
func NormalizeIMDbID(raw string) string {
	return findIMDbID(strings.TrimSpace(raw))
}

func atoi(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

// cnDigits 是汉字数字，只到「十」为止：季号、集号不会有更大的个位。
var cnDigits = map[string]int{
	"一": 1, "二": 2, "两": 2, "三": 3, "四": 4, "五": 5,
	"六": 6, "七": 7, "八": 8, "九": 9, "十": 10,
}

// num 解析季集号，阿拉伯数字和汉字都认。
//
// 只支持 1-99：「第二十三季」这种超长剧不现实，
// 真出现了也是先认错再手动改，比写一个通用的中文数字解析器划算。
func num(s string) int {
	if n, err := strconv.Atoi(s); err == nil {
		return n
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if i := strings.Index(s, "十"); i >= 0 {
		tens := 1
		if i > 0 {
			v, ok := cnDigits[s[:i]]
			if !ok {
				return 0
			}
			tens = v
		}
		ones := 0
		if rest := s[i+len("十"):]; rest != "" {
			v, ok := cnDigits[rest]
			if !ok {
				return 0
			}
			ones = v
		}
		return tens*10 + ones
	}
	return cnDigits[s]
}

// normalizeTitle 用于比较：小写、只留字母数字和汉字。
func normalizeTitle(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r >= 0x4e00 && r <= 0x9fff:
			b.WriteRune(r)
		}
	}
	return b.String()
}
