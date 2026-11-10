package tmdb

// 中文片名 → 英文原名。
//
// 资源搜索原来只靠 IMDb 的自动建议接口换名：能查到，但那是「搜索建议」，
// 中文剧名经常给出一条完全不相干的结果。TMDB 的正经 /search/multi 有
// original_title / original_name，规范得多，而且这个项目本来就配了反代，
// 多打一次接口的代价可以忽略。
//
// 只返回「名字 + 年份」，不做映射表、不缓存：搜索词每次都不一样，缓存命中率
// 极低；真要缓存也是缓存整轮资源搜索结果（internal/resource 已经做了）。

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"
)

// TitleHit 是一次「换名」的结果。Name 是不带年份的英文/拉丁原名，
// Year 是四位年份（拿不到就空）。
//
// 分开两个字段是有用的：TG 频道的 ?q= 用纯名字（帖子标题里未必写年份），
// 种子站要多带一个年份压掉同名重拍片。
type TitleHit struct {
	Name string
	Year string
}

type multiSearchResponse struct {
	Results []struct {
		MediaType     string  `json:"media_type"`
		Title         string  `json:"title"`
		OriginalTitle string  `json:"original_title"`
		Name          string  `json:"name"`
		OriginalName  string  `json:"original_name"`
		ReleaseDate   string  `json:"release_date"`
		FirstAirDate  string  `json:"first_air_date"`
		Popularity    float64 `json:"popularity"`
	} `json:"results"`
}

// SearchOriginal 用一段（通常是中文的）片名查 TMDB，挑一个最像的拉丁名字。
//
// 两遍查询是必须的：
//   - 引进片（沙丘2）在 zh-CN 的 original_title 里直接就是 "Dune: Part Two"；
//   - 华语片（流浪地球 / 三体）的 original_title 本来就是中文，得换 en-US
//     查一次，拿它的**英文译名** "The Wandering Earth" / "Three-Body"。
//     少了这一遍，中文片只能退回 IMDb 建议接口，命中率差一大截。
//
// 查不到时返回 (nil, nil) —— 调用方会退回 IMDb，不算错误。
func (c *Client) SearchOriginal(ctx context.Context, query string) (*TitleHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	// 查询里带的年份只用来给结果打分，不能塞进 TMDB 的 query：
	// 「三体 2023」在 TMDB 里一条都搜不到，去掉年份才有结果。
	want := yearInQuery(query)
	bare := strings.Join(strings.Fields(strings.Replace(query, want, "", 1)), " ")
	if bare == "" {
		bare = query
	}
	hit, err := c.searchOriginalOnce(ctx, bare, want, DefaultLanguage, true)
	if err != nil || hit != nil {
		return hit, err
	}
	return c.searchOriginalOnce(ctx, bare, want, "en-US", false)
}

// searchOriginalOnce 查一遍并打分：原名精确等于查询 +4，年份对得上 +2，
// 同分保留 TMDB 自己排的前面那条（它按相关度排过序）。
func (c *Client) searchOriginalOnce(ctx context.Context, query, want, lang string, preferOriginal bool) (*TitleHit, error) {
	v := url.Values{
		"query":         {query},
		"language":      {lang},
		"include_adult": {"false"},
		"page":          {"1"},
	}
	raw, err := c.get(ctx, "/search/multi", v)
	if err != nil {
		return nil, err
	}
	var resp multiSearchResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}

	var best *TitleHit
	bestScore := -1
	for _, r := range resp.Results {
		orig := strings.TrimSpace(r.OriginalTitle)
		localized := strings.TrimSpace(r.Title)
		date := r.ReleaseDate
		if r.MediaType == KindTV {
			orig = strings.TrimSpace(r.OriginalName)
			localized = strings.TrimSpace(r.Name)
			date = r.FirstAirDate
		}
		// TMDB 的搜索会按简介/别名把不相关的片也带回来（实测「三体」能搜到
		// 一部 2023 年的法国纪录片 Notre corps）。先要求标题和查询对得上，
		// 否则会拿一部完全无关的片去搜种子站，比不换名还糟。
		if !titleMatches(localized, orig, query) {
			continue
		}
		name := orig
		if !preferOriginal {
			name = localized
		}
		if !isLatinTitle(name) {
			name = localized
			if !preferOriginal {
				name = orig
			}
		}
		if !isLatinTitle(name) {
			continue
		}
		y := ""
		if len(date) >= 4 && allDigits(date[:4]) {
			y = date[:4]
		}
		score := 0
		if strings.EqualFold(orig, query) {
			score += 4
		}
		if want != "" && y == want {
			score += 2
		}
		if score > bestScore {
			bestScore = score
			best = &TitleHit{Name: name, Year: y}
		}
	}
	return best, nil
}

// titleMatches 要求结果的本地化标题或原名和查询是「同一部片」：
// 精确相等，或一方是另一方的前缀（"流浪地球2" 对 "流浪地球"、
// "Dune: Part Two" 对 "Dune"）。不做模糊包含，免得简介里出现关键词就命中。
func titleMatches(localized, orig, query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return true
	}
	for _, s := range []string{localized, orig} {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if s == q || strings.HasPrefix(s, q) || strings.HasPrefix(q, s) {
			return true
		}
	}
	return false
}

// isLatinTitle 要求有拉丁字母且没有汉字/假名。
// 有些片的 original_title 就是中文（华语片），换它没意义，
// 但那种情况下种子站用中文反而更准，交给调用方退回原词。
func isLatinTitle(s string) bool {
	if s == "" {
		return false
	}
	hasLatin := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') {
			hasLatin = true
			continue
		}
		if (r >= 0x3040 && r <= 0x30FF) || (r >= 0x3400 && r <= 0x9FFF) ||
			(r >= 0xF900 && r <= 0xFAFF) {
			return false
		}
	}
	return hasLatin
}

// yearInQuery 抓查询里的四位年份，和 httpapi.queryYear 是同一套规则。
func yearInQuery(q string) string {
	for i := 0; i+4 <= len(q); i++ {
		seg := q[i : i+4]
		if (strings.HasPrefix(seg, "19") || strings.HasPrefix(seg, "20")) && allDigits(seg) {
			return seg
		}
	}
	return ""
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
