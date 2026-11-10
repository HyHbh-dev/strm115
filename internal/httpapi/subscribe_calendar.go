package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// 订阅专有播放日历。
//
// 和老的那套「全局按周的在播剧单」完全不同：这里的数据来自用户已订阅的剧，
// 每一集一行、带 TMDB 的单集 air_date 与「搜到没」的状态，轮询器直接按它
// 决定今天该搜哪一集。老周历是展示型的、按周共享缓存，装不下这种进度。

const maxCalendarDays = 62

// 一部 20 季的老剧逐季问 TMDB 要 20 个请求，用户点「订阅」得等十几秒。
// 只详查最近 12 季的单集播出日，更早的季按 season.episode_count 生成占位集
// （没有 air_date，按「已播出」处理，从头追时照样能补）。
const maxDetailedSeasons = 12

// materializeCalendar 把一部剧的 TMDB 集数集合写进订阅的专有日历。
//
// show 可以由调用方传进来（建订阅时刚查过一次，别重复打 TMDB）；刷新时传
// nil，函数自己查。分季接口单季失败不影响其它季：拿不到分集清单的季按
// season.episode_count 生成占位集（没有 air_date），这样老剧 / 数据不全的
// 剧也照样能被订阅抓到，不会因为一个 404 整部剧漏掉。
func (s *Server) materializeCalendar(ctx context.Context, sub store.Subscription,
	show *tmdb.TVShow, client *tmdb.Client) (int, error) {

	if client == nil {
		return 0, errors.New("没有可用的 TMDB Key")
	}
	if show == nil {
		var err error
		show, err = client.TV(ctx, sub.TMDbID)
		if err != nil {
			return 0, err
		}
	}

	seasonNums := make([]int, 0, len(show.Seasons))
	for _, sn := range show.Seasons {
		if sn.Season > 0 { // 特别篇（season 0）不追
			seasonNums = append(seasonNums, sn.Season)
		}
	}
	if len(seasonNums) > maxDetailedSeasons {
		seasonNums = seasonNums[len(seasonNums)-maxDetailedSeasons:]
	}
	detailed, detailErr := client.SeasonEpisodesMeta(ctx, sub.TMDbID, seasonNums)
	if detailErr != nil {
		s.log.Warn("订阅日历：有季没拿到分集清单，用季集数兜底",
			"id", sub.ID, "title", sub.Title, "err", detailErr)
	}

	bySeason := map[int]map[int]tmdb.SeasonEpisode{}
	for _, e := range detailed {
		if bySeason[e.Season] == nil {
			bySeason[e.Season] = map[int]tmdb.SeasonEpisode{}
		}
		bySeason[e.Season][e.Episode] = e
	}

	today := time.Now().Format("2006-01-02")
	out := make([]store.SubEpisode, 0, show.TotalEpisodes)
	for _, sn := range show.Seasons {
		if sn.Season <= 0 {
			continue
		}
		eps := bySeason[sn.Season]
		if len(eps) == 0 {
			for i := 1; i <= sn.EpisodeCount; i++ {
				out = append(out, store.SubEpisode{
					Season: sn.Season, Episode: i, Status: store.SubEpWaiting,
				})
			}
			continue
		}
		for i := 1; i <= len(eps); i++ {
			e, ok := eps[i]
			if !ok {
				// 集号不连续（TMDB 偶尔缺一集），补一个占位，别让这一集永远消失。
				e = tmdb.SeasonEpisode{Season: sn.Season, Episode: i}
			}
			status := store.SubEpWaiting
			// YYYY-MM-DD 可以直接按字典序比；空串 = 没定档，按「已到期」处理。
			if e.AirDate > today {
				status = store.SubEpPending
			}
			out = append(out, store.SubEpisode{
				Season: e.Season, Episode: e.Episode, AirDate: e.AirDate,
				Name: e.Name, Status: status,
			})
		}
	}
	if len(out) == 0 {
		return 0, nil
	}
	if err := s.st.UpsertSubscriptionEpisodes(ctx, sub.ID, out); err != nil {
		return 0, err
	}
	// 「从 S02E05 开始订阅」：起始集本身要追，只把更早的集标成已跳过，
	// 这样周历上看得见全貌，也不会被轮询器翻出来。
	if sub.WatermarkSeason > 0 || sub.WatermarkEpisode > 0 {
		if err := s.st.SkipEpisodesBefore(ctx, sub.ID,
			sub.WatermarkSeason, sub.WatermarkEpisode); err != nil {
			s.log.Warn("订阅日历：跳过起播线之前的集失败", "id", sub.ID, "err", err)
		}
	}
	return len(out), nil
}

