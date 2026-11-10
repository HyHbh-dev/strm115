package tgbot

// Telegram 里的订阅：候选 / 资源清单 / 英文名回复都能直接订一部片。
//
// 和网页共用同一个后端创建入口（见 httpapi.botSubscribe），所以 TG 订出来的
// 剧集一样有专有追更日历。这里只管聊天窗口里的交互：
//
//	⭐ 订阅 → 剧集选季 → 选集号 → 建订阅 → 回执（原地改写那条消息）
//	         → 电影直接建订阅
//
// 回调数据只带序号 / 季集号，真正的片子在内存状态里（按 chat 存，30 分钟），
// 和搜索清单的做法一致。

import (
	"context"
	"strings"
)

// onSubscribeCallback 认领所有 sb* 回调。返回 false 表示这不是订阅按钮，
// 交回 handler.go 的搜索回调 switch 处理。
func (b *Bot) onSubscribeCallback(
	ctx context.Context, a *api, q *CallbackQuery, chat, msgID int64,
) bool {
	d := q.Data
	if !strings.HasPrefix(d, "sb") || b.deps.Subscribe == nil {
		return false
	}
	answer := func(text string) { _ = a.answer(ctx, q.ID, text) }
	expired := func() { answer("这份订阅已过期，重新点一次 ⭐ 订阅") }

	switch {
	case strings.HasPrefix(d, "sb:"):
		// TMDB 认片候选：n 是正文里的全局序号，和 t:n 共用一份清单。
		n, ok := atoiOK(strings.TrimPrefix(d, "sb:"))
		if !ok || n < 1 {
			answer("这条候选不对")
			return true
		}
		p, live := b.tmdbPickFor(chat)
		if !live || n > len(p.hits) {
			expired()
			return true
		}
		answer("")
		b.startSubscribe(ctx, a, chat, p.hits[n-1])
		return true

	case strings.HasPrefix(d, "sbn:"):
		// /name 的英文名清单：序号和正文的 1./2. 对齐。
		n, ok := atoiOK(strings.TrimPrefix(d, "sbn:"))
		if !ok || n < 1 {
			answer("这条候选不对")
			return true
		}
		p, live := b.namePickFor(chat)
		if !live || n > len(p.hits) {
			expired()
			return true
		}
		answer("")
		b.startSubscribe(ctx, a, chat, p.hits[n-1])
		return true

	case d == "sbp":
		// 资源清单底部的「订阅这部」：只有认片后发起的搜索才有 hit。
		p, live := b.pickFor(chat, msgID)
		if !live || p.hit == nil {
			expired()
			return true
		}
		answer("")
		b.startSubscribe(ctx, a, chat, *p.hit)
		return true

	case d == "sbg":
		// 剧集「从头追」：不需要起播线。
		p, live := b.subPickFor(chat)
		if !live {
			expired()
			return true
		}
		answer("正在订阅…")
		b.finishSubscribe(ctx, a, chat, p.hit, p.msgID, 0, 0)
		return true

	case d == "sbback":
		p, live := b.subPickFor(chat)
		if !live {
			expired()
			return true
		}
		p.season = 0
		b.putSubPick(chat, p)
		if err := a.edit(ctx, chat, p.msgID,
			renderSubSeasons(p.hit, p.seasons), subSeasonKeyboard(p.seasons)); err != nil {
			b.log.Debug("返回选季改写失败", "err", err, "chat", chat)
		}
		answer("")
		return true

	case strings.HasPrefix(d, "sbs:"):
		season, ok := atoiOK(strings.TrimPrefix(d, "sbs:"))
		p, live := b.subPickFor(chat)
		if !live {
			expired()
			return true
		}
		count := seasonEpisodes(p.seasons, season)
		if !ok || count <= 0 {
			answer("TMDB 没给这一季的集数")
			return true
		}
		p.season = season
		b.putSubPick(chat, p)
		if err := a.edit(ctx, chat, p.msgID,
			renderSubEpisodes(p.hit, season, count), subEpisodeKeyboard(season, count)); err != nil {
			b.log.Debug("选集改写失败", "err", err, "chat", chat)
		}
		answer("")
		return true

	case strings.HasPrefix(d, "sbe:"):
		parts := strings.Split(strings.TrimPrefix(d, "sbe:"), ":")
		p, live := b.subPickFor(chat)
		if !live {
			expired()
			return true
		}
		if len(parts) != 2 {
			answer("这条选集不对")
			return true
		}
		season, ok1 := atoiOK(parts[0])
		episode, ok2 := atoiOK(parts[1])
		if !ok1 || !ok2 || episode < 1 || seasonEpisodes(p.seasons, season) < episode {
			answer("这一集不在 TMDB 的集数里")
			return true
		}
		answer("正在订阅…")
		b.finishSubscribe(ctx, a, chat, p.hit, p.msgID, season, episode)
		return true

	case d == "sbx":
		b.dropSubPick(chat)
		if err := a.edit(ctx, chat, msgID,
			"❌ <b>不订阅了</b>\n想追更时重新点 ⭐ 订阅就行。", nil); err != nil {
			b.log.Debug("取消订阅改写失败", "err", err, "chat", chat)
		}
		answer("已取消")
		return true
	}
	return false
}

