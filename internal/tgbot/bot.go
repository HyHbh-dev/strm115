// Package tgbot 是 Telegram Bot 的接收端：让人在聊天窗口里把片子丢进库。
//
// 它处理三种输入，都走项目里已经验证过的那几条链路，不另开一套：
//
//	115 分享链接（115.com/s/xxx，可带提取码） → Manager.SaveShare 转存
//	磁力 / ed2k / .torrent 直链              → offline.Service.Add 离线下载
//	一个片名                                  → 先 TMDB 认片挑一部，再搜资源挑一条入库
//
// 三点设计取舍：
//
//  1. **不做第二套搜索**。搜索结果直接来自 internal/resource，和网页上的
//     「资源搜索」是同一份数据；否则同一部片子会出现「网页 3 条、TG 1 条」的怪事。
//  2. **动作要回执**。转存和离线都可能被 115 拒绝（分享过期、没有可用节点），
//     失败原因原样回给用户 —— 在手机上没法看服务端日志。
//  3. **白名单先到先得**。Token 一填，任何拿到 Bot 名的人都能给它发消息；
//     所以只有白名单里的会话能用，白名单为空时第一个 /start 的会话即主人，
//     之后别的会话只能看到自己的 chat id。
package tgbot

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"strm115/internal/resource"
)

// TitleHit 是「TMDB 认片」返回的一条候选。
//
// 字段和 internal/tmdb.MetaHit 一一对应，但单独定义在 tgbot 包里：接收端
// 不该依赖 tmdb 包（那边还带 HTTP 客户端、图片地址等一整套东西），由注入方
// （httpapi）负责把 MetaHit 映射成这个轻结构。
type TitleHit struct {
	ID       int     // TMDB ID
	Title    string  // 中文名（zh-CN 本地化标题）
	Original string  // original_title / original_name：选中后当 en 传给后端搜资源
	Year     string  // 四位年份，拿不到就是空串
	Kind     string  // "movie" / "tv"
	Rating   float64 // 评分（10 分制）
}

// SubSeason 是「从哪一集开始订阅」选季时的一季。
//
// 只带季号和集数：TG 的按钮塞不下每集播出日，而且起播线本来就是个
// 「S/E 号」而不是日期。逐集播出日由后端建订阅时自己从 TMDB 拉。
type SubSeason struct {
	Season   int
	Episodes int
	Name     string
}

// SubscribeRequest 是一次订阅。Watermark 语义和网页一致：
// 「这一集及之前不下载」，0/0 = 从头追。
type SubscribeRequest struct {
	TMDbID           int
	Kind             string // movie / tv
	Title            string
	Original         string
	Year             string
	WatermarkSeason  int
	WatermarkEpisode int
}

// SubscribeResult 是订阅结果。Already 表示这部片已经在订阅里，
// 不算失败 —— 机器人回一句「已经在追了」比抛错误友好。
type SubscribeResult struct {
	ID            int64
	Title         string
	CalendarTotal int // 剧集：物化出的专有日历集数
	Already       bool
}

// Ingest 是一次「入库」的结果，用来拼回复里的入库信息。
type Ingest struct {
	Title   string   // 片名 / 任务名
	Files   int      // 转存的文件数（离线为 0）
	Dir     string   // 落到哪个目录（人类可读）
	Account string   // 哪个 115 账号
	Pending bool     // true = 提交了离线下载，还没下完
	Note    string   // 额外一句（比如「已触发增量同步」）
	Hashes  []string // 离线任务 id
}