// handleSubEpisodes 给「从哪一集开始订阅」弹窗用：返回季集合 + 指定季的分集。
//
// 不带 season 时只返回季清单（每季多少集），选季之后再带 season 拉该季的
// 逐集清单（含单集播出日）。按季懒加载，避免订阅一部 20 季老剧时一次打
// 20 个 TMDB 请求。
func (s *Server) handleSubEpisodes(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, _ := strconv.Atoi(strings.TrimSpace(q.Get("tmdb_id")))
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 tmdb_id"))
		return
	}
	client := s.tmdbClient(r.Context())
	if client == nil {
		writeErr(w, http.StatusBadRequest, errors.New("先到设置里填 TMDB API Key"))
		return
	}
	show, err := client.TV(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	out := map[string]any{
		"show":    show,
		"seasons": show.Seasons,
	}
	if season, _ := strconv.Atoi(strings.TrimSpace(q.Get("season"))); season > 0 {
		eps, err := client.SeasonEpisodes(r.Context(), id, season)
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		out["episodes"] = eps
		out["season"] = season
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSubCalendar 返回用户专有的追更日历：只含已订阅剧集的单集播出日。
//
// start 缺省是本周一，days 默认 28 天（一次给四周，前端翻周不必每次回源）。
// air_date 为空的「未定档」集单独放 undated，不然周历里会冒出一行没日期的卡片。
func (s *Server) handleSubCalendar(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	start := strings.TrimSpace(q.Get("start"))
	if start == "" {
		now := time.Now()
		// 周一为一周起点，和原来的周历保持一致。
		offset := (int(now.Weekday()) + 6) % 7
		start = now.AddDate(0, 0, -offset).Format("2006-01-02")
	}
	from, err := time.Parse("2006-01-02", start)
	if err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("start 要写成 YYYY-MM-DD"))
		return
	}
	days, _ := strconv.Atoi(strings.TrimSpace(q.Get("days")))
	if days <= 0 {
		days = 28
	}
	if days > maxCalendarDays {
		days = maxCalendarDays
	}
	to := from.AddDate(0, 0, days-1).Format("2006-01-02")

	ctx := r.Context()
	items, err := s.st.ListCalendarRange(ctx, from.Format("2006-01-02"), to)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	undated, err := s.st.ListUndatedEpisodes(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"start": from.Format("2006-01-02"), "end": to, "days": days,
		"items": items, "undated": undated, "total": len(items),
	})
}

// handleRefreshSubCalendar 重新从 TMDB 拉一次集数集合（新季/新增集）。
// 巡检每 24 小时会自动调一次，这里给用户一个手动入口。
func (s *Server) handleRefreshSubCalendar(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("订阅 id 不合法"))
		return
	}
	ctx := r.Context()
	sub, err := s.st.GetSubscription(ctx, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if sub == nil {
		writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		return
	}
	if sub.MediaType == "movie" {
		writeErr(w, http.StatusBadRequest, errors.New("电影没有集数日历"))
		return
	}
	n, err := s.materializeCalendar(ctx, *sub, nil, s.tmdbClient(ctx))
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "total": n})
}
