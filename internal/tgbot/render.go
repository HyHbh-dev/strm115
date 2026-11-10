package tgbot

// 回复文案。全部按 Telegram 的 HTML parse_mode 拼：
// 只允许 b/i/code/a 这几种标签，其它字符必须转义，否则一条片名里的
// `&` 就能让整条消息发送失败（Telegram 报 400 can't parse entities）。

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"strm115/internal/resource"
)

// escape 转义 Telegram HTML 模式里的三个特殊字符。
func escape(s string) string {
	return htmlEscaper.Replace(s)
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// pageView 是「一屏搜索结果」的全部信息。
//
// 分页之后 renderResults 不能假设「给的就是全部」，所以把页码、总条数
// 一起传进来 —— 消息里要写「第 2/3 页」，按钮上的序号也得是全局序号，
// 否则第二屏又出现「1. 2. 3.」，和正文对不上。
type pageView struct {
	Query string
	Res   *resource.Result
	Items []resource.Item // 这一屏的条目
	Start int             // 这一屏第一条在全部结果里的序号（从 1 起）
	Total int             // 全部可选条数
	Page  int             // 当前页（从 1 起）
	Pages int
	// BotUsername 非空时把正文里的种子名做成 deep link，键盘不再放结果按钮；
	// 为空（getMe 没拿到用户名）就降级回原来的「一个结果一个按钮」。
	// 放在 view 里而不是给 renderResults 加参数，是为了不改动它的签名：
	// 现有单测直接调 renderResults(v)，不带用户名就自动走降级分支。
	BotUsername string
	// SubHit 是这次搜索认准的那部片（认片后才非空），CanSubscribe 由 bot
	// 按「订阅能力有没有接上」注入。两者都满足，清单底部才有订阅按钮。
	SubHit       *TitleHit
	CanSubscribe bool
}

// deepLink 拼「点一下就给 bot 发 /start」的链接。
//
// Telegram 的 <a href> 支持 t.me 深链：点开会发 /start <payload>。用它把
// 片名 / 种子名本身变成可点的选定入口，比在消息底部铺一排按钮更贴近
// 「列出清单时直接点名字选定」。payload 只有 tN / pN，远短于 64 字节。
func deepLink(botUsername, prefix string, n int) string {
	return "https://t.me/" + escape(botUsername) + "?start=" + prefix + strconv.Itoa(n)
}

// renderResults 把一屏搜索结果拼成一条消息。
//
// 排版对齐 Torrent Hunt 那类搜索 bot：头两行汇总，下面一条结果两行
// （序号 + 片名 / 体积 · 做种 · 日期）。
//
// 只铺当前这一屏（pageSize 条），既压得住消息长度，也和键盘一一对应：
// 用户看到第几条，键盘上就有第几个名字。
func renderResults(v pageView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "📜 <b>Results for 「%s」</b>\n", escape(v.Query))

	ready := 0
	for _, it := range v.Res.Items {
		if it.ShareCode != "" || it.Magnet != "" || it.Torrent != "" {
			ready++
		}
	}
	fmt.Fprintf(&b, "🔎 %d results · 🧲 %d 条可直接入库", len(v.Res.Items), ready)
	if v.Pages > 1 {
		fmt.Fprintf(&b, " · 📄 第 %d/%d 页", v.Page, v.Pages)
	}
	b.WriteString("\n")
	if v.Res.ChannelQuery != "" {
		fmt.Fprintf(&b, "<i>🌐 频道也按「%s」搜了一遍</i>\n", escape(v.Res.ChannelQuery))
	} else if v.Res.SiteQuery != "" && v.Res.SiteQuery != v.Query {
		fmt.Fprintf(&b, "<i>🌐 种子站按「%s」搜</i>\n", escape(v.Res.SiteQuery))
	}
	if line := siteSummary(v.Res); line != "" {
		b.WriteString(line + "\n")
	}

	// 哪个源挂了要说：用户会以为是「真的没有资源」
	var bad []string
	for _, st := range v.Res.Sources {
		if !st.OK {
			bad = append(bad, st.Name)
		}
	}
	if len(bad) > 0 {
		fmt.Fprintf(&b, "<i>⚠️ %s 这次没连上，结果可能不全</i>\n", escape(strings.Join(bad, "、")))
	}
	if v.Pages > 1 {
		fmt.Fprintf(&b, "<i>📄 第 %d-%d 条 / 共 %d 条</i>\n",
			v.Start, v.Start+len(v.Items)-1, v.Total)
	}
	if v.BotUsername != "" {
		// 片名在正文里就是链接，别再说「点下面的按钮」，也别宣传回复序号。
		b.WriteString("<i>点上面的片名或下面的按钮都能入库，30 分钟内有效。</i>")
	} else {
		b.WriteString("<i>点下面的按钮入库，30 分钟内有效。</i>")
	}

	for i, it := range v.Items {
		b.WriteString(renderItem(v.Start+i, it, v.BotUsername))
	}
	// 这里只兜个底：一屏 8 条正常远到不了 4000 字符，但片名是外部数据，
	// 出现异常长的一条时也不能让整条消息发不出去。
	return cutHTML(b.String(), 3900)
}

