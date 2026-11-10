package resource

// Telegram 公开频道的网页预览（t.me/s/<channel>）不需要登录、不需要 bot token，
// 还支持 ?q= 在频道内搜索 —— 这是唯一一条不用 API Key 就能用 TG 当资源库的路子。
//
// 代价是要解析 HTML。好在预览页的结构十几年没变过：一条消息一个
// div.tgme_widget_message_wrap，正文在 div.tgme_widget_message_text 里，
// 时间在 <time datetime>，消息地址在 data-post="channel/123"。
// 这里用正则而不是 HTML 解析器：页面结构固定，正则更少依赖，坏了一眼能看出来。

import (
	"context"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"strm115/internal/quark"
	"time"
	"unicode/utf8"

	"strm115/internal/linkparse"
)

var (
	rePost     = regexp.MustCompile(`data-post="([^"]+)"`)
	reTimeAttr = regexp.MustCompile(`datetime="([^"]+)"`)
	reText     = regexp.MustCompile(`(?s)<div class="tgme_widget_message_text[^"]*"[^>]*>(.*?)</div>`)
	reHref     = regexp.MustCompile(`href="(https?://[^"]+)"`)
	reAnyTag   = regexp.MustCompile(`(?s)<[^>]+>`)
	reBr       = regexp.MustCompile(`(?i)<br\s*/?>`)
	reMagnet   = regexp.MustCompile(`magnet:\?xt=urn:btih:[0-9a-zA-Z]+[^\s"'<>]*`)

	// Torrent Search Bot 🔎 / Torrent Hunt 风格的结果帖：
	// 抬头是「📜 Results for xxx」，每条信息用 💾 / 🟢 / 🔴 / 📅 标字段。
	// 这类帖子转发进频道后，第一行不是片名，按老规则取会把「Results for」
	// 当成标题，体积/做种也全丢。这里把抬头和字段行挑出来单独处理。
	reHuntHead  = regexp.MustCompile(`(?i)^\s*(?:📜|🔎|🔍|📋|✨|\s)*\s*(?:results?\s+for|searching|搜索结果|结果)\b.*$`)
	reHuntField = regexp.MustCompile(`^\s*(?:💾|🟢|🔴|📅|✨|🔗|📦|📂|🧲|🌱)`)
	reHuntSize  = regexp.MustCompile(`💾\s*([\d.]+\s*[KMGTP]?i?B)`)
	reHuntSeed  = regexp.MustCompile(`🟢\s*([\d,]+)`)
	reHuntLeech = regexp.MustCompile(`🔴\s*([\d,]+)`)
	reHuntDate  = regexp.MustCompile(`📅\s*(\d{4}-\d{2}-\d{2})`)

	// 普通资源帖不带 💾 图标：体积常写在文件名方括号里、或「文件大小：」后面。
	// 以前这类帖子 Size 恒为 0，前端 fmtSize 显示「—」，体积筛选也跟着失效。
	// 单位只认 K/M/G/T 开头（B、iB 可省略），[1080P] / [x264] / [HDR] 不会误判。
	reBracketSize = regexp.MustCompile(`(?i)\[\s*(\d+(?:\.\d+)?\s*[KMGT]i?B?)\s*\]`)
	reLabeledSize = regexp.MustCompile(`(?i)(?:文件大小|大小|容量|体积|size)\s*[:：]?\s*(\d+(?:\.\d+)?\s*[KMGT]i?B?)`)
	reBareSize    = regexp.MustCompile(`(?i)(?:^|[\s·|,，])(\d+(?:\.\d+)?\s*[KMGT]i?B)(?:\s|$|[\s·|,，)])`)

	// 频道里的说明帖也会命中 ?q= 搜索（例如「如何投稿：」里带片名），
	// 但它没有可入库的链接，摆进结果只是白占一个序号按钮。
	reChannelNotice = regexp.MustCompile(`^(?:如何|怎么|怎样|公告|通知|进群|加群|投稿|声明|置顶|欢迎|常见|规则|关于|FAQ|faq)`)
)

