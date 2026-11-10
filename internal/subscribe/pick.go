// Package subscribe 是「订阅某部剧，出新集自动提交离线」的最小闭环。
//
// 一轮检查做四件事：问 TMDB 该出到第几集 → 比水位线 → 搜资源 → 挑一条提交。
// 提交出去之后什么都不用管：离线完成 → 自动整理 → 自动同步 → 自动刮削 →
// 入库通知，这条链路本来就在跑。
//
// 这里刻意不做的事（留给后续迭代，而不是假装做了）：
//   - 不抓第三方播出表（yysub 之类）：它们没有 tmdb id，中文名和英文名还粘在
//     一起，对齐成本比收益高；TMDB 的 last_episode_to_air 是同一个问题的权威答案。
//   - 不处理 115 分享（ShareCode）与网盘链接：那需要先解锁再转存，跟「提交磁力
//     离线」是两条完全不同的路。
//   - 不支持日番的 C01 写法：认不出来就如实说「没有匹配 SxxExx 的」，宁可漏
//     一集，也不要下错集。第5集 / EP05 / 单独一个 199 这类中文与简写是支持的，
//     但只认「能唯一确定集号」的标题，见 parseEpisode 的注释。
package subscribe

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"strm115/internal/resource"
)

// PickOptions 是一条订阅的挑选条件。
type PickOptions struct {
	Quality string // 2160p / 1080p / 720p；留空等于 any
	MinSize int64  // GB
	MaxSize int64  // GB，0 表示不限
	Season  int    // 剧集目标季；0 表示不限（电影）
	Episode int    // 剧集目标集；0 表示不限（电影）
}

// Pick 从搜索结果里挑一条最合适的；没有符合条件时返回 nil。
//
// 判定的顺序是有讲究的，每一步都能单独解释给用户听：
//  1. 必须有磁力或种子直链 —— 只有详情页 URL 的条目交不了差
//  2. 剧集必须匹配季集号 —— 这一步宁可严，下错集比漏一集糟得多
//  3. 画质不能低于目标
//  4. 体积落在区间内
//
// 之后按「画质 → 做种数 → 体积」排序：同画质下有做种的才下得动，
// 都没做种数时挑大的（同画质大的一般码率高）。
func Pick(items []resource.Item, opt PickOptions) *resource.Item {
	target := qualityRank(opt.Quality)

	type cand struct {
		it    resource.Item
		rank  int
		seed  int
		size  int64
		known bool // 画质是不是真认出来的
	}
	var cands []cand

	for _, it := range items {
		if !submittable(it) {
			continue
		}
		if opt.Episode > 0 {
			if s, e, ok := parseEpisode(it.Title); !ok || s != opt.Season || e != opt.Episode {
				continue
			}
		}
		rank, known := qualityOf(it.Title)
		if rank < target {
			continue
		}
		if opt.MinSize > 0 && it.Size > 0 && it.Size < opt.MinSize<<30 {
			continue
		}
		if opt.MaxSize > 0 && it.Size > 0 && it.Size > opt.MaxSize<<30 {
			continue
		}
		cands = append(cands, cand{it: it, rank: rank, seed: it.Seeders, size: it.Size, known: known})
	}
	if len(cands) == 0 {
		return nil
	}

	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.rank != b.rank {
			return a.rank > b.rank
		}
		// 画质相同但一个是猜的、一个是认出来的，优先认出来的。
		if a.known != b.known {
			return a.known
		}
		if a.seed != b.seed {
			return a.seed > b.seed
		}
		return a.size > b.size
	})
	out := cands[0].it
	return &out
}

// submittable 判断这条结果能不能直接交给 115 离线。
//
// ShareCode / Links 走的是「先解锁再转存」，跟提交磁力不是一条路，这里不碰。
// Torrent 是 .torrent 直链（Mikan 这种站没有 info hash），离线也吃。
func submittable(it resource.Item) bool {
	return strings.TrimSpace(it.Magnet) != "" || strings.TrimSpace(it.Torrent) != ""
}

// URI 返回可以交给离线任务的链接。
func URI(it resource.Item) string {
	if m := strings.TrimSpace(it.Magnet); m != "" {
		return m
	}
	return strings.TrimSpace(it.Torrent)
}

