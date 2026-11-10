package linkparse

import (
	"net/url"
	"regexp"
	"strings"
)

// LinkType 描述匹配到的链接类型
type LinkType string

const (
	LinkTypeMagnet   LinkType = "magnet"    // 磁力链接
	LinkTypeED2K     LinkType = "ed2k"      // 电驴链接
	LinkTypeTorrent  LinkType = "torrent"   // HTTP/HTTPS 种子链接
	LinkTypeInfoHash LinkType = "infohash"  // 纯 BT Info Hash
	LinkTypeIMDbAPI  LinkType = "imdb_api"  // IMDb GraphQL / REST API 端点
	LinkTypeIMDbPage LinkType = "imdb_page" // IMDb 标题页 / 搜索页 / 裸 ID
)

// MatchedLink 表示一个匹配到的链接
type MatchedLink struct {
	Type       LinkType `json:"type"`                 // 链接类型
	Value      string   `json:"value"`                // 链接原文（或规范化后的链接）
	IMDbID     string   `json:"imdbId,omitempty"`     // 仅 IMDb 标题类型时有值，如 tt1375666
	SearchTerm string   `json:"searchTerm,omitempty"` // 仅 IMDb 搜索页时有值，find?q= 的查询词
}

// ParseResult 是整体解析结果
type ParseResult struct {
	FoundLinks []MatchedLink // 匹配到的所有链接
	MediaName  string        // 从文本中提取的媒体名称（用于后续 IMDb 搜索）
}

// ──────────────────────────────────────────────
// 正则定义（全部使用 RE2 语法，Go 标准库 regexp）
// ──────────────────────────────────────────────

var (
	// ① 磁力链接 magnet:?xt=urn:btih:<hash>&...
	//    hash 支持 40 位十六进制或 32 位 Base32（115 离线下载主要支持的类型）
	reMagnet = regexp.MustCompile(
		`(?i)magnet:\?xt=urn:btih:([a-f0-9]{40}|[a-z2-7]{32})[^\s"'<>]*`,
	)

	// ② ed2k 链接 ed2k://|file|<name>|<size>|<hash>|/
	reED2K = regexp.MustCompile(
		`(?i)ed2k://\|file\|[^|\s]*\|\d+\|[a-f0-9]{32}\|/[^\s"'<>]*`,
	)

	// ③ HTTP/HTTPS .torrent 种子链接（可带查询串）
	reTorrent = regexp.MustCompile(
		`(?i)https?://[^\s"'<>]+?\.torrent(?:\?[^\s"'<>]*)?`,
	)

	// ④ 纯 BT Info Hash — 40 位十六进制（独立出现）
	//    RE2 不支持负向断言，重复排除在 Parse() 里用代码处理
	reInfoHash = regexp.MustCompile(
		`\b[a-fA-F0-9]{40}\b`,
	)

	// ⑤ IMDb 官方 GraphQL / REST API 端点
	//    例: https://api.imdb.com/graphql
	//        https://api.graphql.imdb.com/
	//        https://imdb-api.com/en/API/SearchTitle/<key>/Inception
	//    注意：api. 前缀必选，避免把 www.imdb.com/title/... 误判为 API 端点
	reIMDbAPIEndpoint = regexp.MustCompile(
		`(?i)https?://(?:api\.|api\.graphql\.)imdb\.com/[^\s"'<>]*`,
	)
	reIMDbRESTAPI = regexp.MustCompile(
		`(?i)https?://(?:www\.)?imdb-api\.com/[^\s"'<>]*`,
	)

	// ⑥ IMDb 标题页 / 裸 tt ID
	//    例: https://www.imdb.com/title/tt1375666/
	//        tt1375666
	reIMDbTitle = regexp.MustCompile(
		`(?i)(?:https?://)?(?:www\.)?imdb\.com/title/(tt\d{7,10})`,
	)
	reBareIMDbID = regexp.MustCompile(
		`\b(tt\d{7,10})\b`,
	)

	// ⑦ IMDb 搜索页 find?q=
	reIMDbFind = regexp.MustCompile(
		`(?i)(?:https?://)?(?:www\.)?imdb\.com/find\?[^\s"'<>]*`,
	)
)

