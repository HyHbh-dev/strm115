package tgbot

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"strm115/internal/linkparse"
	"strm115/internal/resource"
)

// nameLookupLimit 是英文名最多列几条。
//
// 只列 5 条：这条命令的产出是「一个能复制的名字」，不是候选墙；
// 列太多反而要用户自己分辨哪条才是他说的那部。
const nameLookupLimit = 5

// commands 是 / 菜单。setMyCommands 之后用户点输入框旁的按钮就能选。
var commands = []BotCommand{
	{Command: "start", Description: "开始使用 / 说明"},
	{Command: "help", Description: "支持的写法"},
	{Command: "search", Description: "按片名搜：/search 沙丘2"},
	{Command: "name", Description: "查 TMDB 英文名：/name 沙丘2"},
	{Command: "id", Description: "查看这个会话的 Chat ID"},
}

// onMessage 处理一条普通消息。
func (b *Bot) onMessage(ctx context.Context, a *api, msg *Message) {
	chat := msg.Chat.ID
	if !b.authorize(ctx, a, chat) {
		return
	}

	text := strings.TrimSpace(msg.Content())
	if text == "" {
		b.reply(ctx, a, chat, "只认文字：发 115 分享链接、磁力，或者一个片名。")
		return
	}

	if cmd, arg, ok := parseCommand(text); ok {
		b.onCommand(ctx, a, chat, cmd, arg)
		return
	}
	b.route(ctx, a, chat, text)
}

// route 把一段文字归到三条链路之一。
//
// 顺序有讲究：先认 115 链接（最明确、结果最好），再认种子/磁力（能直接离线），
// 最后才当成片名去搜 —— 搜索是最慢、最不确定的一条。
func (b *Bot) route(ctx context.Context, a *api, chat int64, text string) {
	if sh := linkparse.Parse115Share(text); sh != nil {
		b.doTransfer(ctx, a, chat, 0, sh.ShareCode, sh.Password, "")
		return
	}
	if uri := findOfflineURI(text); uri != "" {
		b.doOffline(ctx, a, chat, 0, uri, "")
		return
	}
	if looksLikeURL(text) {
		b.reply(ctx, a, chat, "这条链接认不出来。\n"+
			"支持：115 分享链接（115.com/s/…）、磁力（magnet:…）、ed2k、.torrent 直链。\n"+
			"想按名字找片子就只发片名。")
		return
	}
	// 纯数字优先当成「选第几条」：按钮上带着序号，老习惯打数字也能用。
	// 没有待选清单（或越界）就退回当片名搜，发「20」也可能是片名。
	if n, ok := parsePickNumber(text); ok && b.selectByNumber(ctx, a, chat, n) {
		return
	}
	b.doSearch(ctx, a, chat, text)
}

// onCommand 处理 / 开头的命令。
func (b *Bot) onCommand(ctx context.Context, a *api, chat int64, cmd, arg string) {
	switch cmd {
	case "start":
		// deep link：点正文里的片名 / 种子名会给 bot 发 /start tN 或 /start pN。
		// 白名单已在 onMessage 开头验过，这里只做路由，不绕过授权。
		// 尾部可能带 @botname 之类的尾巴，reDeepLink 只取开头的 t/p + 数字。
		if m := reDeepLink.FindStringSubmatch(strings.TrimSpace(arg)); m != nil {
			if n, err := strconv.Atoi(m[2]); err == nil && n > 0 {
				switch m[1] {
				case "t":
					b.selectTMDB(ctx, a, chat, n)
				case "p":
					if !b.selectByNumber(ctx, a, chat, n) {
						b.reply(ctx, a, chat, "这份清单过期了，发片名重新搜一次。")
					}
				}
				return
			}
		}
		b.reply(ctx, a, chat, "👋 <b>strm115</b> —— 在聊天窗口里把片子丢进库。\n\n"+
			"发我一个 <b>115 分享链接</b>、<b>磁力</b>，或者直接发<b>片名</b>：\n"+
			"• 分享链接 → 直接转存进你的网盘\n"+
			"• 磁力 / ed2k → 提交 115 离线下载\n"+
			"• 片名 → 先从 TMDB 候选里点片名认准那部，再点资源名直接入库\n\n"+
			"这个会话的 Chat ID：<code>"+fmt.Sprint(chat)+"</code>")
	case "help":
		b.reply(ctx, a, chat, helpText())
	case "id":
		b.reply(ctx, a, chat, "这个会话的 Chat ID：<code>"+fmt.Sprint(chat)+"</code>")
	case "search":
		q := strings.TrimSpace(arg)
		if q == "" {
			b.reply(ctx, a, chat, "用法：<code>/search 沙丘2</code>\n或者直接发片名。")
			return
		}
		b.doSearch(ctx, a, chat, q)
	case "name", "n":
		b.doNameLookup(ctx, a, chat, arg)
	default:
		b.reply(ctx, a, chat, "不认识这个命令。发 <code>/help</code> 看看支持什么。")
	}
}