// renderItem 是一条结果的正文：片名一行，元数据一行。
//
// 拿到 bot 用户名时，种子名本身就是 deep link：点一下发 /start pN，
// 后端直接走 act() 入库。用户要的是「列清单时直接点种子名选定」，
// 而不是在消息底部再点一排按钮。没用户名时退回纯文本片名（由键盘兜底）。
func renderItem(n int, it resource.Item, botUsername string) string {
	var b strings.Builder
	title := escape(cut(it.Title, 110))
	if botUsername != "" {
		title = `<a href="` + deepLink(botUsername, "p", n) + `">` + title + `</a>`
	}
	fmt.Fprintf(&b, "\n\n<b>%d.</b> %s", n, title)

	meta := make([]string, 0, 4)
	if s := humanSize(it.Size); s != "" {
		meta = append(meta, "🧲 "+s)
	}
	if it.Seeders > 0 {
		meta = append(meta, fmt.Sprintf("🟢 %d", it.Seeders))
	}
	if it.Leechers > 0 {
		meta = append(meta, fmt.Sprintf("🔴 %d", it.Leechers))
	}
	if it.Date != "" {
		meta = append(meta, "📅 "+escape(it.Date))
	}
	if len(meta) > 0 {
		fmt.Fprintf(&b, "\n%s", strings.Join(meta, " · "))
	}
	return b.String()
}

// iconOf 给一条结果挑一个「一眼看出怎么入库」的图标。
func iconOf(it resource.Item) string {
	switch {
	case it.ShareCode != "":
		return "📦" // 115 分享 → 直接转存
	case it.QuarkShare != "":
		return "☁️" // 夸克分享 → 转存到夸克（播放走夸克TV 直链）
	case it.Magnet != "" || it.Torrent != "":
		return "🧲" // 磁力 / 种子 → 离线
	}
	return "🔗" // 只有网页链接，点了会告诉你打不开
}

// pickLabel 是一个结果按钮上的字：「3 · 📦 片名 · 1.5 GB」。
//
// 主体是**片名**：以前按钮上只有序号和体积，用户得在正文和键盘之间
// 来回数才能知道几号是哪部片；现在一屏之内直接点名字就入库。
// 序号留着是给「回复数字」那条老路用的，两边的编号必须一致。
//
// Telegram 的单按钮文字上限是 64 字节，中文一字 3 字节，所以按 46 个
// 字符给预算，超了先砍片名，别把体积砍掉（体积是选片的关键信息）。
func pickLabel(n int, it resource.Item) string {
	icon := iconOf(it)
	size := ""
	if s := humanSize(it.Size); s != "" {
		size = " · " + s
	}
	head := fmt.Sprintf("%d · %s ", n, icon)
	budget := 46 - len([]rune(head)) - len([]rune(size)) - 1 // -1 给截断后的「…」
	if budget < 8 {
		budget = 8
	}
	return head + cut(it.Title, budget) + size
}