// Parse 解析输入文本，提取所有 115 可离线下载链接和/或 IMDb 链接。
// 若未找到任何链接，则尝试从文本中提取媒体名称，供上层调用 IMDb 搜索。
func Parse(input string) *ParseResult {
	result := &ParseResult{}
	seen := make(map[string]bool) // 按链接/ID 去重

	// ── 1. 磁力链接 ──
	for _, m := range reMagnet.FindAllString(input, -1) {
		if !seen[m] {
			seen[m] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:  LinkTypeMagnet,
				Value: m,
			})
		}
	}

	// ── 2. ed2k 链接 ──
	for _, m := range reED2K.FindAllString(input, -1) {
		if !seen[m] {
			seen[m] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:  LinkTypeED2K,
				Value: m,
			})
		}
	}

	// ── 3. .torrent 种子链接 ──
	for _, m := range reTorrent.FindAllString(input, -1) {
		if !seen[m] {
			seen[m] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:  LinkTypeTorrent,
				Value: m,
			})
		}
	}

	// ── 4. 独立 Info Hash（排除已嵌在 magnet / ed2k 里的） ──
	for _, m := range reInfoHash.FindAllString(input, -1) {
		if seen[m] {
			continue
		}
		alreadyInsideOtherLink := false
		for _, link := range result.FoundLinks {
			if strings.Contains(strings.ToLower(link.Value), strings.ToLower(m)) {
				alreadyInsideOtherLink = true
				break
			}
		}
		if !alreadyInsideOtherLink {
			seen[m] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:  LinkTypeInfoHash,
				Value: m,
			})
		}
	}

	// ── 5. IMDb 官方 API 端点 ──
	for _, pattern := range []*regexp.Regexp{reIMDbAPIEndpoint, reIMDbRESTAPI} {
		for _, m := range pattern.FindAllString(input, -1) {
			if !seen[m] {
				seen[m] = true
				result.FoundLinks = append(result.FoundLinks, MatchedLink{
					Type:  LinkTypeIMDbAPI,
					Value: m,
				})
			}
		}
	}

	// ── 6. IMDb 标题页（含 tt ID） ──
	for _, sub := range reIMDbTitle.FindAllStringSubmatch(input, -1) {
		imdbID := strings.ToLower(sub[1])
		if !seen["id:"+imdbID] {
			seen["id:"+imdbID] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:   LinkTypeIMDbPage,
				Value:  "https://www.imdb.com/title/" + imdbID + "/",
				IMDbID: imdbID,
			})
		}
	}

	// ── 7. 裸 IMDb ID（排除已由标题页匹配过的） ──
	for _, sub := range reBareIMDbID.FindAllStringSubmatch(input, -1) {
		imdbID := strings.ToLower(sub[1])
		if !seen["id:"+imdbID] {
			seen["id:"+imdbID] = true
			result.FoundLinks = append(result.FoundLinks, MatchedLink{
				Type:   LinkTypeIMDbPage,
				Value:  "https://www.imdb.com/title/" + imdbID + "/",
				IMDbID: imdbID,
			})
		}
	}

	// ── 8. IMDb 搜索页 find?q= ──
	for _, m := range reIMDbFind.FindAllString(input, -1) {
		if seen[m] {
			continue
		}
		seen[m] = true
		searchTerm := extractQParam(m)
		result.FoundLinks = append(result.FoundLinks, MatchedLink{
			Type:       LinkTypeIMDbPage,
			Value:      m,
			SearchTerm: searchTerm,
		})
	}

	// ── 9. 没有任何链接 → 提取媒体名称 ──
	if len(result.FoundLinks) == 0 {
		result.MediaName = extractMediaName(input)
	}

	return result
}