func helpText() string {
	return "<b>支持的写法</b>\n\n" +
		"<b>1. 115 分享链接</b>\n" +
		"<code>https://115.com/s/swxxxx?password=abcd</code>\n" +
		"直接转存到资源目录，回复里带文件数与目标目录。\n\n" +
		"<b>2. 磁力 / ed2k / 种子直链</b>\n" +
		"<code>magnet:?xt=urn:btih:…</code>、<code>ed2k://…</code>、<code>https://….torrent</code>\n" +
		"提交 115 离线下载，下完自动同步并通知你。\n\n" +
		"<b>3. 片名</b>\n" +
		"直接发 <code>沙丘2</code> 或 <code>沙丘2 2024</code>：\n" +
		"先从 <b>TMDB 候选</b>里点片名认准那部（带年份、类型、评分），\n" +
		"再在资源清单里<b>直接点种子名</b>就入库。结果多时用「下一页」翻。\n\n" +
		"<b>命令</b>\n" +
		"/search 片名 · /name 片名（查 TMDB 英文名）· /id · /help"
}

// doNameLookup 只查 TMDB 的英文名：不搜资源、不弹候选、不入库。
//
// 为什么单独一个命令：改名、拼种子搜索词、跟人核对片名时，用户要的是一个
// 能直接复制走的英文标题；走 /search 反而会被候选按钮和资源清单带偏。
func (b *Bot) doNameLookup(ctx context.Context, a *api, chat int64, query string) {
	q := strings.TrimSpace(query)
	if q == "" {
		b.reply(ctx, a, chat, "用法：<code>/name 沙丘2</code>\n"+
			"也可以写 <code>/n 沙丘2</code>；中文名、英文名都认。")
		return
	}
	if b.deps.TMDBName == nil {
		b.reply(ctx, a, chat, "TMDB 没接上（可能没配 API Key），查不了英文名。")
		return
	}

	hits, err := b.deps.TMDBName(ctx, q, nameLookupLimit)
	if err != nil {
		b.log.Warn("查 TMDB 英文名失败", "err", err, "chat", chat, "query", q)
		b.reply(ctx, a, chat, "TMDB 查询失败："+escape(cut(err.Error(), 120)))
		return
	}
	if len(hits) == 0 {
		b.reply(ctx, a, chat, "TMDB 没找到「"+escape(cut(q, 60))+"」。\n"+
			"试试只发英文名，或者去掉副标题 / 季集号。")
		return
	}
	b.reply(ctx, a, chat, renderNameLookup(q, hits))
}

