package resource

// 两个只有 HTML 搜索页、没有 RSS/JSON 接口的站：
//
//   - UIndex    https://uindex.org/search.php?search=…   表格里直接给磁力；
//   - TheRarBG  https://therarbg.com/get-posts/?keywords=…  列表只给详情页，
//     磁力要去 /post-detail/… 里翻，所以另外并发补一次。
//
// 为什么是正则而不是 DOM 解析器：两个站的表格列名/顺序各不一样
// （UIndex 带 category + uploaded 两列，TheRarBG 带 8 列），但每行里
// 「磁力 href / details.php 链接 / td class」这三样是稳定的。按内容特征
// 抓比按列序号抓抗改版，也不用为了两站引一个 HTML 解析库。
//
// UIndex 对部分出口 IP 会弹 Cloudflare 挑战页，这里明确报出来 —— 一个
// 403 加一句「换个出口或走代理」比空结果有用得多。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

const (
	uindexBase   = "https://uindex.org"
	therarbgBase = "https://therarbg.com"
	// 详情页要一条一条翻，只补前几条就够用了：列表按做种排序，
	// 后面的条目用户多半也不会点。剩下的仍然给详情页链接。
	maxDetailFetch = 12
	detailWorkers  = 4
)

var (
	reHTMLRow = regexp.MustCompile(`(?is)<tr[^>]*>(.*?)</tr>`)
	reCell    = regexp.MustCompile(`(?is)<t[dh][^>]*class="([^"]*)"[^>]*>(.*?)</t[dh]>`)
	reMagnetH = regexp.MustCompile(`(?i)href="(magnet:\?[^"]+)"`)
	reAnyHref = regexp.MustCompile(`(?i)magnet:\?xt=urn:btih:[^"'\s<>]+`)

	// UIndex：标题在 /details.php 的锚文本里，磁力在另一个 <a> 的 href 里。
	reUIndexDetail = regexp.MustCompile(`(?is)<a[^>]*href="(/details\.php\?id=\d+)"[^>]*>(.*?)</a>`)
	reUIndexSize   = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*(B|KB|MB|GB|TB)`)
	reISODate      = regexp.MustCompile(`\d{4}-\d{2}-\d{2}`)

	// TheRarBG：标题写在 <a title="…"> 上，比锚文本干净（锚文本后面还挂着 M.Q.A）。
	reRarBGName = regexp.MustCompile(`(?is)<a[^>]*href="(/post-detail/[^"]+)"[^>]*title="([^"]*)"`)
)

// ==================== UIndex ====================

func searchUIndex(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := uindexBase + "/search.php?search=" + url.QueryEscape(q) + "&c=0"
	body, status, err := httpGetStatus(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	if isCloudflare(body) {
		return nil, errors.New("uindex.org 被 Cloudflare 拦下（换个出口 IP 或走代理再试）")
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 %d", hostOf(u), status)
	}
	return parseUIndex(body, q, limit), nil
}

// isCloudflare 认挑战页。有些配置下它会用 200 返回，所以不能只看状态码。
func isCloudflare(body []byte) bool {
	return bytes.Contains(body, []byte("Just a moment")) ||
		bytes.Contains(body, []byte("challenges.cloudflare.com"))
}

// parseUIndex 拆出来是为了能拿真页面做单测（不用真的连 uindex.org）。
func parseUIndex(body []byte, q string, limit int) []Item {
	out := make([]Item, 0, limit)
	for _, row := range reHTMLRow.FindAllSubmatch(body, -1) {
		block := row[1]
		m := reMagnetH.FindSubmatch(block)
		if m == nil {
			continue // 表头行、广告行都没有磁力，直接跳过
		}
		magnet := html.UnescapeString(string(m[1]))

		title, page := "", ""
		if d := reUIndexDetail.FindSubmatch(block); d != nil {
			title = strings.TrimSpace(htmlToText(string(d[2])))
			page = uindexBase + html.UnescapeString(string(d[1]))
		}
		if title == "" {
			title = magnetName(magnet)
		}
		if title == "" {
			continue
		}

		it := Item{
			Title: title, Source: "UIndex", SourceKey: "uindex", Kind: "torrent",
			Magnet: magnet, URL: page,
		}
		for _, c := range reCell.FindAllStringSubmatch(string(block), -1) {
			cls, val := strings.ToLower(c[1]), strings.TrimSpace(htmlToText(c[2]))
			switch {
			case cellHas(cls, "size", "hp-td-size"):
				if sz := reUIndexSize.FindString(val); sz != "" {
					it.Size = parseSizeText(sz)
				}
			case cellHas(cls, "seed", "hp-td-seed"):
				it.Seeders = atoiLoose(val)
			case cellHas(cls, "leech", "hp-td-leech"):
				it.Leechers = atoiLoose(val)
			case cellHas(cls, "date", "uploaded", "hp-td-date") && it.Date == "":
				it.Date = reISODate.FindString(val)
			}
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// ==================== TheRarBG ====================

func searchTheRarBG(ctx context.Context, hc *http.Client, q string, limit int) ([]Item, error) {
	u := therarbgBase + "/get-posts/?keywords=" + url.QueryEscape(q)
	body, err := httpGet(ctx, hc, u, 8<<20)
	if err != nil {
		return nil, err
	}
	items := parseTheRarBGList(body, limit)
	fillMagnetsFromDetails(ctx, hc, items)
	return items, nil
}

func parseTheRarBGList(body []byte, limit int) []Item {
	out := make([]Item, 0, limit)
	for _, row := range reHTMLRow.FindAllSubmatch(body, -1) {
		block := string(row[1])
		// 只认带 /post-detail/ 链接的行：分页条、表头、广告位都没有它，
		// 比按 tr 的 class 判断抗改版。
		n := reRarBGName.FindStringSubmatch(block)
		if n == nil {
			continue
		}
		page := therarbgBase + html.UnescapeString(n[1])
		title := strings.TrimSpace(html.UnescapeString(n[2]))
		if title == "" {
			continue
		}
		it := Item{
			Title: title, Source: "TheRarBG", SourceKey: "therarbg", Kind: "torrent",
			URL: page,
		}
		for _, c := range reCell.FindAllStringSubmatch(block, -1) {
			cls, val := strings.ToLower(c[1]), strings.TrimSpace(htmlToText(c[2]))
			// class 是单个字母（n/k/d/t/s/se/le），必须按整个词比，
			// 否则 "s" 会把 "size"、"seed" 之类的列全吃掉。
			exact := map[string]bool{}
			for _, f := range strings.Fields(cls) {
				exact[f] = true
			}
			switch {
			case exact["s"]:
				it.Size = parseSizeText(val)
			case exact["se"]:
				it.Seeders = atoiLoose(val)
			case exact["le"]:
				it.Leechers = atoiLoose(val)
			case exact["d"]:
				it.Date = reISODate.FindString(val)
			case exact["k"] && it.Category == "":
				it.Category = val
			}
		}
		out = append(out, it)
		if len(out) >= limit {
			break
		}
	}
	return out
}

// fillMagnetsFromDetails 并发翻详情页补磁力。
//
// TheRarBG 不补的话列表里一条能直接入库的都没有（只有详情页链接），搜索结果
// 就废了；磁力猫正常能靠 slug 拼出来，只有 URL 规则变了才走到这里。
// 已经有磁力的条目直接跳过，不白花请求。
// 限 4 并发 + 最多 12 条：一次搜索不该把人家站点打崩，也别让用户等太久。
func fillMagnetsFromDetails(ctx context.Context, hc *http.Client, items []Item) {
	idx := make([]int, 0, maxDetailFetch)
	for i := range items {
		if items[i].URL == "" || items[i].Magnet != "" {
			continue
		}
		idx = append(idx, i)
		if len(idx) >= maxDetailFetch {
			break
		}
	}
	var wg sync.WaitGroup
	sem := make(chan struct{}, detailWorkers)
	for _, i := range idx {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			body, err := httpGet(ctx, hc, items[i].URL, 2<<20)
			if err != nil {
				return
			}
			m := reAnyHref.FindString(string(body))
			if m == "" {
				// 磁力猫这类站把正文也塞在 window.atob 里，原样搜是搜不到的
				m = reAnyHref.FindString(decodeClmPayload(body))
			}
			if m != "" {
				items[i].Magnet = html.UnescapeString(m)
			}
		}(i)
	}
	wg.Wait()
}

// ==================== 小工具 ====================

// cellHas 判断 td 的 class 里有没有目标词。UIndex 用 hp-td-size 这种长名，
// 用 Contains 就够；TheRarBG 的单字母类名走调用方自己的 exact 比较。
func cellHas(cls string, names ...string) bool {
	for _, n := range names {
		if strings.Contains(cls, n) {
			return true
		}
	}
	return false
}

// atoiLoose 把 "21,716" / "465" / "1 234" 里的数字抠出来。
func atoiLoose(s string) int {
	s = strings.NewReplacer(",", "", "\u00a0", "", " ", "").Replace(s)
	n, _ := strconv.Atoi(s)
	return n
}

// magnetName 从磁力的 dn= 参数里兜一个标题：详情页链接缺失时至少不是空串。
func magnetName(magnet string) string {
	u, err := url.Parse(magnet)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(u.Query().Get("dn"))
}