// pageKeyboard 拼一屏的键盘。
//
// 每一项都配一个按钮，**不管有没有 bot 用户名**。以前「有用户名就只留翻页」
// 是想着「选定搬到正文的链接上更清爽」，实际用起来是灾难：点正文的深链会
// 让 Telegram 往会话里发一条 `/start p3`，用户看到的就是「我明明点了，怎么
// 还要我发 /start」；在群聊里更糟——深链会跳到和 bot 的私聊，而待选清单是
// 按 chat 存的，切过去就找不到，只会回一句「清单过期了」。
// 按钮走 callback_query，点一下原地完成，不产生任何消息。
func pageKeyboard(v pageView) InlineKeyboard {
	kb := make(InlineKeyboard, 0, len(v.Items)+2)
	for i, it := range v.Items {
		n := v.Start + i
		kb = append(kb, []Button{{Text: pickLabel(n, it), CallbackData: fmt.Sprintf("p:%d", n-1)}})
	}
	if v.Pages > 1 {
		row := make([]Button, 0, 3)
		if v.Page > 1 {
			row = append(row, Button{Text: "◀ 上一页", CallbackData: fmt.Sprintf("pg:%d", v.Page-2)})
		}
		row = append(row, Button{Text: fmt.Sprintf("%d / %d", v.Page, v.Pages), CallbackData: "pgnow"})
		if v.Page < v.Pages {
			row = append(row, Button{Text: "下一页 ▶", CallbackData: fmt.Sprintf("pg:%d", v.Page)})
		}
		kb = append(kb, row)
	}
	last := []Button{{Text: "❌ 都不对，重新搜", CallbackData: "x"}}
	// 订阅按钮和「重新搜」同一行：键盘行数不变，旧清单的翻页/取消按钮位置
	// 也不动。有人已经搜完资源才想追更，这里是最顺手的一处入口。
	if v.CanSubscribe && v.SubHit != nil && v.SubHit.ID > 0 {
		last = append(last, Button{Text: "⭐ 订阅这部", CallbackData: "sbp"})
	}
	return append(kb, last)
}

// tmdbView 是「一屏 TMDB 认片候选」。
type tmdbView struct {
	Query       string
	Hits        []TitleHit // 这一屏的候选
	Start       int        // 这一屏第一条的全局序号（从 1 起）
	Total       int
	Page        int // 从 1 起
	Pages       int
	BotUsername string
	// 没接订阅能力时按钮不出现，免得给一个点了没反应的「⭐」。
	CanSubscribe bool
}

// renderTMDB 拼 TMDB 认片候选清单。
//
// 格式对齐用户要的 Torrent Search Bot 回复：一行汇总，下面每部一行
// 「N. 片名（年份） 电影/剧集 8.5分」。有 bot 用户名时把片名本身做成
// deep link，点一下发 /start tN —— 选定发生在正文的名字上，不是底部按钮。
// 翻页与取消才用底部 inline keyboard。
func renderTMDB(v tmdbView) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔍 「%s」TMDB 找到 %d 部 · 第 %d/%d 页，选一部：",
		escape(v.Query), v.Total, v.Page, v.Pages)
	for i, h := range v.Hits {
		n := v.Start + i
		name := escape(cut(h.Title, 110))
		if v.BotUsername != "" {
			name = `<a href="` + deepLink(v.BotUsername, "t", n) + `">` + name + `</a>`
		}
		fmt.Fprintf(&b, "\n<b>%d.</b> %s", n, name)
		if h.Year != "" {
			fmt.Fprintf(&b, "（%s）", escape(h.Year))
		}
		b.WriteString(" " + tmdbKindLabel(h.Kind))
		if h.Rating > 0 {
			fmt.Fprintf(&b, " %.1f分", h.Rating)
		}
	}
	if v.BotUsername != "" {
		b.WriteString("\n\n<i>点上面的片名或下面的按钮都能选定，30 分钟内有效。</i>")
	} else {
		// 拿不到 bot 用户名就拼不出 deep link：片名降级成纯文本，选定改用
		// 底部按钮（tmdbKeyboard），并把原因写清楚，免得用户对着死链发懵。
		b.WriteString("\n\n<i>没拿到 Bot 用户名，片名暂时点不了；用下面的按钮选一部，30 分钟内有效。</i>")
	}
	return cutHTML(b.String(), 3900)
}