// 画质档位。只认这几个：再多就是给「HDR」「DV」「REMUX」这些修饰词
// 单开档位，而它们跟分辨率不是一回事（1080p REMUX 比 2160p WEBRip 清楚）。
var qualityPatterns = []struct {
	re   *regexp.Regexp
	rank int
}{
	{regexp.MustCompile(`(?i)(?:^|[^0-9])(2160p|4k|uhd)(?:[^0-9]|$)`), 2160},
	{regexp.MustCompile(`(?i)(?:^|[^0-9])(1440p|2k)(?:[^0-9]|$)`), 1440},
	{regexp.MustCompile(`(?i)(?:^|[^0-9])(1080[pi]|fhd)(?:[^0-9]|$)`), 1080},
	{regexp.MustCompile(`(?i)(?:^|[^0-9])(720p)(?:[^0-9]|$)`), 720},
	{regexp.MustCompile(`(?i)(?:^|[^0-9])(480p|360p)(?:[^0-9]|$)`), 480},
}

// qualityOf 从标题里认分辨率。
//
// 认不出来时返回 (1080, false)：多数片源就是 1080p，而「认不出来就丢掉」
// 会把大量只有剧名+集数的 TG 帖子排除掉 —— 那些恰恰是能下的。
// 认出来的排前面（见 Pick 的排序），所以这只是兜底，不是首选。
func qualityOf(title string) (rank int, known bool) {
	for _, p := range qualityPatterns {
		if p.re.MatchString(title) {
			return p.rank, true
		}
	}
	return 1080, false
}

// qualityRank 把订阅设置里的画质串换成档位。认不出的按 1080p 算。
func qualityRank(q string) int {
	if strings.TrimSpace(q) == "" {
		return 0
	}
	if r, ok := qualityOf(q); ok {
		return r
	}
	return 1080
}