// Deps 是这个 bot 需要的全部外部能力。
//
// 全是函数而不是接口：注入方（httpapi）本来就有这些方法，
// 包一层接口只会多一个文件；测试时给几个闭包就能跑，不需要起服务器。
type Deps struct {
	// Token 每次轮询前读一遍：用户可能在设置里改了 Token 或清空了它。
	Token func() string
	// Proxy 出站代理，空 = 直连。
	Proxy func() string
	// Allow 返回白名单会话；空表示还没绑定。
	Allow func(ctx context.Context) []int64
	// Bind 把第一个说话的会话记成主人。
	Bind func(ctx context.Context, chatID int64) error

	// Search 搜片名。en 是「TMDB 认片」锁定的英文原名（original_title），
	// 由后端当作种子站换名用；没走认片（或认片降级）时为空。limit 是希望返回的条数。
	Search func(ctx context.Context, query, en string, limit int) (*resource.Result, error)
	// SearchTMDB 先按片名问 TMDB 要一批候选，让人点选锁定准确的那部，再拿它的
	// 中文名 + 英文原名去搜资源。返回错误或空结果时，机器人必须自动降级到
	// 「直接按片名搜资源」，绝不能让 bot 哑掉。为 nil 时整段认片流程跳过。
	SearchTMDB func(ctx context.Context, query string, limit int) ([]TitleHit, error)
	// TMDBName 只查英文名（language=en-US 的 title），不搜资源、不上键盘。
	// 用途：/name 命令 —— 用户要的是一个能直接复制去改名、拼搜索词的英文标题。
	// 为 nil 时命令回复「没接上 TMDB」。
	TMDBName func(ctx context.Context, query string, limit int) ([]TitleHit, error)
	// Transfer 转存一条 115 分享。
	Transfer func(ctx context.Context, shareCode, password string) (*Ingest, error)
	// TransferQuark 转存一条夸克分享。夸克走的是另一条链路：
	// 网页 Cookie 转存进夸克网盘，播放再由夸克TV 的 302 直链负责。
	TransferQuark func(ctx context.Context, url, passcode string) (*Ingest, error)
	// Offline 提交一条离线链接。
	Offline func(ctx context.Context, uri, label string) (*Ingest, error)

	// Subscribe 建一条订阅（剧集会顺带物化专有追更日历）。为 nil 时
	// 所有回复里的订阅按钮都藏起来，绝不给出一个点了没反应的按钮。
	Subscribe func(ctx context.Context, req SubscribeRequest) (*SubscribeResult, error)
	// SubscribeSeasons 给剧集的「从哪一集开始订阅」列季集合。
	// 为 nil 或失败时机器人降级成「直接从头追」，不让订阅卡住。
	SubscribeSeasons func(ctx context.Context, tmdbID int) ([]SubSeason, error)

	// Offset / SaveOffset 持久化 getUpdates 游标。
	//
	// 不存盘的话，服务重启会把「上次已经转存过的分享」当新消息再处理一遍。
	Offset     func(ctx context.Context) int64
	SaveOffset func(ctx context.Context, n int64)

	// State 把「机器人现在什么状态」写给界面看（Token 错了、被 webhook 占了…）。
	State func(ctx context.Context, msg string)

	Log *slog.Logger
	Now func() time.Time
}

// Bot 是接收端本体。零值不可用，必须用 New。
type Bot struct {
	deps Deps
	log  *slog.Logger
	now  func() time.Time

	mu     sync.Mutex
	api    *api
	offset int64
	// greeted 记住「已经连上并打过日志」，换 Token 重建时也不会重复打。
	greeted bool
	// username 是 getMe 拿到的 bot 用户名，用来拼 deep link：
	// https://t.me/<username>?start=tN 。只有它非空时，正文里的片名 / 种子名
	// 才能做成可点链接；拿不到就降级回按钮选定，功能不能丢。
	// 受 mu 保护：poll 里写、渲染时读，两者可能落在不同 goroutine。
	username string
	// picks 记住每条搜索结果消息背后的完整列表，等用户点按钮或回序号。
	// 只在内存里：重启后旧清单点下去会得到「重新搜一次」，
	// 这比把几十条搜索结果写进数据库更合适。
	picks map[msgKey]*pick
	// last 是每个会话最近一次搜索。不带按钮的数字回复（「3」）只能靠它
	// 知道 3 指的是哪份清单。
	last map[int64]*pick
	// tmdbPicks 是「待选 TMDB 候选」，按会话存，30 分钟有效。
	// 和资源清单分开存：这是两个阶段，选中候选后要清掉它、再进入资源清单。
	tmdbPicks map[int64]*tmdbPick
	// namePicks 是 /name 回的那几部片，给「查完英文名顺手订阅」用。
	// 和 tmdbPicks 分开：/name 不该覆盖用户正在选的认片候选。
	namePicks map[int64]*tmdbPick
	// subPicks 是「正在选起始集」的会话，按 chat 存，30 分钟有效。
	subPicks map[int64]*subPick
}