// tgBase 是 t.me 的基地址。单测里换成 httptest 服务器，才能在本地复现
// 「机器人没有 /s/ 预览页」这条分支 —— 真去连 t.me 的测试既慢又会被限流。
var tgBase = "https://t.me"

// searchTelegram 在一个频道里搜关键词。
//
// 频道用 ?q= 搜索时，Telegram 返回的是「匹配的消息」，顺序基本按消息 ID 升序，
// 也就是越老的越靠前 —— 对找片来说没用，所以这里自己按时间倒序再截断。
func searchTelegram(ctx context.Context, hc *http.Client, channel, q string, limit int) ([]Item, error) {
	u := tgBase + "/s/" + url.PathEscape(channel)
	if strings.TrimSpace(q) != "" {
		u += "?q=" + url.QueryEscape(q)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "+
		"(KHTML, like Gecko) Chrome/122.0.0.0 Safari/537.36")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")

	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连不上 t.me：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, fmt.Errorf("t.me 限流了，过几分钟再试")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("t.me 返回 %d", resp.StatusCode)
	}

	// /s/ 只是「公开频道」的网页预览。机器人（@xxxbot）、私有群、个人号没有这个页面，
	// t.me 会 302 到 https://t.me/<name> 的落地页（标题写着 Launch @xxx），
	// 页面里一条消息都没有。这种情况必须显式认出来：不认的话界面上只显示
	// 「0 条」，用户会以为是搜索词不对，反复改关键词也永远搜不到。
	if got := strings.TrimSuffix(resp.Request.URL.Path, "/"); !strings.EqualFold(got, "/s/"+channel) {
		return nil, fmt.Errorf("@%s 不是公开频道（机器人 / 私有群没有网页预览），t.me 搜不到里面的内容", channel)
	}

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}

	items := parseChannel(string(raw))
	sort.SliceStable(items, func(i, j int) bool { return items[i].Date > items[j].Date })
	if len(items) > limit {
		items = items[:limit]
	}
	for i := range items {
		items[i].Source = "@" + channel
		items[i].SourceKey = "tg:" + channel
		items[i].Kind = "tg"
	}
	return items, nil
}

// splitMessages 按消息容器切块。
// 不用正则做这个：Go 的 RE2 不支持前瞻，靠 strings.Split 反而更稳 ——
// 每条消息以固定的 div 开头，切完再削掉页面尾巴（</main> 之后是页脚）。
func splitMessages(page string) []string {
	const marker = `<div class="tgme_widget_message_wrap`
	parts := strings.Split(page, marker)
	out := make([]string, 0, len(parts))
	for _, p := range parts[1:] {
		if i := strings.Index(p, "</main>"); i >= 0 {
			p = p[:i]
		}
		out = append(out, p)
	}
	return out
}