// tmdbKindLabel 把 TMDB 的 kind 写成中文。剧集也认 "series"，兼容别处映射。
func tmdbKindLabel(kind string) string {
	if kind == "tv" || kind == "series" {
		return "剧集"
	}
	return "电影"
}

// tmdbKeyboard 拼认片候选的键盘。
//
// 和 pageKeyboard 一样：**每部片都配按钮**。正文里的片名链接保留（点它也行），
// 但按钮才是那条不产生 `/start` 消息、在群聊里也不丢上下文的路。
func tmdbKeyboard(v tmdbView) InlineKeyboard {
	kb := make(InlineKeyboard, 0, len(v.Hits)+2)
	for i, h := range v.Hits {
		n := v.Start + i
		row := []Button{{
			Text:         fmt.Sprintf("%d. %s", n, cut(h.Title, 24)),
			CallbackData: fmt.Sprintf("t:%d", n),
		}}
		// 每部片同一行再给一个订阅：想直接追更的人不用先「选片 → 搜资源 →
		// 再回网页订阅」。放在第二个位置，第一个按钮仍是原来的 t:N。
		if v.CanSubscribe && h.ID > 0 {
			row = append(row, Button{
				Text: "⭐ 订阅", CallbackData: fmt.Sprintf("sb:%d", n),
			})
		}
		kb = append(kb, row)
	}
	if v.Pages > 1 {
		row := make([]Button, 0, 3)
		if v.Page > 1 {
			row = append(row, Button{Text: "◀ 上一页", CallbackData: fmt.Sprintf("tp:%d", v.Page-2)})
		}
		row = append(row, Button{Text: fmt.Sprintf("%d / %d", v.Page, v.Pages), CallbackData: "tpnow"})
		if v.Page < v.Pages {
			row = append(row, Button{Text: "下一页 ▶", CallbackData: fmt.Sprintf("tp:%d", v.Page)})
		}
		kb = append(kb, row)
	}
	return append(kb, []Button{{Text: "❌ 都不对，重新搜", CallbackData: "tx"}})
}

// cutHTML 截断**已经拼好**的 HTML，只在换行处断。
//
// 直接按字符数切是危险的：`&amp;` 被切成 `&am` 会让 Telegram 直接 400
// （can't parse entities），而这条消息本来是列表，截在换行处损失最小。
func cutHTML(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	t := string(r[:max])
	if i := strings.LastIndex(t, "\n"); i > 0 {
		t = t[:i]
	}
	return t + "\n…"
}

// siteSummary 明说「种子站这轮到底怎么了」。
//
// 以前只列连不上的源；可 nyaa / dmhy / mikan 这类站连得上、只是对一部
// 美剧英文名返回 0 条，Status.OK 是 true，机器人一个字都不提 —— 用户
// 只看到一屏 TG 分享，自然以为「种子站没搜」。0 条也要说清楚。
func siteSummary(res *resource.Result) string {
	stations, failed, withItems := 0, 0, 0
	for _, st := range res.Sources {
		if st.Kind != "torrent" {
			continue
		}
		stations++
		switch {
		case !st.OK:
			failed++
		case st.Count > 0:
			withItems++
		}
	}
	if stations == 0 {
		return ""
	}
	hits := 0
	for _, it := range res.Items {
		if it.Kind == "torrent" {
			hits++
		}
	}
	switch {
	case hits == 0 && failed > 0:
		return fmt.Sprintf("<i>⚠️ 种子站 %d 个里 %d 个没连上，其余没匹配</i>", stations, failed)
	case hits == 0:
		return fmt.Sprintf("<i>⚠️ 种子站 %d 个站这次都没匹配（换英文原名 / 配代理再试）</i>", stations)
	default:
		return fmt.Sprintf("<i>🌱 种子站 %d/%d 个站有结果，共 %d 条</i>", withItems, stations, hits)
	}
}