// msgKey 是「哪条消息」：同一个会话里可能同时躺着两次搜索，
// 只按 chat 存的话，翻旧消息的「下一页」会翻到新搜索结果上去。
type msgKey struct {
	chat int64
	msg  int64
}

// pick 是一次搜索的完整结果，等用户点按钮（或回序号）。
type pick struct {
	query string
	res   *resource.Result
	items []resource.Item // 已按「种子站 / TG 交错」排好序的完整结果
	msgID int64           // 清单那条消息，回执要能对上
	page  int             // 当前屏（从 0 起），翻页只改它
	at    time.Time
	// hit 非空表示这次搜索是「认片之后」发起的：清单底部才有
	// 「订阅这部」按钮。降级直接按片名搜时没有 TMDB id，不能硬建订阅。
	hit *TitleHit
}

// tmdbPick 是一次「TMDB 认片」的完整候选，等用户点片名（deep link 或降级按钮）。
//
// 按 chat 存而不是按消息 id：一个会话同一时刻只该有一份待选候选，重新发个
// 片名就覆盖旧的；deep link /start tN 进来时手里也只有 chat，没有消息 id。
type tmdbPick struct {
	query string
	hits  []TitleHit
	msgID int64 // 候选清单那条消息，翻页改写要用
	page  int   // 当前屏（从 0 起）
	at    time.Time
}

// subPick 是「点过订阅、正在挑起始集」的会话。
//
// 按 chat 存：一个会话同一时刻只该有一份待定订阅，重新点别的片就覆盖。
// 电影不走它 —— 没有集可选，点一下直接建好。
type subPick struct {
	hit     TitleHit
	seasons []SubSeason
	season  int   // 当前正在挑集号的季
	msgID   int64 // 「从哪一集开始」那条消息，选季/选集原地改写它
	at      time.Time
}

// New 建一个 Bot。
func New(d Deps) *Bot {
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	return &Bot{
		deps: d, log: log, now: now,
		picks: map[msgKey]*pick{}, last: map[int64]*pick{},
		tmdbPicks: map[int64]*tmdbPick{},
		namePicks: map[int64]*tmdbPick{},
		subPicks:  map[int64]*subPick{},
	}
}

// botUsername 读当前 bot 用户名。加锁：poll 写它的同时渲染可能正在读。
func (b *Bot) botUsername() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.username
}

// setBotUsername 存 getMe 的结果。
//
// 特意把它做成 Bot 上的可注入字段：单测不走 poll/getMe（那要起长轮询），
// 直接调这个方法塞一个假用户名，就能验证 deep link 渲染，不必连 Telegram。
func (b *Bot) setBotUsername(name string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.username = strings.TrimSpace(name)
}

// pageSize 一屏列几条。
//
// 8 条：手机上一屏正好放得下「8 个按钮 + 取消」，再多就得滑，
// 而滑到第 9 条的人通常已经想改关键词了 —— 真要往下看就点「下一页」。
const pageSize = 8

// searchLimit 是「每个源最多取几条」，和展示条数故意分开。
//
// 以前两个值都用 maxResults：一个 TG 频道就能返回 8 条，把种子站
// 的结果全挤到列表外，用户以为种子站没搜。多取一些再按来源均衡挑，
// 才既压得住消息长度，又不会让单一来源霸屏。
const searchLimit = 20

// Run 是常驻循环，直到 ctx 取消。
//
// 外层只做一件事：盯着 Token。Token 空着就等着（用户还没配），
// 变了就重建客户端 —— 这样在设置页改完 Token 不用重启容器。
func (b *Bot) Run(ctx context.Context) {
	b.log.Info("Telegram 机器人已启动（等待 Token）")
	backoff := 3 * time.Second
	idle := 15 * time.Second

	for ctx.Err() == nil {
		token := strings.TrimSpace(b.deps.Token())
		if token == "" {
			if b.state(ctx, "还没填 Bot Token") && !sleep(ctx, idle) {
				return
			}
			continue
		}

		a := b.client(token)
		err := b.poll(ctx, a)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			backoff = 3 * time.Second
			continue
		}

		// 区分「要人改配置」和「网络抖了一下」：前者不该无限快速重试。
		var ae *apiError
		switch {
		case errors.As(err, &ae) && ae.Code == 401:
			b.state(ctx, "Token 不对或已被吊销，机器人停下来了")
			b.log.Error("Telegram Token 无效，暂停轮询", "err", err)
			if !sleep(ctx, 60*time.Second) {
				return
			}
		case errors.As(err, &ae) && ae.Code == 409:
			b.state(ctx, "Bot 设置了 Webhook，getUpdates 拿不到消息：先去删掉 webhook")
			b.log.Error("Telegram 409：Bot 被 webhook 占着，收不到消息", "err", err)
			if !sleep(ctx, 30*time.Second) {
				return
			}
		default:
			b.state(ctx, "轮询出错："+err.Error())
			b.log.Warn("Telegram 轮询失败，稍后重试", "err", err, "backoff", backoff)
			if !sleep(ctx, backoff) {
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
		}
	}
}