// doSearch 处理一个片名：先 TMDB 认片，再搜资源。
//
// 为什么要多一步认片：用户随手打的「蝙蝠侠」可能指七八部片子，直接拿去搜
// 资源会混进一堆不相干的东西。先列 TMDB 候选让人点准那部，再用它规范的
// 中文名 + 英文原名去搜，命中率高得多。
//
// 认片没接上、TMDB 挂了、或者没候选，一律降级成原来的「直接按片名搜资源」——
// 绝不能因为一个可选环节让 bot 哑掉。
func (b *Bot) doSearch(ctx context.Context, a *api, chat int64, query string) {
	if b.deps.SearchTMDB != nil && b.doTMDBCandidates(ctx, a, chat, query) {
		return
	}
	b.searchResources(ctx, a, chat, query, "")
}

// doTMDBCandidates 问 TMDB 要候选、发一屏让人点。
//
// 返回 true = 候选已经发出去了，调用方就此打住；false = 该降级直接搜资源。
// 失败（err / 空结果 / 发送失败）都只记日志、不回错误文案：用户要的是资源，
// 不是一段「TMDB 挂了」的说明，静默降级最不打扰。
func (b *Bot) doTMDBCandidates(ctx context.Context, a *api, chat int64, query string) bool {
	hits, err := b.deps.SearchTMDB(ctx, query, searchLimit)
	if err != nil {
		b.log.Warn("TMDB 认片失败，降级为直接搜资源", "err", err, "chat", chat)
		return false
	}
	if len(hits) == 0 {
		b.log.Info("TMDB 没有候选，降级为直接搜资源", "chat", chat, "query", query)
		return false
	}

	p := &tmdbPick{query: query, hits: hits, at: b.now()}
	v := p.view(b.botUsername())
	msgID, err := a.send(ctx, chat, renderTMDB(v), tmdbKeyboard(v))
	if err != nil {
		b.log.Warn("发送 TMDB 候选失败，降级为直接搜资源", "err", err, "chat", chat)
		return false
	}
	p.msgID = msgID
	b.putTMDBPick(chat, p)
	return true
}

// selectTMDB 按全局序号选中一部 TMDB 候选，再用它的名字去搜资源。
//
// n 是正文里那个 1 起的全局序号（deep link tN / 降级按钮 t:N 都用这套编号）。
// 中文名当 query、original_title 当 en 传给后端：en 是种子站换名、TG 频道
// 英文搜索的关键。年份 / 类型只用于候选展示，不拼进 query —— 带年份的词
// （「三体 2023」）在 TMDB 反而一条都搜不到，别把认片的成果又弄丢。
func (b *Bot) selectTMDB(ctx context.Context, a *api, chat int64, n int) bool {
	p, ok := b.tmdbPickFor(chat)
	if !ok || n < 1 || n > len(p.hits) {
		b.reply(ctx, a, chat, "这份候选过期了，发片名重新搜一次。")
		return false
	}
	h := p.hits[n-1]
	b.dropTMDBPick(chat) // 选中即作废：一份候选只该锁定一部

	q := h.Title
	if q == "" {
		q = h.Original
	}
	if q == "" {
		q = p.query // 两个名字都空的极端兜底
	}
	b.log.Info("已选定 TMDB 候选，开始搜资源", "chat", chat, "title", q, "en", h.Original)
	b.searchResources(ctx, a, chat, q, h.Original)
	return true
}

// searchResources 走真正的资源搜索，结果分屏列出来。
//
// 有 bot 用户名时，正文里的种子名本身就是 deep link，键盘只留翻页 / 取消；
// 没用户名时降级回「每行一个结果按钮」，选定功能不丢。
// en 是认片锁定的英文原名，为 nil / 空时后端自己按中文名去换。
func (b *Bot) searchResources(ctx context.Context, a *api, chat int64, query, en string) {
	if b.deps.Search == nil {
		b.reply(ctx, a, chat, "搜索功能没接上（后端没提供）。")
		return
	}
	// 多取一些再挑：limit 是「每个源的上限」，不是「消息里能放几条」。
	res, err := b.deps.Search(ctx, query, en, searchLimit)
	if res == nil || len(res.Items) == 0 {
		msg := "❌ No results for <b>" + escape(query) + "</b>."
		if err != nil {
			msg += "\n⚠️ 原因：" + escape(err.Error())
		}
		msg += "\n\n🔎 换个写法再试：中文名加年份，或者直接发英文原名。"
		b.reply(ctx, a, chat, msg)
		return
	}

	p := &pick{query: query, res: res, items: balanceOrder(res.Items)}
	v := p.view()
	v.BotUsername = b.botUsername()
	msgID, err := a.send(ctx, chat, renderResults(v), pageKeyboard(v))
	if err != nil {
		b.log.Warn("发送搜索结果失败", "err", err, "chat", chat)
		return
	}
	// 状态要在消息发出去之后存：deep link / 回调里带的是全局序号，
	// 消息 id 是这条清单的身份，翻页、点选都靠它对上。
	p.msgID = msgID
	b.putPick(chat, p)
}

