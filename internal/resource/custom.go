package resource

// 自定义源：用户自己填的 RSS / Torznab 地址。
//
// 为什么是 RSS 而不是「填个域名就能搜」：每个种子站的搜索页 HTML 都不一样，
// 通用解析做不到，硬做出来的东西每周都会坏。RSS 是这类站的公共接口 ——
// 内置的 Nyaa / 动漫花园 / Mikan 走的是它，Jackett、Prowlarr 聚合出来的
// 也是它（Torznab 就是带扩展字段的 RSS）。所以「自定义种子网」= 一条能搜索的
// RSS 地址，覆盖面反而比逐个适配域名大得多。
//
// 设置里存多行文本，每行一个源：
//
//	馒头 | https://pt.example.com/api/torznab/api?apikey=xxx&q={q}
//	https://nyaa.si/?page=rss&q={q}
//
// 名称可省（省了就用域名）；{q} 是关键词占位符，不写就自动追加 ?q=。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// CustomSite 是一条用户自填的搜索源。
type CustomSite struct {
	Key  string // 稳定标识 custom:<url 短哈希>
	Name string // 显示名
	URL  string // 含占位符的搜索地址
}

// ParseCustomSites 把设置里的多行文本解析成源列表。
//
// 容错按「用户会怎么抄」来设计：
//   - `名称 | 地址`、`名称｜地址`、`名称 地址` 都认（Jackett 的界面就是名字和地址两列）；
//   - 只填地址时名称取域名，界面上不至于显示一长串 URL；
//   - # 开头算注释，空行跳过；
//   - 地址必须以 http(s):// 开头，填错的行直接忽略 —— 宁可少一个源，
//     也不要带着一个永远超时的垃圾地址拖慢每一轮搜索。
func ParseCustomSites(raw string) []CustomSite {
	out := make([]CustomSite, 0, 4)
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, addr := splitSiteLine(line)
		if !isHTTPURL(addr) {
			continue
		}
		key := "custom:" + shortHash(addr)
		if seen[key] {
			continue
		}
		seen[key] = true
		if name == "" {
			name = hostOf(addr)
		}
		out = append(out, CustomSite{Key: key, Name: name, URL: addr})
		if len(out) >= 20 {
			break // 一屏之外没人看，也免得一次搜几十个源
		}
	}
	return out
}

// splitSiteLine 把一行拆成「名称 + 地址」。
func splitSiteLine(line string) (string, string) {
	// 先按竖线拆：这是最明确的写法，名称里带空格也不会拆错
	for _, sep := range []string{"|", "｜"} {
		if i := strings.Index(line, sep); i >= 0 {
			return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+len(sep):])
		}
	}
	// 没有竖线时按「哪一段是 URL」拆：URL 里不会有空格，所以第一个 http(s)
	// 开头的词就是地址，它前面都是名称。
	var name []string
	for _, f := range strings.Fields(line) {
		if isHTTPURL(f) {
			return strings.TrimSpace(strings.Join(name, " ")), f
		}
		name = append(name, f)
	}
	return "", ""
}

func isHTTPURL(s string) bool {
	lu := strings.ToLower(strings.TrimSpace(s))
	return strings.HasPrefix(lu, "http://") || strings.HasPrefix(lu, "https://")
}

// shortHash 给地址一个稳定的短 id：同一个地址每轮搜索结果都落在同一个 key 上，
// 前端排序和「记住哪个源坏了」才不会被搅乱。用哈希而不是明文是顺带的 ——
// key 会进接口响应，没必要把 apikey 带出去。
func shortHash(s string) string {
	sum := sha1.Sum([]byte(s))
	return hex.EncodeToString(sum[:4])
}

// searchURL 把关键词塞进地址。
func (c CustomSite) searchURL(q string) string {
	enc := url.QueryEscape(strings.TrimSpace(q))
	switch {
	case strings.Contains(c.URL, "{q}"):
		return strings.ReplaceAll(c.URL, "{q}", enc)
	case strings.Contains(c.URL, "{query}"):
		return strings.ReplaceAll(c.URL, "{query}", enc)
	}
	if strings.Contains(c.URL, "?") {
		return c.URL + "&q=" + enc
	}
	return c.URL + "?q=" + enc
}

// searchCustom 拉一次自定义源并转成统一结构。
//
// 走通用 RSS 解析，所以 Nyaa 那种 <nyaa:seeders> 和 Torznab 那种
// <torznab:attr name="seeders" value="12"/> 都能读出做种数。
func searchCustom(ctx context.Context, hc *http.Client, c CustomSite, q string, limit int) ([]Item, error) {
	body, err := httpGet(ctx, hc, c.searchURL(q), 8<<20)
	if err != nil {
		return nil, err
	}
	items, err := parseRSS(body)
	if err != nil {
		return nil, err
	}
	return customItems(items, c, limit), nil
}

// customItems 把 RSS 条目归一成 Item。
//
// 三种常见形态都要照顾：
//   - enclosure 是 magnet（动漫花园）→ 直接用；
//   - 有 infohash（Torznab / Nyaa）→ 拼磁力，115 能接；
//   - 只有 .torrent 直链（Mikan / 私有站）→ 交给 Torrent 字段，前端拿去离线或下载。
func customItems(items []rssItem, c CustomSite, limit int) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		enclosure := strings.TrimSpace(it.Enclosure.URL)

		magnet := ""
		if strings.HasPrefix(strings.ToLower(enclosure), "magnet:") {
			magnet = enclosure
		}
		if magnet == "" {
			magnet = strings.TrimSpace(it.extra("magneturl"))
		}
		if magnet == "" {
			if hash := it.extra("infohash"); hash != "" {
				magnet = magnetFor(hash, title)
			}
		}
		if magnet == "" {
			// 正文里常常也贴了一条磁力，比 .torrent 直链好用
			if m := reMagnet.FindString(it.Description); m != "" {
				magnet = m
			}
		}

		size := parseSizeText(it.extra("size"))
		if size == 0 {
			size, _ = strconv.ParseInt(strings.TrimSpace(it.Enclosure.Length), 10, 64)
		}
		seed, _ := strconv.Atoi(it.extra("seeders"))
		if seed == 0 {
			seed, _ = strconv.Atoi(it.extra("seeds"))
		}
		leech, _ := strconv.Atoi(it.extra("peers"))
		if leech == 0 {
			leech, _ = strconv.Atoi(it.extra("leechers"))
		}

		torrent := ""
		if strings.HasSuffix(strings.ToLower(enclosure), ".torrent") ||
			strings.Contains(strings.ToLower(it.Enclosure.Type), "bittorrent") {
			torrent = enclosure
		}
		link := strings.TrimSpace(it.Link)
		if link == "" {
			link = strings.TrimSpace(it.GUID)
		}

		out = append(out, Item{
			Title: title, Source: c.Name, SourceKey: c.Key, Kind: "torrent",
			Magnet: magnet, URL: link, Torrent: torrent,
			Size: size, Seeders: seed, Leechers: leech,
			Date:     parseRSSDate(it.PubDate),
			Category: it.extra("category"),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// CustomSummary 给界面用：把「用户填的那行」变成一句人话。
func (c CustomSite) Summary() string {
	if c.Name == hostOf(c.URL) {
		return c.URL
	}
	return fmt.Sprintf("%s · %s", c.Name, hostOf(c.URL))
}