// client 按 Token 缓存 api：Token 没变就不重建连接池。
func (b *Bot) client(token string) *api {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.api != nil && b.api.token == token {
		return b.api
	}
	proxy := ""
	if b.deps.Proxy != nil {
		proxy = b.deps.Proxy()
	}
	b.api = newAPI(token, proxy)
	b.offset = b.loadOffset(context.Background())
	return b.api
}

func (b *Bot) loadOffset(ctx context.Context) int64 {
	if b.deps.Offset == nil {
		return 0
	}
	return b.deps.Offset(ctx)
}

// poll 长轮询并逐条处理，直到出错或 ctx 结束。
func (b *Bot) poll(ctx context.Context, a *api) error {
	// 先确认 Token 是谁的：getMe 一次，日志里就有 Bot 名字，
	// 用户配错 Token 时比「消息发不出去」好排查得多。
	if me, err := a.getMe(ctx); err != nil {
		return err
	} else {
		// 每次连上都刷新：用户可能换了 Token，bot 用户名跟着变，
		// deep link 必须用当前这个用户名，否则链接点开是别的 bot。
		b.setBotUsername(me.Username)
		if !b.greeted {
			b.greeted = true
			b.log.Info("Telegram 机器人已连接", "bot", "@"+me.Username, "id", me.ID)
			// 把「连上了」也写进状态：否则界面上会一直挂着上一次的错误
			b.state(ctx, "运行中（@"+me.Username+"）")
			_ = a.setMyCommands(ctx, commands)
		}
	}

	for ctx.Err() == nil {
		ups, err := a.getUpdates(ctx, b.currentOffset(), pollTimeout)
		if err != nil {
			return err
		}
		for _, up := range ups {
			b.handleOne(ctx, a, up)
			b.advance(ctx, up.UpdateID+1)
		}
	}
	return nil
}

func (b *Bot) currentOffset() int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.offset
}

func (b *Bot) advance(ctx context.Context, next int64) {
	b.mu.Lock()
	if next > b.offset {
		b.offset = next
	}
	cur := b.offset
	b.mu.Unlock()

	if b.deps.SaveOffset != nil {
		b.deps.SaveOffset(ctx, cur)
	}
}

// handleOne 处理一条更新，panic 不会带走整个循环。
func (b *Bot) handleOne(ctx context.Context, a *api, up Update) {
	defer func() {
		if r := recover(); r != nil {
			b.log.Error("处理 Telegram 更新时 panic", "update", up.UpdateID, "panic", r)
		}
	}()

	// 单条处理给个上限：一次搜索 + 一次转存可能几十秒，
	// 但不该拖着整个循环无限期卡住。
	hctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	switch {
	case up.CallbackQuery != nil:
		b.onCallback(hctx, a, up.CallbackQuery)
	case up.Message != nil || up.EditedMessage != nil:
		msg := up.Message
		if msg == nil {
			msg = up.EditedMessage
		}
		if msg.From != nil && msg.From.IsBot {
			return // 别的 bot 的消息一律不理，避免两个 bot 互相回
		}
		b.onMessage(hctx, a, msg)
	}
}

// state 记录一次状态变化，返回是否真的变了（用来避免刷屏日志）。
func (b *Bot) state(ctx context.Context, msg string) bool {
	if b.deps.State != nil {
		b.deps.State(ctx, msg)
	}
	return msg != ""
}

func sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
