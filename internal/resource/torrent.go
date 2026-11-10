package resource

// 种子站都走「搜索接口」而不是搜索页：
//   - 海盗湾：apibay.org/q.php 返回 JSON，带 info_hash，磁力自己拼；
//   - Nyaa：RSS，带 infoHash / seeders / size；
//   - 动漫花园：RSS，enclosure 直接就是磁力；
//   - Mikan：RSS，只给 .torrent 下载地址（没有 info hash，拼不出磁力）。
//
// 这些站没有统一 schema，所以每家一个函数，共用 RSS 解析和体积/时间归一化。

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// trackers 会拼进磁力链接。115 离线对磁力的 tracker 不敏感，
// 但用户可能把同一串磁力丢给 qBittorrent，带上更稳。
var trackers = []string{
	"udp://tracker.opentrackr.org:1337/announce",
	"udp://open.tracker.cl:1337/announce",
	"udp://tracker.openbittorrent.com:6969/announce",
	"http://tracker.openbittorrent.com:80/announce",
}

// magnetFor 用 info hash 拼一条能直接用的磁力。
func magnetFor(hash, name string) string {
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return ""
	}
	q := "magnet:?xt=urn:btih:" + strings.ToUpper(hash)
	if name != "" {
		q += "&dn=" + url.QueryEscape(name)
	}
	for _, tr := range trackers {
		q += "&tr=" + url.QueryEscape(tr)
	}
	return q
}

// ==================== 海盗湾（apibay） ====================

type apibayEntry struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	InfoHash string `json:"info_hash"`
	Seeders  string `json:"seeders"`
	Leechers string `json:"leechers"`
	Size     string `json:"size"`
	Added    string `json:"added"`
	Category string `json:"category"`
	Username string `json:"username"`
}

func searchTPB(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := "https://apibay.org/q.php?q=" + url.QueryEscape(q) + "&cat=0"
	body, err := httpGet(ctx, hc, u, 4<<20)
	if err != nil {
		return nil, err
	}
	items, err := parseApibay(body, q, limit)
	if err != nil {
		return nil, err
	}
	return items, nil
}