// renderIngest 是「入库信息」：成功之后告诉用户到底发生了什么。
func renderIngest(ing *Ingest) string {
	if ing == nil {
		return "✅ 完成。"
	}
	var b strings.Builder
	if ing.Pending {
		b.WriteString("⏳ <b>已提交离线下载</b>\n")
	} else {
		b.WriteString("✅ <b>已入库</b>\n")
	}
	if ing.Title != "" {
		fmt.Fprintf(&b, "<b>%s</b>\n", escape(cut(ing.Title, 120)))
	}

	var lines []string
	if ing.Files > 0 {
		lines = append(lines, fmt.Sprintf("%d 个文件", ing.Files))
	}
	if ing.Dir != "" {
		lines = append(lines, "目录："+escape(ing.Dir))
	}
	if ing.Account != "" {
		lines = append(lines, "账号："+escape(ing.Account))
	}
	if len(lines) > 0 {
		b.WriteString(strings.Join(lines, " · "))
		b.WriteString("\n")
	}
	if ing.Pending {
		b.WriteString("<i>115 正在下，完成后会自动同步并推给你。</i>")
	}
	if ing.Note != "" {
		b.WriteString("\n" + escape(ing.Note))
	}
	return strings.TrimSpace(b.String())
}

// renderFail 把失败原因原样带上：在手机上没法看服务端日志。
func renderFail(title, code string, err error) string {
	head := "❌ <b>入库失败</b>\n"
	if title != "" {
		head += "<b>" + escape(cut(title, 120)) + "</b>\n"
	} else if code != "" {
		head += "分享码 <code>" + escape(code) + "</code>\n"
	}
	body := "原因："
	if err != nil {
		body += escape(err.Error())
	} else {
		body += "未知"
	}
	return head + body + "\n\n换个源再试，或者发片名让我重新搜。"
}

// ==================== 输入识别 ====================

// parseCommand 认 /xxx 和 /xxx@BotName 两种写法（群里 @ 到具体 bot 的写法）。
func parseCommand(text string) (cmd, arg string, ok bool) {
	if !strings.HasPrefix(text, "/") {
		return "", "", false
	}
	line := strings.TrimPrefix(text, "/")
	// 一行里的第一个空格分开命令与参数；参数允许跨多行（贴一段带换行的文案）
	head, rest, _ := strings.Cut(line, " ")
	if name, _, hasAt := strings.Cut(head, "@"); hasAt {
		head = name
	}
	cmd = strings.ToLower(strings.TrimSpace(head))
	if cmd == "" {
		return "", "", false
	}
	return cmd, strings.TrimSpace(rest), true
}

// reDeepLink 认 /start 的 deep link 参数：t3 选 TMDB 第 3 部、p7 选资源第 7 条。
//
// 只匹配开头，允许后面跟尾巴（部分客户端会带成 t3@botname）：调用方先
// TrimSpace，再用它取前缀和数字，尾部一律忽略，免得一个 @ 就让链接失效。
// 数字是正文里那个 1 起的全局序号，不是内部下标。
var reDeepLink = regexp.MustCompile(`^([tp])(\d+)`)

// 离线下载能直接吃的三种链接。
var (
	reMagnet  = regexp.MustCompile(`(?i)magnet:\?[^\s<>"']+`)
	reED2K    = regexp.MustCompile(`(?i)ed2k://[^\s<>"']+`)
	reTorrent = regexp.MustCompile(`(?i)https?://[^\s<>"']+\.torrent(\?[^\s<>"']*)?`)
)

// findOfflineURI 从一段文字里挖出可离线下载的链接。
//
// 顺序：磁力 > ed2k > .torrent。贴一整段发布文案时，通常是「标题 + 磁力」，
// 拿到磁力就够了，标题由离线任务名自己带上。
func findOfflineURI(text string) string {
	if m := reMagnet.FindString(text); m != "" {
		return m
	}
	if m := reED2K.FindString(text); m != "" {
		return m
	}
	if m := reTorrent.FindString(text); m != "" {
		return m
	}
	return ""
}

