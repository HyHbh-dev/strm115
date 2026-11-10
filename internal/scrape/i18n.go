package scrape

// 这个文件只干一件事：把 IMDb 的英文元数据变成「中文优先」的样子。
//
// 三处差异，按收益从高到低：
//
//  1. 标题：文件名里的中文名（发行方起的名，比机翻准）优先；
//  2. 简介：IMDb 的 overview 是英文，交给 Index-Translate 翻成中文；
//  3. 类型：Action/Drama 这类枚举，查表比机翻稳定，不消耗请求。
//
// 翻译结果按「原文」缓存进 SQLite。同一部片第二次刮削时零请求 —— 这既是
// 速度问题，也是账号安全问题：翻译接口同样会限流。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"
	"unicode"

	"strm115/internal/translate"
)

// hasChinese 判断是否含汉字。用来区分「已经是中文」和「需要翻译」。
func hasChinese(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// genreZH 是 IMDb / TMDB 的类型枚举到中文的映射。
//
// 用表而不是翻译接口：类型词是封闭集合，机翻偶尔会把 "War" 翻成「战争片」
// 而把 "Short" 翻成「短的」，Emby 的类型筛选就会多出一堆奇怪的分类。
var genreZH = map[string]string{
	"action":          "动作",
	"adventure":       "冒险",
	"animation":       "动画",
	"biography":       "传记",
	"comedy":          "喜剧",
	"crime":           "犯罪",
	"documentary":     "纪录",
	"drama":           "剧情",
	"family":          "家庭",
	"fantasy":         "奇幻",
	"film-noir":       "黑色电影",
	"film noir":       "黑色电影",
	"game-show":       "游戏节目",
	"history":         "历史",
	"horror":          "恐怖",
	"music":           "音乐",
	"musical":         "歌舞",
	"mystery":         "悬疑",
	"news":            "新闻",
	"reality-tv":      "真人秀",
	"romance":         "爱情",
	"sci-fi":          "科幻",
	"science fiction": "科幻",
	"short":           "短片",
	"sport":           "运动",
	"talk-show":       "脱口秀",
	"thriller":        "惊悚",
	"war":             "战争",
	"western":         "西部",
	"documentary ":    "纪录",
	"anime":           "动画",
}

// GenreZH 把单个英文类型翻成中文；认不出来就原样返回。
// 电影墙按这个显示类型标签，和写进 nfo 的用同一张表，不会出现两处叫法不一样。
func GenreZH(s string) string {
	if zh := genreZH[strings.ToLower(strings.TrimSpace(s))]; zh != "" {
		return zh
	}
	return strings.TrimSpace(s)
}

// zhGenres 把英文类型翻成中文并去重，保持原有顺序。
// 认不出来的原样保留——宁可出现一个英文类型，也不要丢信息。
func zhGenres(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, g := range in {
		g = strings.TrimSpace(g)
		if g == "" {
			continue
		}
		zh := genreZH[strings.ToLower(g)]
		if zh == "" {
			zh = g
		}
		if _, ok := seen[zh]; ok {
			continue
		}
		seen[zh] = struct{}{}
		out = append(out, zh)
	}
	return out
}

// translationKey 与字幕翻译的缓存键共用一张表，但加前缀隔离：
// 同一句台词和同一段简介可能碰巧一样，混用会让「翻译过的字幕」
// 被当成「翻译过的简介」，出错时极难排查。
func translationKey(text string) string {
	h := sha256.Sum256([]byte("meta\x00zh\x00" + text))
	return hex.EncodeToString(h[:])
}

// localize 把一部作品的元数据改成中文优先，返回改动后的副本。
//
// 翻译失败不报错：能翻就翻，翻不了保留英文。刮削的主线是「元数据落盘」，
// 不该因为一个第三方翻译接口挂了就整部片失败。
func (s *Service) localize(ctx context.Context, m Meta, rawTitle string, opt Options) Meta {
	if opt.Language != "" && !strings.EqualFold(opt.Language, "zh") {
		return m
	}
	if m.TitleCN == "" && hasChinese(rawTitle) {
		// 文件名里的中文名来自片源发布组，比机翻的片名可信得多。
		m.TitleCN = strings.TrimSpace(rawTitle)
	}
	if len(m.Genres) > 0 {
		m.Genres = zhGenres(m.Genres)
	}
	if !opt.Translate {
		return m
	}

	// 收集需要翻译的文本，一次请求解决标题 + 简介 + 单集标题/简介。
	// 分开调用会让一部 24 集的剧多出 48 次请求，限流是必然的。
	var (
		texts []string
		slots []func(string) // 每个待翻译文本对应的回填动作
	)
	add := func(text string, set func(string)) {
		text = strings.TrimSpace(text)
		if text == "" || hasChinese(text) {
			return
		}
		texts = append(texts, text)
		slots = append(slots, set)
	}

	if m.TitleCN == "" {
		add(m.Title, func(v string) { m.TitleCN = v })
	}
	add(m.Overview, func(v string) { m.Overview = v })
	for i := range m.Episodes {
		i := i
		add(m.Episodes[i].Title, func(v string) { m.Episodes[i].Title = v })
		add(m.Episodes[i].Overview, func(v string) { m.Episodes[i].Overview = v })
	}
	if len(texts) == 0 {
		return m
	}

	out := s.translateTexts(ctx, texts, opt)
	for i, set := range slots {
		if i < len(out) && out[i] != "" {
			set(out[i])
		}
	}
	return m
}

// translateTexts 批量翻译，命中本地缓存的不发请求。
// 返回与入参等长、顺序一致的切片；失败的位置保留原文。
func (s *Service) translateTexts(ctx context.Context, texts []string, opt Options) []string {
	out := make([]string, len(texts))
	copy(out, texts)
	if len(texts) == 0 {
		return out
	}

	keys := make([]string, len(texts))
	for i, t := range texts {
		keys[i] = translationKey(t)
	}
	cached := map[string]string{}
	if s.st != nil {
		if got, err := s.st.GetTranslations(ctx, keys); err == nil {
			cached = got
		} else {
			s.log.Warn("读翻译缓存失败，按全部未命中处理", "err", err)
		}
	}

	var (
		missingIdx []int
		missing    []string
	)
	for i, k := range keys {
		if v := cached[k]; strings.TrimSpace(v) != "" {
			out[i] = v
			continue
		}
		missingIdx = append(missingIdx, i)
		missing = append(missing, texts[i])
	}
	if len(missing) == 0 {
		return out
	}

	apiBase := opt.APIBase
	if apiBase == "" {
		apiBase = translate.DefaultAPIBase
	}
	model := opt.Model
	if model == "" {
		model = translate.DefaultModel
	}
	workers := opt.Workers
	if workers <= 0 {
		workers = 2
	}

	client := translate.NewClient(apiBase, model, 90*time.Second)
	tr := &translate.Translator{
		Client:  client,
		Target:  "zh",
		Workers: workers,
		Retries: 2,
		// 简介和标题共用一个提示词：两者都是「影视文本」，
		// 加太细的约束反而会让模型把标题也解释一遍。
		Instruction: "这是影视作品的标题或简介，请翻译成简体中文，保留人名、地名和专有名词，只输出译文。",
	}
	got, _, err := tr.Translate(ctx, missing)
	if err != nil {
		s.log.Warn("元数据翻译失败，保留原文", "err", err)
		return out
	}

	fresh := make(map[string]string, len(missing))
	for i, idx := range missingIdx {
		if i >= len(got) {
			break
		}
		v := strings.TrimSpace(got[i])
		// 与原文一模一样的多半是兜底返回，不写缓存，下次还能重试
		if v == "" || v == missing[i] {
			continue
		}
		out[idx] = v
		fresh[keys[idx]] = v
	}
	if s.st != nil && len(fresh) > 0 {
		if err := s.st.PutTranslations(ctx, fresh); err != nil {
			s.log.Warn("写翻译缓存失败", "err", err)
		}
	}
	return out
}