// 集号识别的几条正则，按可靠性从高到低排。
//
// episodeRe：S02E05 / S02.E05 / S02 E05（季集都有，最可靠）
// cnEpisodeRe：「第5集 / 第5话」，国产剧和日番最常见
// epMarkRe：EP05 / E05 / EP.5（只有集号，季号另找）
// cnSuffixRe：5集 / 5话（没有「第」字）
// seasonOnlyRe：标题里单独出现的 S02（给上面几种只有集号的形式补季号）
var (
	episodeRe    = regexp.MustCompile(`(?i)s(\d{1,2})\s*[._-]?\s*e(\d{1,4})`)
	cnSeasonRe   = regexp.MustCompile(`第\s*(\d{1,3})\s*季`)
	cnEpisodeRe  = regexp.MustCompile(`第\s*(\d{1,4})\s*[集话話期]`)
	cnSuffixRe   = regexp.MustCompile(`(?:^|[^0-9])(\d{1,4})\s*[集话話期]`)
	epMarkRe     = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])ep?[._\s-]?(\d{1,4})(?:[^0-9]|$)`)
	seasonOnlyRe = regexp.MustCompile(`(?i)(?:^|[^a-z0-9])s(\d{1,2})(?:[^0-9]|$)`)
)

// parseEpisode 从标题里抠季集号，认不出来就返回 ok=false。
//
// 支持的写法（按优先级）：
//  1. S02E05 / S02.E05 / S02 E05 / S02E05-E06（多集连播取第一集）
//  2. 第5集 / 第5话 / 第5期，前面有「第2季」时季号跟着走
//  3. EP05 / E05 / EP.5
//  4. 5集 / 5话（没有「第」字）
//  5. 标题里孤零零一个数字：「凡人修仙传 199」（见 loneEpisode）
//
// 只有集号、没有季号的一律按第 1 季算：目标季不是 1 时自然对不上，
// 结果就是「漏一集」而不是「下错集」，这是故意的取舍。
//
// 不支持的写法：C01（不同站点含义不一致）、中文数字（第一百九十九集，
// 极少见且容易和「第二季」混淆）。这些仍然返回 false。
func parseEpisode(title string) (season, episode int, ok bool) {
	if m := episodeRe.FindStringSubmatch(title); m != nil {
		s, err1 := strconv.Atoi(m[1])
		e, err2 := strconv.Atoi(m[2])
		if err1 == nil && err2 == nil && s > 0 && e > 0 {
			return s, e, true
		}
	}
	season = seasonOf(title)
	// 「第199集」比「199集」更明确，先试前者；两者都只认第一个命中。
	for _, re := range []*regexp.Regexp{cnEpisodeRe, epMarkRe, cnSuffixRe} {
		if m := re.FindStringSubmatch(title); m != nil {
			if e, err := strconv.Atoi(m[1]); err == nil && e > 0 {
				return season, e, true
			}
		}
	}
	if e, ok := loneEpisode(title); ok {
		return season, e, true
	}
	return 0, 0, false
}

// seasonOf 找标题里的季号：先看「第2季」，再看单独的 S02，都没有就算第 1 季。
func seasonOf(title string) int {
	for _, re := range []*regexp.Regexp{cnSeasonRe, seasonOnlyRe} {
		if m := re.FindStringSubmatch(title); m != nil {
			if s, err := strconv.Atoi(m[1]); err == nil && s > 0 {
				return s
			}
		}
	}
	return 1
}

// loneEpisode 处理只有剧名和数字的标题：「凡人修仙传 199」「Show 05 1080p」。
//
// 这里没有「第」「集」这类标记，唯一的依据是数字的形状，所以规则必须保守：
// 只有**恰好一个**数字候选时才认，其它一律 false。被排除的：
//   - 紧贴字母的：x264 / H265 / 1080p / 4k / AAC2.0 里的数字
//   - 小数的：DDP5.1 / 2.0 声道
//   - 年份：1900~2099（凡人修仙传 199 (2020) 里的 2020）
//   - 分辨率：480 / 576 / 720 / 1080 / 1440 / 2160 / 4320
//
// 出现两个以上候选（如「199-210 合集」）说明这是范围或合辑，交出去可能下错，
// 直接放弃。
func loneEpisode(title string) (int, bool) {
	hit, episode := 0, 0
	for i := 0; i < len(title); i++ {
		if !isDigit(title[i]) {
			continue
		}
		start := i
		for i < len(title) && isDigit(title[i]) {
			i++
		}
		end := i
		i-- // 外层循环还要 i++，这里退一格
		if isAttachedToWord(title, start, end) {
			continue
		}
		v, err := strconv.Atoi(title[start:end])
		if err != nil || v <= 0 || v > 2000 {
			continue
		}
		if v >= 1900 || isResolutionNumber(v) {
			continue
		}
		hit++
		episode = v
	}
	return episode, hit == 1
}

// isAttachedToWord 判断这个数字是不是和字母、小数点粘在一起。
//
// 粘着就说明它是标识符的一部分（x264 / 1080p / 5.1），不是独立写出来的集号。
// 连字符（199-210）不在这里排除：那种范围会出现两个候选，由 loneEpisode
// 用「只认一个候选」的规则否决。
func isAttachedToWord(s string, start, end int) bool {
	isAlpha := func(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }
	if start > 0 {
		prev := s[start-1]
		if isAlpha(prev) {
			return true
		}
		if prev == '.' && start > 1 && isDigit(s[start-2]) {
			return true
		}
	}
	if end < len(s) {
		next := s[end]
		if isAlpha(next) {
			return true
		}
		if next == '.' && end+1 < len(s) && isDigit(s[end+1]) {
			return true
		}
	}
	return false
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }

// isResolutionNumber 是一组「当集号太像分辨率」的数字，裸数字场景下直接排除。
func isResolutionNumber(v int) bool {
	switch v {
	case 360, 480, 540, 576, 720, 1080, 1440, 2160, 4320:
		return true
	}
	return false
}

// Describe 把一轮检查的结论写成人话，直接存进 last_result 给页面看。
func Describe(items int, opt PickOptions) string {
	if opt.Episode > 0 {
		return fmt.Sprintf("搜到 %d 条，没有匹配 S%02dE%02d 且满足画质的", items, opt.Season, opt.Episode)
	}
	return fmt.Sprintf("搜到 %d 条，没有满足画质与体积条件的", items)
}