// ExtractID 返回解析结果中第一个 IMDb ID，没有则返回空字符串
func (r *ParseResult) ExtractID() string {
	for _, link := range r.FoundLinks {
		if link.IMDbID != "" {
			return link.IMDbID
		}
	}
	return ""
}

// extractQParam 从 IMDb find 链接里解析 q 查询参数
func extractQParam(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("q"))
}

// CleanMediaName 把文件名 / 磁力链标题洗成片名：去扩展名、分辨率、编码、
// 片源、发布组，再把 . _ 换成空格。
//
// 给「链接里自带 dn=文件名」这种场景用 —— 那种文本没有独立的标题行，
// 但文件名本身通常就够拿去 IMDb 搜了。
func CleanMediaName(s string) string { return extractMediaName(s) }

// extractMediaName 从纯文本中提取可能的媒体名称。
// 策略（启发式，可按实际文件名规则调整）：
//   - 取第一行非空文本
//   - 去除文件扩展名、分辨率、编码、片源、发布组标识
//   - 将 . _ 替换为空格并压缩空白
func extractMediaName(input string) string {
	text := strings.TrimSpace(input)

	// 多行时取第一行非空行
	for _, line := range strings.Split(text, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			text = trimmed
			break
		}
	}

	// 去除常见扩展名
	extRe := regexp.MustCompile(`(?i)\.(mkv|mp4|avi|mov|wmv|flv|ts|m2ts|torrent|iso|rmvb|rm|webm)$`)
	text = extRe.ReplaceAllString(text, "")

	// 分隔符归一
	text = strings.ReplaceAll(text, ".", " ")
	text = strings.ReplaceAll(text, "_", " ")

	// 分辨率 / 编码 / 片源
	resRe := regexp.MustCompile(`(?i)\b(480p|576p|720p|1080[pi]|2160p|4k|uhd|hdr10?|sdr|dovi|10bit|8bit)\b`)
	text = resRe.ReplaceAllString(text, "")
	codecRe := regexp.MustCompile(`(?i)\b(x264|x265|h\.?264|h\.?265|hevc|avc|aac|ac3|dts|ddp?5\.?1|atmos|truehd|flac)\b`)
	text = codecRe.ReplaceAllString(text, "")
	srcRe := regexp.MustCompile(`(?i)\b(bluray|blu-ray|bdrip|brrip|web-?dl|web-?rip|webrip|hdtv|dvdrip|dvdscr|hdcam|cam|ts|tc|remux|repack|proper|extended|unrated)\b`)
	text = srcRe.ReplaceAllString(text, "")

	// 末尾发布组标识，如 -RARBG / -FGT
	groupRe := regexp.MustCompile(`\s*-[A-Za-z0-9]{2,}$`)
	text = groupRe.ReplaceAllString(text, "")

	// 压缩空白
	text = regexp.MustCompile(`\s+`).ReplaceAllString(text, " ")
	return strings.TrimSpace(text)
}

// HasOfflineDownloadLink 快速判断是否包含 115 可离线下载链接（不含 IMDb 链接）
func HasOfflineDownloadLink(input string) bool {
	return reMagnet.MatchString(input) ||
		reED2K.MatchString(input) ||
		reTorrent.MatchString(input) ||
		reInfoHash.MatchString(input)
}

// HasIMDbLink 判断是否包含任意 IMDb 链接或 ID
func HasIMDbLink(input string) bool {
	return reIMDbAPIEndpoint.MatchString(input) ||
		reIMDbRESTAPI.MatchString(input) ||
		reIMDbTitle.MatchString(input) ||
		reBareIMDbID.MatchString(input) ||
		reIMDbFind.MatchString(input)
}

// ExtractIMDbID 提取第一个 IMDb ID (ttXXXXXXX)，没有则返回空字符串
func ExtractIMDbID(input string) string {
	if sub := reIMDbTitle.FindStringSubmatch(input); len(sub) >= 2 {
		return strings.ToLower(sub[1])
	}
	if sub := reBareIMDbID.FindStringSubmatch(input); len(sub) >= 2 {
		return strings.ToLower(sub[1])
	}
	return ""
}