// onCallback 处理内联键盘。
//
// 两组回调：TMDB 候选的 tp:（翻页）/ t:（降级选定）/ tx（取消），以及
// 资源清单的 p:（选定）/ pg:（翻页）/ x（取消）。p:/pg:/x 是老消息上的按钮
// 留下的格式，必须继续认 —— 不然旧清单上的按钮点了没反应。
//
// 先 answer 再干活：不 answer 的话用户手机上那个按钮会一直转圈，
// 而一次入库可能要几十秒（搜索 + 转存 + 离线提交）。
func (b *Bot) onCallback(ctx context.Context, a *api, q *CallbackQuery) {
	if q == nil || q.Message == nil {
		return
	}
	if !b.authorizeCallback(ctx, a, q) {
		return
	}
	chat := q.Message.Chat.ID
	msgID := q.Message.MessageID

	switch {
	case strings.HasPrefix(q.Data, "tp:"):
		// TMDB 候选翻页：页码存回状态，再按当前 bot 用户名重渲染。
		page, ok := atoiOK(strings.TrimPrefix(q.Data, "tp:"))
		p, live := b.tmdbPickFor(chat)
		if !live || !ok {
			_ = a.answer(ctx, q.ID, "这份候选过期了，发片名重新搜一次")
			return
		}
		b.setTMDBPage(chat, page)
		p.page = page
		v := p.view(b.botUsername())
		if err := a.edit(ctx, chat, msgID, renderTMDB(v), tmdbKeyboard(v)); err != nil {
			b.log.Debug("TMDB 候选翻页改写失败", "err", err, "chat", chat)
		}
		_ = a.answer(ctx, q.ID, "")

	case q.Data == "tpnow":
		if p, live := b.tmdbPickFor(chat); live {
			v := p.view(b.botUsername())
			_ = a.answer(ctx, q.ID, fmt.Sprintf("第 %d/%d 页", v.Page, v.Pages))
			return
		}
		_ = a.answer(ctx, q.ID, "")

	case q.Data == "tx":
		b.dropTMDBPick(chat)
		if err := a.edit(ctx, chat, msgID,
			"❌ <b>这次不要了</b>\n直接发一个新的片名就能重新搜。", nil); err != nil {
			b.log.Debug("取消 TMDB 候选改写失败", "err", err, "chat", chat)
		}
		_ = a.answer(ctx, q.ID, "已取消")

	case strings.HasPrefix(q.Data, "t:"):
		// 没拿到 bot 用户名时拼不出 deep link，候选改用按钮选（保证功能不丢）。
		n, ok := atoiOK(strings.TrimPrefix(q.Data, "t:"))
		if !ok || n < 1 {
			_ = a.answer(ctx, q.ID, "这条候选不对")
			return
		}
		_ = a.answer(ctx, q.ID, "")
		b.selectTMDB(ctx, a, chat, n)

	case strings.HasPrefix(q.Data, "p:"):
		idx, ok := atoiOK(strings.TrimPrefix(q.Data, "p:"))
		p, live := b.takePick(chat, msgID)
		if !live || !ok || idx < 0 || idx >= len(p.items) {
			_ = a.answer(ctx, q.ID, "这条清单过期了，发片名重新搜一次")
			return
		}
		it := p.items[idx]
		_ = a.answer(ctx, q.ID, "开始处理："+cut(it.Title, 40))
		// 原地改写这条清单：按钮随之消失，不可能对同一条点两次。
		b.act(ctx, a, chat, msgID, it)

	case strings.HasPrefix(q.Data, "pg:"):
		page, ok := atoiOK(strings.TrimPrefix(q.Data, "pg:"))
		p, live := b.pickFor(chat, msgID)
		if !live || !ok {
			_ = a.answer(ctx, q.ID, "这条清单过期了，发片名重新搜一次")
			return
		}
		p.page = page
		v := p.view()
		v.BotUsername = b.botUsername()
		if err := a.edit(ctx, chat, msgID, renderResults(v), pageKeyboard(v)); err != nil {
			// 「message is not modified」也会走到这里：页码没变，不算错
			b.log.Debug("翻页改写失败", "err", err, "chat", chat)
		}
		_ = a.answer(ctx, q.ID, "")

	case q.Data == "pgnow":
		if p, live := b.pickFor(chat, msgID); live {
			_ = a.answer(ctx, q.ID, fmt.Sprintf("第 %d/%d 页", p.view().Page, p.view().Pages))
			return
		}
		_ = a.answer(ctx, q.ID, "")

	default: // "x"：都不对，重新搜
		b.dropPick(chat, msgID)
		if err := a.edit(ctx, chat, msgID,
			"❌ <b>这次搜索不要了</b>\n直接发一个新的片名就能重新搜。", nil); err != nil {
			b.log.Debug("取消改写失败", "err", err, "chat", chat)
		}
		_ = a.answer(ctx, q.ID, "已取消")
	}
}