// startSubscribe 点完「⭐ 订阅」的第一步。
//
// 电影没有集可选，直接建；剧集先问后端要季集合再发一屏选起始集。
// 季集合拿不到（TMDB 挂了、没配 Key、没注入该能力）时降级成「从头追」——
// 订阅这件事不能被一个可选的选集步骤卡住。
func (b *Bot) startSubscribe(ctx context.Context, a *api, chat int64, hit TitleHit) {
	if hit.ID <= 0 {
		b.reply(ctx, a, chat, "这部片没带上 TMDB ID，订阅不了；换个候选或到网页里认片后再订。")
		return
	}
	if !isTVHit(hit) {
		b.finishSubscribe(ctx, a, chat, hit, 0, 0, 0)
		return
	}

	var seasons []SubSeason
	if b.deps.SubscribeSeasons != nil {
		got, err := b.deps.SubscribeSeasons(ctx, hit.ID)
		if err != nil {
			b.log.Warn("读取季集数失败，订阅降级为从头追",
				"err", err, "chat", chat, "tmdb_id", hit.ID)
		} else {
			seasons = got
		}
	}
	if len(seasons) == 0 {
		b.finishSubscribe(ctx, a, chat, hit, 0, 0, 0)
		return
	}

	msgID, err := a.send(ctx, chat,
		renderSubSeasons(hit, seasons), subSeasonKeyboard(seasons))
	if err != nil {
		b.log.Warn("发送选季界面失败，订阅降级为从头追", "err", err, "chat", chat)
		b.finishSubscribe(ctx, a, chat, hit, 0, 0, 0)
		return
	}
	b.putSubPick(chat, &subPick{
		hit: hit, seasons: seasons, msgID: msgID, at: b.now(),
	})
}

// finishSubscribe 真正建订阅，并把结果写回「选起始集」那条消息。
// msgID 为 0（电影 / 降级路径）时另发一条回执。
func (b *Bot) finishSubscribe(
	ctx context.Context, a *api, chat int64, hit TitleHit, msgID int64, ws, we int,
) {
	req := SubscribeRequest{
		TMDbID: hit.ID, Kind: hit.Kind,
		Title: subTitle(hit), Original: hit.Original, Year: hit.Year,
		WatermarkSeason: ws, WatermarkEpisode: we,
	}
	res, err := b.deps.Subscribe(ctx, req)
	errText := ""
	if err != nil {
		errText = err.Error()
		b.log.Warn("TG 订阅失败", "err", err, "chat", chat, "tmdb_id", hit.ID)
	}
	html := renderSubResult(hit, res, ws, we, errText)
	if msgID > 0 {
		if e := a.edit(ctx, chat, msgID, html, nil); e == nil {
			b.dropSubPick(chat)
			return
		}
		b.log.Debug("订阅回执改写失败，改发新消息", "chat", chat)
	}
	b.reply(ctx, a, chat, html)
	b.dropSubPick(chat)
}

func seasonEpisodes(seasons []SubSeason, season int) int {
	for _, s := range seasons {
		if s.Season == season {
			return s.Episodes
		}
	}
	return 0
}

// ==================== 会话状态 ====================

func (b *Bot) putNamePick(chat int64, p *tmdbPick) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, old := range b.namePicks {
		if b.now().Sub(old.at) > pickTTL {
			delete(b.namePicks, k)
		}
	}
	if p.at.IsZero() {
		p.at = b.now()
	}
	b.namePicks[chat] = p
}

func (b *Bot) namePickFor(chat int64) (*tmdbPick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.namePicks[chat]
	if !ok || b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	cp.hits = append([]TitleHit(nil), p.hits...)
	return &cp, true
}

func (b *Bot) putSubPick(chat int64, p *subPick) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for k, old := range b.subPicks {
		if b.now().Sub(old.at) > pickTTL {
			delete(b.subPicks, k)
		}
	}
	if p.at.IsZero() {
		p.at = b.now()
	}
	b.subPicks[chat] = p
}

func (b *Bot) subPickFor(chat int64) (*subPick, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	p, ok := b.subPicks[chat]
	if !ok || b.now().Sub(p.at) > pickTTL {
		return nil, false
	}
	cp := *p
	cp.seasons = append([]SubSeason(nil), p.seasons...)
	return &cp, true
}

func (b *Bot) dropSubPick(chat int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.subPicks, chat)
}
