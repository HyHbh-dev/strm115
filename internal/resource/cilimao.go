package resource

// 磁力猫（clm）：DHT 索引站。没有 RSS / JSON 接口，只有 HTML 搜索页，
// 而且有两层「不用 JS 就看不懂」的东西，必须自己解开：
//
//  1. 关键词不是明文塞 URL 的，要先 base64（去掉 = 填充）再放进 ?word=。
//     直接传中文会返回一张「keyword error」的提示页，看起来像站点挂了。
//  2. 结果列表也不在 HTML 里：服务端把整段 <ul> 先 URL 编码、再 base64，
//     塞进 window.atob("…")，由前端 JS 解出来 innerHTML 进 #search-box。
//     这里照做一遍 —— 拿不到这段 payload 就一条都读不出来。
//
// 回报是它比 TheRarBG 好抓：详情页 slug 就是 infohash 的 base32 小写形式
// （/information/savdhddo… ←→ 902A338C…），磁力能直接拼，不必逐条翻详情页。
// slug 将来若换成随机串，还有一条「翻详情页补磁力」的兜底（见 searchClm）。

import (
	"context"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const (
	// 磁力猫的入口域名经常换（clm02 / clm10 / …），只钉一个；哪天不通了改这行。
	// 它只有 HTTP，没有 HTTPS 监听，别顺手改成 https。
	clmBase = "http://clm10.xyz"
)

var (
	// 结果 payload：window.atob("JTNDdWwlMjBpZCUzRC…")
	reClmPayload = regexp.MustCompile(`(?is)window\.atob\("([A-Za-z0-9+/=\s]+)"\)`)
	// 列表行。没用 <tr>：它给的是 <ul><li>，和 UIndex / TheRarBG 的表格不是一套。
	reClmRow  = regexp.MustCompile(`(?is)<li[^>]*>(.*?)</li>`)
	reClmLink = regexp.MustCompile(`(?is)<a[^>]*href="(/information/[^"]+)"[^>]*>(.*?)</a>`)
	reClmISO  = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)
)

func searchClm(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := clmBase + "/search?word=" + url.QueryEscape(clmWord(q))
	body, status, err := httpGetStatus(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", hostOf(u), status)
	}
	items := parseClm(body, limit)
	// 正常情况 slug 已经换出磁力，这里一条都不会翻；只有当站点改了 URL
	// 规则、拼不出 infohash 时才会真的去请求详情页。
	fillMagnetsFromDetails(ctx, hc, items)
	return items, nil
}

// parseClm 拆出来是为了能拿真页面做单测（不用真连 clm10.xyz）。
func parseClm(body []byte, limit int) []Item {
	page := decodeClmPayload(body)
	out := make([]Item, 0, limit)
	for _, row := range reClmRow.FindAllStringSubmatch(page, -1) {
		m := reClmLink.FindStringSubmatch(row[1])
		if m == nil {
			// 「很抱歉，没有搜索到…」那页里也有 <li>（提示条目），没有 /information/ 链接
			continue
		}
		title := strings.TrimSpace(htmlToText(m[2]))
		if title == "" {
			continue
		}
		href := html.UnescapeString(m[1])
		slug := strings.TrimPrefix(href, "/information/")
		out = append(out, Item{
			Title: title, Source: "磁力猫", SourceKey: "clm", Kind: "torrent",
			URL: clmBase + href, Magnet: magnetFor(base32InfoHash(slug), title),
			Size: parseSizeText(fieldAfter(row[1], "文件大小：")),
			Date: clmDate(row[1]),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// decodeClmPayload 把 window.atob("…") 里的内容解回 HTML。
//
// 解不出来就原样返回：站点哪天改成服务端直出，下面的正则照样能用，
// 不必跟着改。
func decodeClmPayload(body []byte) string {
	m := reClmPayload.FindSubmatch(body)
	if m == nil {
		return string(body)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(string(m[1])), ""))
	if err != nil {
		return string(body)
	}
	// 外层还有一层 URL 编码（中文标题是 %E9%AB%98… 那种）。用 PathUnescape
	// 而不是 QueryUnescape：标题里常有 "[国粤多音轨+中文字幕]" 这样的加号，
	// QueryUnescape 会把它变成空格，前端 JS 用的是 decodeURIComponent，不会。
	if s, err := url.PathUnescape(string(raw)); err == nil {
		return s
	}
	return string(raw)
}

// clmWord 把关键词变成站点认的搜索词：UTF-8 → base64 → 去掉 = 填充。
//
//	python: base64.b64encode("空枪".encode()).decode().rstrip("=") == "56m65p6q"
func clmWord(q string) string {
	return base64.RawStdEncoding.EncodeToString([]byte(strings.TrimSpace(q)))
}

// base32InfoHash 把详情页 slug 还原成 infohash。
//
// infohash 是 20 字节，base32 出来正好 32 个字符，站点用小写放在 URL 里。
// 认不出来（长度不对 / 不是合法 base32）就返回空串，调用方据此退回翻详情页。
func base32InfoHash(slug string) string {
	if len(slug) != 32 {
		return ""
	}
	b, err := base32.StdEncoding.DecodeString(strings.ToUpper(slug))
	if err != nil || len(b) != 20 {
		return ""
	}
	return strings.ToUpper(hex.EncodeToString(b))
}

// fieldAfter 取标签后面那一小段文本。
//
// 站点把「标签」和「值」放在相邻的两个节点里：
//
//	<em>文件大小：</em>17.23 GB<em>创建时间：</em>2026-10-24
//
// 所以跳过标签后面的闭合标签，再取到下一个 '<' 为止。
func fieldAfter(block, label string) string {
	i := strings.Index(block, label)
	if i < 0 {
		return ""
	}
	rest := strings.TrimSpace(block[i+len(label):])
	if strings.HasPrefix(rest, "</") {
		if j := strings.Index(rest, ">"); j >= 0 {
			rest = strings.TrimSpace(rest[j+1:])
		}
	}
	if j := strings.Index(rest, "<"); j >= 0 {
		rest = rest[:j]
	}
	return strings.TrimSpace(html.UnescapeString(rest))
}

// clmDate 先按「创建时间：」取；标签万一改名，退一步从整行里捞第一个 ISO 日期。
func clmDate(block string) string {
	if d := reClmISO.FindString(fieldAfter(block, "创建时间：")); d != "" {
		return d
	}
	return reClmISO.FindString(htmlToText(block))
}