// pages 一共有几屏。
func (p *pick) pages() int {
	n := (len(p.items) + pageSize - 1) / pageSize
	if n < 1 {
		n = 1
	}
	return n
}

// view 取当前这一屏。page 越界时夹回最后一屏 —— 用户手里的按钮
// 可能来自一个结果数已经变了的旧状态。
func (p *pick) view() pageView {
	pages := p.pages()
	page := p.page
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	lo := page * pageSize
	hi := lo + pageSize
	if hi > len(p.items) {
		hi = len(p.items)
	}
	return pageView{
		Query: p.query, Res: p.res, Items: p.items[lo:hi],
		Start: lo + 1, Total: len(p.items), Page: page + 1, Pages: pages,
	}
}

// tmdbView 取 TMDB 候选当前这一屏，越界页码同样夹回最后一屏。
func (p *tmdbPick) view(botUsername string) tmdbView {
	pages := (len(p.hits) + pageSize - 1) / pageSize
	if pages < 1 {
		pages = 1
	}
	page := p.page
	if page < 0 {
		page = 0
	}
	if page >= pages {
		page = pages - 1
	}
	lo := page * pageSize
	hi := lo + pageSize
	if hi > len(p.hits) {
		hi = len(p.hits)
	}
	return tmdbView{
		Query: p.query, Hits: p.hits[lo:hi],
		Start: lo + 1, Total: len(p.hits),
		Page: page + 1, Pages: pages,
		BotUsername: botUsername,
	}
}

// selectByNumber 处理「用户回复了一个序号」。
//
// 按钮已经能点片名，这条路是给习惯打数字的人留的兼容：序号和按钮上的
// 前缀用的是同一套全局编号。返回 false 表示这条数字不该被当成选片
// （没有搜索结果、或者越界），交给上层继续当片名去搜 —— 发「20」也
// 可能是在搜《20》。
func (b *Bot) selectByNumber(ctx context.Context, a *api, chat int64, n int) bool {
	p, ok := b.takeLast(chat)
	if !ok || n < 1 || n > len(p.items) {
		return false
	}
	it := p.items[n-1]
	// 入库结果单独发一条新消息（msgID=0），清单留着让用户还能回下一个序号。
	b.act(ctx, a, chat, 0, it)
	return true
}