// looksLikeURL 判断这段文字是不是「一条链接」—— 是的话就不该拿去搜片名。
func looksLikeURL(text string) bool {
	t := strings.TrimSpace(text)
	if strings.ContainsAny(t, " \n\t") {
		return false // 带空格的更像片名
	}
	lower := strings.ToLower(t)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "ftp://")
}

// humanSize 把字节数写成 12.4 GB。0 返回空串（模板里据此决定要不要显示）。
func humanSize(n int64) string {
	if n <= 0 {
		return ""
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

// renderNameLookup 渲染 /name 的结果。
//
// 英文名一律放进 <code>：Telegram 里点一下就能复制，这是这条命令的主要用途
// （拿去改名、拼搜索词）。中文名 / 年份 / 类型只是用来确认「是不是这一部」。
func renderNameLookup(query string, hits []TitleHit) string {
	// /name 的数据源是 language=en-US 的搜索结果：Title 就是英文名，
	// Original 是原始语言名（日文 / 韩文片常与英文名不同），拿来当备注。
	en := func(h TitleHit) string {
		if s := strings.TrimSpace(h.Title); s != "" {
			return s
		}
		return strings.TrimSpace(h.Original)
	}
	zh := func(h TitleHit) string {
		t := strings.TrimSpace(h.Original)
		if t == "" || t == en(h) {
			return ""
		}
		return t
	}

	var b strings.Builder
	if len(hits) == 1 {
		h := hits[0]
		fmt.Fprintf(&b, "🔤 英文名：<code>%s</code>", escape(en(h)))
		var tail []string
		if t := zh(h); t != "" {
			tail = append(tail, escape(t))
		}
		if h.Year != "" {
			tail = append(tail, escape(h.Year))
		}
		tail = append(tail, tmdbKindLabel(h.Kind))
		if len(tail) > 0 {
			b.WriteString("\n" + strings.Join(tail, " · "))
		}
		return b.String()
	}

	fmt.Fprintf(&b, "🔤 「%s」TMDB 找到 %d 个，英文名：", escape(cut(query, 60)), len(hits))
	for i, h := range hits {
		fmt.Fprintf(&b, "\n<b>%d.</b> <code>%s</code>", i+1, escape(en(h)))
		if t := zh(h); t != "" {
			fmt.Fprintf(&b, " · %s", escape(t))
		}
		if h.Year != "" {
			fmt.Fprintf(&b, " · %s", escape(h.Year))
		}
		b.WriteString(" · " + tmdbKindLabel(h.Kind))
	}
	b.WriteString("\n\n<i>点英文名即可复制。要入库就用 /search 片名。</i>")
	return b.String()
}

// ==================== 订阅按钮与「从哪一集开始」 ====================

// nameKeyboard 给 /name 的英文名清单配订阅按钮。
//
// /name 本来只回一段纯文本；查完英文名顺手订阅是很自然的下一步，所以
// 给每条候选一个按钮，序号和正文里的 1./2. 对齐。没接订阅能力时返回 nil，
// 老行为（只有文本）一点不变。
func nameKeyboard(hits []TitleHit, can bool) InlineKeyboard {
	if !can || len(hits) == 0 {
		return nil
	}
	kb := make(InlineKeyboard, 0, len(hits))
	for i, h := range hits {
		if h.ID <= 0 {
			continue
		}
		label := fmt.Sprintf("⭐ 订阅 %d", i+1)
		if len(hits) == 1 {
			label = "⭐ 订阅这部"
		}
		kb = append(kb, []Button{{
			Text: label, CallbackData: fmt.Sprintf("sbn:%d", i+1),
		}})
	}
	return kb
}

// 一季几百集时别把键盘撑爆：前 60 集够选起播线了，剩下的用「从头追」。
const maxSubEpisodeButtons = 60

func isTVHit(h TitleHit) bool { return h.Kind == "tv" || h.Kind == "series" }

func subTitle(h TitleHit) string {
	if strings.TrimSpace(h.Title) != "" {
		return h.Title
	}
	return h.Original
}

func renderSubSeasons(hit TitleHit, seasons []SubSeason) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⭐ 订阅《%s》", escape(cut(subTitle(hit), 90)))
	b.WriteString("\n\n<b>从哪一集开始订阅？</b>")
	b.WriteString("\n起始集及之前不下载，也不会进搜索队列。")
	if len(seasons) > 0 {
		b.WriteString("\n点「从头追」订阅全部集，或选一季再挑集号。")
	} else {
		b.WriteString("\nTMDB 没给季集数，只能从头追。")
	}
	return b.String()
}

func subSeasonKeyboard(seasons []SubSeason) InlineKeyboard {
	kb := InlineKeyboard{}
	kb = append(kb, []Button{{Text: "✅ 从头追（全部集）", CallbackData: "sbg"}})
	row := []Button{}
	for _, s := range seasons {
		row = append(row, Button{
			Text:         fmt.Sprintf("S%02d · %d集", s.Season, s.Episodes),
			CallbackData: fmt.Sprintf("sbs:%d", s.Season),
		})
		if len(row) == 4 {
			kb = append(kb, row)
			row = []Button{}
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	return append(kb, []Button{{Text: "❌ 取消", CallbackData: "sbx"}})
}

func renderSubEpisodes(hit TitleHit, season, episodes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⭐ 订阅《%s》· 第 %d 季",
		escape(cut(subTitle(hit), 90)), season)
	b.WriteString("\n\n<b>选起始集</b>：这一集及之前不下载。")
	if episodes > maxSubEpisodeButtons {
		fmt.Fprintf(&b, "\n<i>这一季有 %d 集，只列出前 %d 集。</i>",
			episodes, maxSubEpisodeButtons)
	}
	b.WriteString("\n订阅后按 TMDB 的单集播出日生成你的追更日历，到播出日自动搜资源。")
	return b.String()
}

func subEpisodeKeyboard(season, episodes int) InlineKeyboard {
	kb := InlineKeyboard{}
	row := []Button{}
	for e := 1; e <= episodes && e <= maxSubEpisodeButtons; e++ {
		row = append(row, Button{
			Text:         fmt.Sprintf("E%02d", e),
			CallbackData: fmt.Sprintf("sbe:%d:%d", season, e),
		})
		if len(row) == 6 {
			kb = append(kb, row)
			row = []Button{}
		}
	}
	if len(row) > 0 {
		kb = append(kb, row)
	}
	return append(kb, []Button{
		{Text: "◀ 返回选季", CallbackData: "sbback"},
		{Text: "❌ 取消", CallbackData: "sbx"},
	})
}

// renderSubResult 订阅回执。errText 非空时是失败，否则按 Already / 剧集 /
// 电影三种情况给不同的下一句 —— 用户最关心「接下来会发生什么」。
func renderSubResult(hit TitleHit, res *SubscribeResult, ws, we int, errText string) string {
	name := escape(cut(subTitle(hit), 90))
	if errText != "" {
		return "❌ 订阅《" + name + "》失败：" + escape(cut(errText, 180)) +
			"\n\n可以到「发现与订阅 → 订阅」里再试一次。"
	}
	if res == nil {
		return "❌ 订阅《" + name + "》没有返回结果。"
	}
	if res.Already {
		return "⭐ 《" + name + "》已经在订阅里了\n\n" +
			"到「发现与订阅 → 订阅」里改画质、起播线或暂停，追更日历也在那边看。"
	}
	shown := escape(cut(firstNonEmptyString(res.Title, subTitle(hit)), 90))
	if isTVHit(hit) || res.CalendarTotal > 0 {
		start := "从头追（全部集）"
		if ws > 0 || we > 0 {
			start = fmt.Sprintf("从 S%02dE%02d 之后开始", ws, we)
		}
		return fmt.Sprintf("✅ 已订阅《%s》（剧集）\n\n%s · 已生成 %d 集专有追更日历\n"+
			"到播出日自动搜资源，没搜到的集每天重试一次。",
			shown, start, res.CalendarTotal)
	}
	return "✅ 已订阅《" + shown + "》（电影）\n\n有资源时会自动提交离线下载。"
}

// firstNonEmptyString 只给订阅回执用：/name 那条路径可能只给了英文名。
func firstNonEmptyString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}
