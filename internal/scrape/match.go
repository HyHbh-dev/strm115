package scrape

// 从 TMDB 搜索结果里挑一条最像的。
//
// 以前这一步是对 IMDb suggestion 结果打分；改成 TMDB 之后阈值和权重保持
// 不变（宁可漏刮也不要错刮），只把「一条候选」换成 tmdb.MetaHit：
// 它同时带 zh-CN 标题和 original_title，中文文件名、英文文件名都能对上。

import (
	"strconv"
	"strings"

	"strm115/internal/tmdb"
)

// 匹配阈值。低于它的结果一律当作「没找到」。
//
// 宁可漏刮也不要错刮：一部被认成另一部片的电影，Emby 里会显示错误的
// 海报和简介，用户很难意识到是刮削错了，只会觉得"这软件有 bug"。
const minScore = 45

// Pick 从 TMDB 搜索结果里挑一条最像的。
func Pick(p Parsed, results []tmdb.MetaHit) (tmdb.MetaHit, bool) {
	best := tmdb.MetaHit{}
	bestScore := 0
	for _, r := range results {
		s := score(p, r)
		if s > bestScore {
			best, bestScore = r, s
		}
	}
	if bestScore < minScore {
		return tmdb.MetaHit{}, false
	}
	return best, true
}

// score 给一条候选打分。
func score(p Parsed, r tmdb.MetaHit) int {
	if r.ID <= 0 {
		return 0
	}

	total := 0

	// —— 片名 ——
	// TMDB 的 zh-CN 标题对中文片名，original_title 对英文片名，取高的那个。
	want := normalizeTitle(p.Title)
	gotTitle := normalizeTitle(r.Title)
	gotOrig := normalizeTitle(r.Original)
	nameScore := 0
	for _, got := range []string{gotTitle, gotOrig} {
		if s := namePoints(want, got); s > nameScore {
			nameScore = s
		}
	}
	total += nameScore

	// —— 年份 ——
	if p.Year != "" && r.Year != "" {
		py, _ := strconv.Atoi(p.Year)
		ry, _ := strconv.Atoi(r.Year)
		switch {
		case py == ry:
			total += 30
		case diff(py, ry) == 1:
			// 跨年上映很常见：国内 2024 上映，TMDB 记 2023
			total += 16
		case diff(py, ry) <= 2:
			total += 6
		default:
			total -= 22
		}
	}

	// —— 类型 ——
	isTV := r.Kind == tmdb.KindTV
	switch {
	case p.Kind == KindSeries && isTV:
		total += 18
	case p.Kind == KindSeries && !isTV:
		total -= 26
	case p.Kind == KindMovie && !isTV:
		total += 18
	case p.Kind == KindMovie && isTV:
		total -= 26
	}

	return total
}

func namePoints(want, got string) int {
	switch {
	case want == "" || got == "":
		return 0
	case want == got:
		return 50
	case strings.Contains(got, want) || strings.Contains(want, got):
		// 长度差太大说明只是碰巧包含（`Dune` vs `Dune Part Two` 这种要扣一点）
		long, short := len(got), len(want)
		if long < short {
			long, short = short, long
		}
		if short*100 >= long*55 {
			return 30
		}
		return 12
	default:
		return 20 * tokenOverlap(want, got) / 100
	}
}

func diff(a, b int) int {
	if a > b {
		return a - b
	}
	return b - a
}

// tokenOverlap 算两段文本的词重合度（0~100）。
// 中文没有空格，按字切；两边都这么做，比较才公平。
func tokenOverlap(a, b string) int {
	as := tokenize(a)
	bs := tokenize(b)
	if len(as) == 0 || len(bs) == 0 {
		return 0
	}
	set := make(map[string]struct{}, len(bs))
	for _, t := range bs {
		set[t] = struct{}{}
	}
	hit := 0
	for _, t := range as {
		if _, ok := set[t]; ok {
			hit++
		}
	}
	return hit * 100 / max(len(as), len(bs))
}

// tokenize 把归一化后的字符串切成可比较的单元：
// 连续 ASCII 字母数字算一个词，汉字每个算一个。
func tokenize(s string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, r := range s {
		if r >= 0x4e00 && r <= 0x9fff {
			flush()
			out = append(out, string(r))
			continue
		}
		cur.WriteRune(r)
	}
	flush()
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