// parsePickNumber 认「3」「9」这种纯序号，最多两位。
//
// 片名里带数字（《沙丘2》）不会被误判：带任何非数字字符都返回 false。
func parsePickNumber(text string) (int, bool) {
	t := strings.TrimSpace(text)
	if len(t) == 0 || len(t) > 2 {
		return 0, false
	}
	n := 0
	for _, c := range t {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if n <= 0 {
		return 0, false
	}
	return n, true
}

// balanceOrder 把一整轮结果按「种子站 / TG 交错」重新排。
//
// 直接按相关性排的问题是：一个 TG 频道一次就能给 20 条 115 分享，
// 相关性又常常不输种子站，于是前三屏一条磁力都没有 —— 用户会以为
// 「种子站没搜」。这里按 kind 分成两队交错取：种子站排前面（它才是
// 本项目的一键离线入口），队内保持相关性排序，谁先用完就全给另一队。
func balanceOrder(items []resource.Item) []resource.Item {
	var tor, tg []resource.Item
	for _, it := range items {
		if it.Kind == "torrent" {
			tor = append(tor, it)
		} else {
			tg = append(tg, it)
		}
	}
	if len(tor) == 0 || len(tg) == 0 {
		return items // 只有一边，原样返回，别白白复制一份
	}
	out := make([]resource.Item, 0, len(items))
	ti, gi := 0, 0
	for ti < len(tor) || gi < len(tg) {
		if ti < len(tor) {
			out = append(out, tor[ti])
			ti++
		}
		if gi < len(tg) {
			out = append(out, tg[gi])
			gi++
		}
	}
	return out
}

// act 对一条搜索结果执行入库。msgID>0 时原地改写那条列表消息。
func (b *Bot) act(ctx context.Context, a *api, chat, msgID int64, it resource.Item) {
	switch {
	case it.ShareCode != "":
		b.doTransfer(ctx, a, chat, msgID, it.ShareCode, it.SharePwd, it.Title)
	case it.QuarkShare != "":
		b.doQuarkTransfer(ctx, a, chat, msgID, it.QuarkURL, it.QuarkPwd, it.Title)
	case it.Magnet != "":
		b.doOffline(ctx, a, chat, msgID, it.Magnet, it.Title)
	case it.Torrent != "":
		b.doOffline(ctx, a, chat, msgID, it.Torrent, it.Title)
	case it.URL != "":
		b.say(ctx, a, chat, msgID,
			"⚠️ <b>"+escape(it.Title)+"</b>\n这条只有网页链接，没有磁力也没有 115 分享，我没法直接入库。\n"+
				"打开看看：<a href=\""+escape(it.URL)+"\">原文</a>")
	default:
		b.say(ctx, a, chat, msgID,
			"⚠️ <b>"+escape(it.Title)+"</b>\n这条没有可用的下载方式。换一条试试。")
	}
}

func (b *Bot) doTransfer(ctx context.Context, a *api, chat, msgID int64, code, pwd, title string) {
	if b.deps.Transfer == nil {
		b.say(ctx, a, chat, msgID, "转存功能没接上（后端没提供）。")
		return
	}
	ing, err := b.deps.Transfer(ctx, code, pwd)
	if err != nil {
		b.say(ctx, a, chat, msgID, renderFail(title, code, err))
		return
	}
	b.say(ctx, a, chat, msgID, renderIngest(ing))
}

// doQuarkTransfer 转存一条夸克分享。
//
// 夸克和 115 是两条互不相干的链路：115 走离线/转存，夸克走「网页 Cookie 转存
// 进夸克网盘 → 夸克TV 接口取 302 直链播放」。所以这里不复用 doTransfer
// —— 传过去的 share_code 对夸克毫无意义，只会换回一句看不懂的错误。
func (b *Bot) doQuarkTransfer(ctx context.Context, a *api, chat, msgID int64, url, pwd, title string) {
	if b.deps.TransferQuark == nil {
		b.say(ctx, a, chat, msgID, "夸克转存没接上（后端没提供）。")
		return
	}
	ing, err := b.deps.TransferQuark(ctx, url, pwd)
	if err != nil {
		b.say(ctx, a, chat, msgID, renderFail(title, "", err))
		return
	}
	b.say(ctx, a, chat, msgID, renderIngest(ing))
}

func (b *Bot) doOffline(ctx context.Context, a *api, chat, msgID int64, uri, title string) {
	if b.deps.Offline == nil {
		b.say(ctx, a, chat, msgID, "离线下载功能没接上（后端没提供）。")
		return
	}
	ing, err := b.deps.Offline(ctx, uri, title)
	if err != nil {
		b.say(ctx, a, chat, msgID, renderFail(title, "", err))
		return
	}
	b.say(ctx, a, chat, msgID, renderIngest(ing))
}

// ==================== 白名单 ====================

// authorize 判断这个会话能不能用，并在「还没主人」时认领。
func (b *Bot) authorize(ctx context.Context, a *api, chat int64) bool {
	allow := b.allowlist(ctx)
	for _, id := range allow {
		if id == chat {
			return true
		}
	}
	if len(allow) == 0 && b.deps.Bind != nil {
		if err := b.deps.Bind(ctx, chat); err != nil {
			b.log.Error("绑定 Telegram 会话失败", "chat", chat, "err", err)
			b.reply(ctx, a, chat, "绑定失败："+escape(err.Error()))
			return false
		}
		b.log.Info("已把第一个会话设为机器人主人", "chat", chat)
		b.reply(ctx, a, chat, "✅ 已绑定这个会话（Chat ID <code>"+fmt.Sprint(chat)+"</code>）。\n"+
			"之后的转存、离线都归你一个人，别的会话发消息会被拒绝。\n\n"+
			"发 115 分享链接、磁力，或者直接发片名试试。")
		return true
	}
	b.log.Warn("拒绝了未授权的 Telegram 会话", "chat", chat)
	b.reply(ctx, a, chat, "⛔️ 这个会话没有权限。\n"+
		"把这个 ID 加到「消息配置 → 机器人」的白名单里才能用：\n"+
		"<code>"+fmt.Sprint(chat)+"</code>")
	return false
}

// authorizeCallback 是按钮版本：不回复（用户已经看到列表了），只记日志。
func (b *Bot) authorizeCallback(ctx context.Context, a *api, q *CallbackQuery) bool {
	allow := b.allowlist(ctx)
	for _, id := range allow {
		if id == q.Message.Chat.ID {
			return true
		}
	}
	_ = a.answer(ctx, q.ID, "没有权限")
	return false
}

func (b *Bot) allowlist(ctx context.Context) []int64 {
	if b.deps.Allow == nil {
		return nil
	}
	return b.deps.Allow(ctx)
}

// ==================== 会话状态 ====================

// 搜索结果的存活时间。半小时足够一个人从看到清单到点片名。
const pickTTL = 30 * time.Minute

// putPick 记下一条清单消息，同时把它设成这个会话「最近一次搜索」。
//
// 两个索引各管一条路：picks 按消息 id 存，翻页和点按钮靠它对上具体那条
// 清单（同一个会话里可能躺着两次搜索）；last 按会话存最近一条，给不带
// 按钮的数字回复用。顺手清掉过期的，避免长期运行时 map 只涨不落。
func (b *Bot) putPick(chat int64, p *pick) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, old := range b.picks {
		if b.now().Sub(old.at) > pickTTL {
			delete(b.picks, k)
		}
	}
	for k, old := range b.last {
		if b.now().Sub(old.at) > pickTTL {
			delete(b.last, k)
		}
	}
	if p.at.IsZero() {
		p.at = b.now()
	}
	b.picks[msgKey{chat: chat, msg: p.msgID}] = p
	b.last[chat] = p
}