// parseChannel 把预览页拆成一条条消息。
func parseChannel(page string) []Item {
	blocks := splitMessages(page)
	out := make([]Item, 0, len(blocks))
	for _, b := range blocks {
		textHTML := ""
		if m := reText.FindStringSubmatch(b); len(m) > 1 {
			textHTML = m[1]
		}
		plain := htmlToText(textHTML)
		if plain == "" {
			continue
		}
		post := ""
		if m := rePost.FindStringSubmatch(b); len(m) > 1 {
			post = m[1]
		}
		date := ""
		if m := reTimeAttr.FindStringSubmatch(b); len(m) > 1 {
			date = shortDate(m[1])
		}

		title, snippet := splitTitle(plain)
		links := resourceLinks(b, plain)
		it := Item{
			Title:   title,
			Snippet: snippet,
			Date:    date,
			Links:   links,
		}
		applyHuntMeta(&it, plain)
		if post != "" {
			it.URL = "https://t.me/" + post
		}
		if m := reMagnet.FindString(plain); m != "" {
			it.Magnet = m
		}
		// 115 分享就地认出来：前端拿到 share_code 就能直接转存，
		// 不必再让人点开 t.me 的帖子手动复制。
		if sh := findShare115(links, plain); sh != nil {
			it.ShareCode, it.SharePwd = sh.ShareCode, sh.Password
			it.ShareURL, it.ShareHost = sh.URL, sh.Host
		}
		// 夸克分享同理就地认出来：前端能直接「存到夸克」，
		// 机器人选定之后也能走夸克那条转存链路。
		if qs, ok := findShareQuark(links, plain); ok {
			it.QuarkShare, it.QuarkPwd, it.QuarkURL = qs.PwdID, qs.Passcode, qs.URL
		}
		// 纯公告帖：既没磁力、没 115 分享，也没任何可复制的网盘链接。
		// 只按标题前缀判断，且要求三条全空，避免误伤标题带「关于」的资源帖。
		if it.Magnet == "" && it.ShareCode == "" && len(it.Links) == 0 &&
			reChannelNotice.MatchString(it.Title) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// splitTitle 取第一行当标题（TG 资源帖第一行通常就是「名称：片名 规格」），
// 剩下的压成一行当摘要。
func splitTitle(plain string) (string, string) {
	lines := strings.Split(plain, "\n")
	var title, rest []string
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		if title == nil {
			// Torrent Search Bot 风格的结果帖：抬头行和 💾/🟢 字段行不是片名，
			// 跳过它们才能取到真正的第一条资源标题。
			if reHuntHead.MatchString(ln) || reHuntField.MatchString(ln) {
				continue
			}
			ln = strings.TrimPrefix(ln, "名称：")
			ln = strings.TrimPrefix(ln, "名称:")
			ln = strings.TrimSpace(ln)
			title = append(title, ln)
			continue
		}
		rest = append(rest, ln)
	}
	t := ""
	if len(title) > 0 {
		t = cutRunes(title[0], 110)
	}
	s := strings.Join(rest, " ")
	// 摘要里常带「描述：」「简介:」这类前缀，留着显得啰嗦
	s = strings.TrimPrefix(s, "描述：")
	s = strings.TrimPrefix(s, "简介：")
	return t, cutRunes(s, 180)
}

// applyHuntMeta 从 Torrent Search Bot 风格的图标字段里补元数据。
//
// 只在字段还没值时补：普通 TG 资源帖偶尔也会带 💾/🟢，
// 但 RSS/链接解析出来的值更可信，不该被一行文案覆盖。
func applyHuntMeta(it *Item, plain string) {
	if it.Size == 0 {
		if m := reHuntSize.FindStringSubmatch(plain); m != nil {
			it.Size = parseSizeText(m[1])
		}
	}
	if it.Size == 0 {
		it.Size = sizeFromText(plain)
	}
	if it.Seeders == 0 {
		if m := reHuntSeed.FindStringSubmatch(plain); m != nil {
			it.Seeders = atoiLoose(m[1])
		}
	}
	if it.Leechers == 0 {
		if m := reHuntLeech.FindStringSubmatch(plain); m != nil {
			it.Leechers = atoiLoose(m[1])
		}
	}
	if it.Date == "" {
		if m := reHuntDate.FindStringSubmatch(plain); m != nil {
			it.Date = m[1]
		}
	}
}

// sizeFromText 从普通资源帖的文字里认体积。
//
// 顺序按可信度：文件名方括号 > 带「文件大小/大小/容量」标签 > 裸的「2.1 GB」。
// 裸匹配要求单位带 B 且前后是分隔符，避免把分辨率、年份之类的数字吃进来。
func sizeFromText(plain string) int64 {
	for _, re := range []*regexp.Regexp{reBracketSize, reLabeledSize, reBareSize} {
		if m := re.FindStringSubmatch(plain); m != nil {
			if v := parseSizeText(m[1]); v > 0 {
				return v
			}
		}
	}
	return 0
}

// resourceLinks 只留「能拿去存/下」的链接：网盘分享页和磁力。
// t.me 自身的外链、投稿人主页统统丢掉 —— 那不是资源。
func resourceLinks(block, plain string) []string {
	seen := map[string]bool{}
	var out []string
	add := func(u string) {
		u = strings.TrimSpace(html.UnescapeString(u))
		if u == "" || seen[u] || !isResourceLink(u) {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	for _, m := range reHref.FindAllStringSubmatch(block, -1) {
		add(m[1])
	}
	for _, m := range reMagnet.FindAllString(plain, -1) {
		add(m)
	}
	return out
}

func isResourceLink(u string) bool {
	lu := strings.ToLower(u)
	if strings.HasPrefix(lu, "magnet:") {
		return true
	}
	for _, host := range []string{
		"115.com/s/", "115cdn.com/s/", "anxia.com/s/", // 115
		"pan.baidu.com/s/", "yun.baidu.com/s/", // 百度
		"alipan.com/s/", "aliyundrive.com/s/", // 阿里
		"pan.quark.cn/s/",        // 夸克
		"cloud.189.cn/",          // 天翼
		"caiyun.139.com/",        // 移动云盘
		".torrent",               // 种子直链
		"share.dmhy.org/topics/", // 动漫花园详情页
		"mikanani.me/Download/",  // Mikan 种子
		"nyaa.si/download/",      // Nyaa 种子
	} {
		if strings.Contains(lu, host) {
			return true
		}
	}
	return false
}

// htmlToText 把消息正文转成纯文本：换行保留，其余标签丢掉，实体还原。
func htmlToText(s string) string {
	if s == "" {
		return ""
	}
	s = reBr.ReplaceAllString(s, "\n")
	s = reAnyTag.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\u00a0", " ")
	// 连续空行压成一个，不然摘要会很难看
	for strings.Contains(s, "\n\n\n") {
		s = strings.ReplaceAll(s, "\n\n\n", "\n\n")
	}
	return strings.TrimSpace(s)
}

func shortDate(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02")
	}
	if len(s) >= 10 {
		return s[:10]
	}
	return s
}

// findShare115 从一条 TG 消息里认 115 分享。
//
// 先看链接（href 里的最可信），再把正文当补充：帖子常把链接做成锚文本、
// 提取码单独写一行，两边各拿一半才齐。都没有就看正文里的「分享码：xxx」。
func findShare115(links []string, plain string) *linkparse.Share115 {
	var hit *linkparse.Share115
	for _, u := range links {
		if sh := linkparse.Parse115Share(u); sh != nil {
			hit = sh
			break
		}
	}
	fromText := linkparse.Parse115Share(plain)
	switch {
	case hit == nil:
		return fromText
	case hit.Password == "" && fromText != nil:
		hit.SetPassword(fromText.Password)
	}
	return hit
}

// findShareQuark 从一条 TG 消息里认夸克分享。
//
// 和 115 那套一样：先看 href（最可信），再拿正文兜提取码 —— 帖子常把链接做成
// 锚文本、提取码单独一行写「提取码：abcd」，两边各取一半才齐。
func findShareQuark(links []string, plain string) (quark.Share, bool) {
	var hit quark.Share
	found := false
	for _, u := range links {
		if sh, ok := quark.ParseShare(u); ok {
			hit, found = sh, true
			break
		}
	}
	fromText, okText := quark.ParseShare(plain)
	switch {
	case !found && okText:
		return fromText, true
	case !found:
		return quark.Share{}, false
	case hit.Passcode == "" && okText:
		hit.Passcode = fromText.Passcode
	}
	return hit, true
}

// cutRunes 按字符截断，不会把汉字切成半个。
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return strings.TrimSpace(s)
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n])) + "…"
}