// parseApibay 单独拆出来是为了能拿真响应做单测（不用真连海盗湾）。
//
// 带 q 是因为海盗湾有个坑：查询串是纯中文时它不返回空，而是把全站热门
// 一百条原样丢回来。那不是搜索结果，得靠标题里有没有关键词把它筛掉。
func parseApibay(body []byte, q string, limit int) ([]Item, error) {
	var raw []apibayEntry
	if err := json.Unmarshal(body, &raw); err != nil {
		// 被 Cloudflare 拦下来时返回的是 HTML，报错要说得明白点
		return nil, fmt.Errorf("apibay 返回的不是 JSON（可能被拦截）")
	}
	out := make([]Item, 0, len(raw))
	for _, e := range raw {
		if e.ID == "0" || e.InfoHash == "" || strings.EqualFold(e.Name, "No results returned") {
			continue
		}
		if relevance(e.Name, q) == 0 {
			continue
		}
		size, _ := strconv.ParseInt(e.Size, 10, 64)
		seed, _ := strconv.Atoi(e.Seeders)
		leech, _ := strconv.Atoi(e.Leechers)
		added, _ := strconv.ParseInt(e.Added, 10, 64)
		date := ""
		if added > 0 {
			date = time.Unix(added, 0).Format("2006-01-02")
		}
		out = append(out, Item{
			Title: e.Name, Source: "海盗湾", SourceKey: "tpb", Kind: "torrent",
			Magnet: magnetFor(e.InfoHash, e.Name), URL: "https://thepiratebay.org/description.php?id=" + e.ID,
			Size: size, Seeders: seed, Leechers: leech, Date: date,
			Category: tpbCategory(e.Category), Snippet: e.Username,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// tpbCategory 只区分「是不是片」，太细的分类对找片没用。
func tpbCategory(code string) string {
	switch {
	case strings.HasPrefix(code, "2"):
		return "视频"
	case strings.HasPrefix(code, "1"):
		return "音频"
	case strings.HasPrefix(code, "3"):
		return "软件"
	case strings.HasPrefix(code, "4"):
		return "游戏"
	}
	return ""
}

// ==================== RSS 通用解析 ====================

type rssDoc struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
}

type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	PubDate     string `xml:"pubDate"`
	Description string `xml:"description"`
	// 注意：这里不能声明 `xml:"size"` 字段。Go 的 xml 解码按 Local 名匹配、
	// 不看命名空间，加了这个字段会把 Nyaa 的 <nyaa:size>1.4 GiB</nyaa:size>
	// 抢走，Extras 里就再也读不到体积了。统一走 extra("size")。
	Enclosure struct {
		URL    string `xml:"url,attr"`
		Length string `xml:"length,attr"`
		Type   string `xml:"type,attr"`
	} `xml:"enclosure"`
	// Extras 兜住带命名空间的字段（nyaa:seeders 之类）。
	// 按 Local 名匹配，不绑 namespace URI —— 它们改过一次命名空间，靠 URI 匹配会失效。
	Extras []rssExtra `xml:",any"`
}

// rssExtra 同时兜两种扩展字段：
//
//	<nyaa:seeders>12</nyaa:seeders>            → 值在 chardata 里
//	<torznab:attr name="seeders" value="12"/>  → 值在 value 属性里
//
// Torznab（Jackett / Prowlarr）把 infohash、seeders、size 全塞在 attr 里，
// 不认这个格式，自定义源就只能拿到标题。
type rssExtra struct {
	XMLName xml.Name
	Value   string `xml:",chardata"`
	Name    string `xml:"name,attr"`
	AttrVal string `xml:"value,attr"`
}

func (it rssItem) extra(name string) string {
	for _, e := range it.Extras {
		if strings.EqualFold(e.XMLName.Local, name) && e.AttrVal == "" {
			if v := strings.TrimSpace(e.Value); v != "" {
				return v
			}
		}
	}
	for _, e := range it.Extras {
		if strings.EqualFold(e.Name, name) {
			if v := strings.TrimSpace(e.AttrVal); v != "" {
				return v
			}
		}
	}
	return ""
}

func parseRSS(body []byte) ([]rssItem, error) {
	var doc rssDoc
	if err := xml.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("RSS 解析失败：%w", err)
	}
	return doc.Channel.Items, nil
}

// ==================== Nyaa ====================

func searchNyaa(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := "https://nyaa.si/?page=rss&q=" + url.QueryEscape(q) + "&c=0_0&f=0"
	body, err := httpGet(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	items, err := parseRSS(body)
	if err != nil {
		return nil, err
	}
	return nyaaItems(items, limit), nil
}

func nyaaItems(items []rssItem, limit int) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		hash := it.extra("infoHash")
		seed, _ := strconv.Atoi(it.extra("seeders"))
		leech, _ := strconv.Atoi(it.extra("leechers"))
		size := parseSizeText(it.extra("size"))
		if size == 0 {
			size, _ = strconv.ParseInt(it.Enclosure.Length, 10, 64)
		}
		link := strings.TrimSpace(it.GUID)
		if link == "" {
			link = strings.TrimSpace(it.Link)
		}
		out = append(out, Item{
			Title: strings.TrimSpace(it.Title), Source: "Nyaa", SourceKey: "nyaa", Kind: "torrent",
			Magnet: magnetFor(hash, it.Title), URL: link,
			Size: size, Seeders: seed, Leechers: leech, Date: parseRSSDate(it.PubDate),
			Category: it.extra("category"),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ==================== 动漫花园 ====================

func searchDMHY(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := "https://share.dmhy.org/topics/rss/rss.xml?keyword=" + url.QueryEscape(q)
	body, err := httpGet(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	items, err := parseRSS(body)
	if err != nil {
		return nil, err
	}
	return dmhyItems(items, limit), nil
}

func dmhyItems(items []rssItem, limit int) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		magnet := ""
		if strings.HasPrefix(strings.TrimSpace(it.Enclosure.URL), "magnet:") {
			magnet = strings.TrimSpace(it.Enclosure.URL)
		}
		if magnet == "" {
			// 少数老帖只有 115 链接，正文里能翻出来
			if m := reMagnet.FindString(it.Description); m != "" {
				magnet = m
			}
		}
		links := resourceLinks(it.Description, htmlToText(it.Description))
		out = append(out, Item{
			Title: strings.TrimSpace(it.Title), Source: "动漫花园", SourceKey: "dmhy", Kind: "torrent",
			Magnet: magnet, URL: strings.TrimSpace(it.Link), Links: links,
			Date: parseRSSDate(it.PubDate), Category: "动画",
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ==================== Mikan ====================

func searchMikan(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := "https://mikanani.me/RSS/Search?searchstr=" + url.QueryEscape(q)
	body, err := httpGet(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	items, err := parseRSS(body)
	if err != nil {
		return nil, err
	}
	return mikanItems(items, limit), nil
}

func mikanItems(items []rssItem, limit int) []Item {
	out := make([]Item, 0, len(items))
	for _, it := range items {
		size, _ := strconv.ParseInt(it.Enclosure.Length, 10, 64)
		title := strings.TrimSpace(it.Title)
		if size == 0 {
			// 描述里通常长这样：「…1080P[8.9GB]」
			size = parseSizeText(bracketSize(it.Description))
		}
		out = append(out, Item{
			Title: title, Source: "Mikan", SourceKey: "mikan", Kind: "torrent",
			URL: strings.TrimSpace(it.Link), Torrent: strings.TrimSpace(it.Enclosure.URL),
			Size: size, Date: parseRSSDate(it.PubDate), Category: "动画",
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// bracketSize 从描述里捞出第一个 [] 里的体积文本。
func bracketSize(s string) string {
	open := strings.Index(s, "[")
	if open < 0 {
		return ""
	}
	closeIdx := strings.Index(s[open:], "]")
	if closeIdx < 0 {
		return ""
	}
	return s[open+1 : open+closeIdx]
}

// ==================== 小工具 ====================

func httpGet(ctx context.Context, hc *http.Client, u string, maxBytes int64) ([]byte, error) {
	body, status, err := httpGetStatus(ctx, hc, u, maxBytes)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", hostOf(u), status)
	}
	return body, nil
}

// httpGetStatus 和 httpGet 一样，但非 200 也把 body 带回来。
//
// 有的站被 Cloudflare 挡下时返回 403 + 一个「Just a moment...」页面，
// 只看状态码用户完全不知道发生了什么；解析层要能读到内容才好把话说明白。
func httpGetStatus(ctx context.Context, hc *http.Client, u string, maxBytes int64) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("连不上 %s：%w", hostOf(u), err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

func hostOf(u string) string {
	if p, err := url.Parse(u); err == nil {
		return p.Host
	}
	return u
}

// parseRSSDate 兼容 RSS 的几种时间写法；认不出来就空着，不报错。
func parseRSSDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{
		time.RFC1123Z, time.RFC1123, time.RFC3339,
		"Mon, 02 Jan 2006 15:04:05 -0700",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

// parseSizeText 把 "1.4 GiB" / "2166271375" / "8.9GB" 统一成字节数。
func parseSizeText(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v
	}
	// 拆成「数字 + 单位」
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	if i == 0 {
		return 0
	}
	num, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	unit := strings.ToUpper(strings.TrimSpace(s[i:]))
	mult := float64(1)
	switch {
	case strings.HasPrefix(unit, "T"):
		mult = 1 << 40
	case strings.HasPrefix(unit, "G"):
		mult = 1 << 30
	case strings.HasPrefix(unit, "M"):
		mult = 1 << 20
	case strings.HasPrefix(unit, "K"):
		mult = 1 << 10
	}
	return int64(num * mult)
}