// pickFor 是「只看不改」：翻页要能反复取同一条清单。
//
// 返回的是副本：调用方会改 page，直接给指针会在锁外改到共享状态。
func (b *Bot) pickFor(chat, msgID int64) (*pick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.picks[msgKey{chat: chat, msg: msgID}]
	if !ok || b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// takePick 取走某条清单。取走即失效：点完片名之后那条消息就改成入库
// 回执了，万一客户端还留着旧按钮，再点也只会得到「过期了」，不会重复入库。
func (b *Bot) takePick(chat, msgID int64) (*pick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	key := msgKey{chat: chat, msg: msgID}
	p, ok := b.picks[key]
	if !ok {
		return nil, false
	}
	delete(b.picks, key)
	if lp, has := b.last[chat]; has && lp.msgID == msgID {
		delete(b.last, chat)
	}
	if b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// dropPick 把某条清单作废（用户点了「都不对」）。
func (b *Bot) dropPick(chat, msgID int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.picks, msgKey{chat: chat, msg: msgID})
	if lp, ok := b.last[chat]; ok && lp.msgID == msgID {
		delete(b.last, chat)
	}
}

// takeLast 取走这个会话最近一次搜索。取走即失效：同一条清单不能
// 被「回复 3、再回复 3」入库两次。
func (b *Bot) takeLast(chat int64) (*pick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.last[chat]
	if !ok {
		return nil, false
	}
	delete(b.last, chat)
	// 清单本身也一起作废：序号已经用掉了，按钮不该还能点。
	delete(b.picks, msgKey{chat: chat, msg: p.msgID})
	if b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	return &cp, true
}

// ==================== TMDB 候选会话状态 ====================

// putTMDBPick 记下一份待选候选，顺手清掉过期的（和 pick 一样 30 分钟）。
// 按 chat 存：重新发个片名就覆盖旧的，一个会话不该同时躺着两份候选。
func (b *Bot) putTMDBPick(chat int64, p *tmdbPick) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, old := range b.tmdbPicks {
		if b.now().Sub(old.at) > pickTTL {
			delete(b.tmdbPicks, k)
		}
	}
	if p.at.IsZero() {
		p.at = b.now()
	}
	b.tmdbPicks[chat] = p
}

