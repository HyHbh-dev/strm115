package subscribe

import (
	"context"
	"fmt"
	"time"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// 专有日历驱动的巡检。
//
// 旧的 checkTV 是「问 TMDB 最新出到第几集 → 比水位线 → 一轮补最多 3 集 →
// 没资源 10 分钟后再来」。它不知道单集的播出日，也不知道哪集已经试过。
// 现在每集在 subscription_episodes 里有自己的 air_date / next_try_at：
//   - 到了播出日才搜；
//   - 搜不到就把这一集排到 24 小时后（每天一次），不再 10 分钟空转；
//   - 搜到并提交后这一集永远不再搜。
//
// 同一个订阅一轮只发一次搜索请求，逐集用 Pick 按季集号精确过滤，避免
// 首订一部 24 集的剧时打 24 次资源搜索。
const (
	calendarRetry           = 24 * time.Hour // 没搜到：每天重试一次
	calendarRefreshInterval = 24 * time.Hour // 有日历的订阅：多久刷一次 TMDB 集数集合
	calendarBatch           = 30             // 一轮最多取多少到期集
	calendarSearchTimeout   = 3 * time.Minute
)

// tickCalendar 扫一轮「今天该搜」的集，按订阅分组串行处理。
func (s *Service) tickCalendar(ctx context.Context) {
	if s.dep.Search == nil || s.dep.Submit == nil {
		return
	}
	items, err := s.st.DueCalendarEpisodes(ctx, time.Now(), calendarBatch)
	if err != nil {
		s.log.Warn("订阅日历：读取到期集失败", "err", err)
		return
	}
	if len(items) == 0 {
		return
	}

	groups := map[int64][]store.DueSubEpisode{}
	order := []int64{}
	for _, it := range items {
		if _, ok := groups[it.Sub.ID]; !ok {
			order = append(order, it.Sub.ID)
		}
		groups[it.Sub.ID] = append(groups[it.Sub.ID], it)
	}
	s.log.Info("订阅日历：这一轮有到期的集", "count", len(items), "subs", len(order))
	for _, id := range order {
		if ctx.Err() != nil {
			return
		}
		s.checkCalendarGroup(ctx, groups[id])
	}
}

type calendarHit struct {
	ep  store.SubEpisode
	uri string
	tve tmdb.TVEpisode
}

// checkCalendarGroup 处理一部剧本轮到期的所有集：一次搜索、逐集挑选、一次提交。
func (s *Service) checkCalendarGroup(parent context.Context, group []store.DueSubEpisode) {
	if len(group) == 0 {
		return
	}
	sub := group[0].Sub
	// 和 CheckOne 共用同一把锁：手动「立即检查」撞上轮询时不会同时提交。
	if !s.acquire(sub.ID) {
		return
	}
	defer s.release(sub.ID)

	ctx, cancel := context.WithTimeout(parent, calendarSearchTimeout)
	defer cancel()

	res, err := s.search(ctx, sub)
	if err != nil {
		msg := "搜索失败：" + err.Error()
		// 搜索本身出错（不是没搜到）不该等一整天，1 小时后再试。
		for _, it := range group {
			s.missEpisode(ctx, it.Ep, time.Now().Add(time.Hour), msg)
		}
		s.log.Warn("订阅日历：搜索失败", "id", sub.ID, "title", sub.Title, "err", err)
		return
	}

	opt := PickOptions{Quality: sub.Quality, MinSize: sub.MinSize, MaxSize: sub.MaxSize}
	hits := make([]calendarHit, 0, len(group))
	for _, it := range group {
		if len(hits) >= maxCatchUp {
			// 一轮最多补 3 集，剩下的留到下一轮（下一分钟就是下一轮）。
			// 这里不改 next_try_at，它们仍然到期，不会被漏掉。
			break
		}
		o := opt
		o.Season, o.Episode = it.Ep.Season, it.Ep.Episode
		hit := Pick(res.Items, o)
		if hit == nil {
			s.missEpisode(ctx, it.Ep, time.Now().Add(calendarRetry), Describe(len(res.Items), o))
			continue
		}
		hits = append(hits, calendarHit{
			ep: it.Ep, uri: URI(*hit),
			tve: tmdb.TVEpisode{Season: it.Ep.Season, Episode: it.Ep.Episode},
		})
	}
	if len(hits) == 0 {
		return
	}

	uris := make([]string, 0, len(hits))
	tves := make([]tmdb.TVEpisode, 0, len(hits))
	for _, h := range hits {
		uris = append(uris, h.uri)
		tves = append(tves, h.tve)
	}
	acctID, cid := s.target(ctx, sub)
	label := sub.Title + " " + episodeSpan(tves)
	if _, err := s.dep.Submit(ctx, acctID, uris, cid, label); err != nil {
		msg := "提交离线失败：" + err.Error()
		for _, h := range hits {
			s.missEpisode(ctx, h.ep, time.Now().Add(calendarRetry), msg)
		}
		s.log.Warn("订阅日历：提交离线失败", "id", sub.ID, "err", err)
		return
	}

	last := tves[len(tves)-1]
	if err := s.st.SetSubscriptionWatermark(ctx, sub.ID, last.Season, last.Episode); err != nil {
		// 提交已经成功，水位线没存上会让下一轮重复下载，必须报出来。
		s.log.Error("订阅日历：已提交但水位线没存上", "id", sub.ID, "err", err)
	}
	for _, h := range hits {
		if err := s.st.MarkSubEpisodeDone(ctx, h.ep.ID, "已提交 "+label); err != nil {
			s.log.Warn("订阅日历：标记已下载失败", "ep_id", h.ep.ID, "err", err)
		}
	}
	s.mark(ctx, sub, time.Now().Add(calendarRefreshInterval), store.SubWatching,
		"已提交 "+episodeSpan(tves), 0)
	s.log.Info("订阅日历：已按播出日提交", "id", sub.ID, "title", sub.Title, "episodes", episodeSpan(tves))
}

func (s *Service) missEpisode(ctx context.Context, ep store.SubEpisode, next time.Time, msg string) {
	if err := s.st.MarkSubEpisodeMissed(ctx, ep.ID, next, msg); err != nil {
		s.log.Warn("订阅日历：写回重试排期失败", "ep_id", ep.ID, "err", err)
	}
}

// refreshCalendar 刷新一部剧的 TMDB 集数集合（新季 / 新增集 / 补上缺失的 air_date）。
//
// 有专有日历的剧集订阅每 24 小时走一次这里，不再跑 checkTV：资源搜索已经
// 由播出日驱动，两条路径同时跑会把同一集提交两次。
func (s *Service) refreshCalendar(ctx context.Context, sub store.Subscription) (string, error) {
	n, err := s.dep.CalendarRefresh(ctx, sub)
	if err != nil {
		return "", err
	}
	state := sub.State
	if state == "" {
		state = store.SubWatching
	}
	// done 的订阅保持 done：DueSubscriptions 不再捞它，别为了刷日历把
	// 已完结的剧重新唤醒成 watching。
	msg := fmt.Sprintf("专有日历已刷新（%d 集），按播出日搜索", n)
	s.mark(ctx, sub, time.Now().Add(calendarRefreshInterval), state, msg, 0)
	return msg, nil
}