// tmdbPickFor 返回只读副本：翻页会改 page，不能把共享指针放出去；
// hits 也拷一份，免得外部改到切片底层数组。
func (b *Bot) tmdbPickFor(chat int64) (*tmdbPick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.tmdbPicks[chat]
	if !ok || b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	cp.hits = append([]TitleHit(nil), p.hits...)
	return &cp, true
}

// setTMDBPage 只改页码，候选本身留着给下一次翻页 / 选定。
func (b *Bot) setTMDBPage(chat int64, page int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if p, ok := b.tmdbPicks[chat]; ok {
		p.page = page
	}
}

// dropTMDBPick 作废一份候选：选中一部或点取消之后调。
func (b *Bot) dropTMDBPick(chat int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.tmdbPicks, chat)
}

// ==================== 发送 ====================

func (b *Bot) reply(ctx context.Context, a *api, chat int64, html string) {
	if _, err := a.send(ctx, chat, html, nil); err != nil {
		b.log.Warn("回复 Telegram 消息失败", "chat", chat, "err", err)
	}
}

// say 有 msgID 就改写那条消息，没有就发新的。
func (b *Bot) say(ctx context.Context, a *api, chat, msgID int64, html string) {
	if msgID > 0 {
		if err := a.edit(ctx, chat, msgID, html, nil); err == nil {
			return
		}
		// 改不了（比如超过 48 小时、或内容没变）就退化成发新消息
	}
	b.reply(ctx, a, chat, html)
}

// atoiOK 解析回调数据里的数字。回调是外部输入（老消息里的按钮可能带着
// 别的格式），解析不出来要能区分「0」和「没解析出来」。
func atoiOK(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, false
		}
		n = n*10 + int(r-'0')
	}
	return n, true
}
