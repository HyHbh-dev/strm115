package httpapi

// Telegram 里的订阅。
//
// 不另写一套订阅逻辑：bot 点「订阅」和网页点「确认订阅」最终都调
// Server.createSubscription —— 同样的 TMDB 校正、同样的专有追更日历。
// 这里只负责把 tgbot 的轻量结构翻译成 store.Subscription。

import (
	"context"
	"errors"
	"strconv"
	"strings"

	"strm115/internal/store"
	"strm115/internal/tgbot"
)

// botSubscribe 建一条订阅。重复订阅不算错误：机器人回「已经在追了」，
// 而不是把 store.ErrSubExists 当成一条红字报错。
func (s *Server) botSubscribe(ctx context.Context, req tgbot.SubscribeRequest) (*tgbot.SubscribeResult, error) {
	if req.TMDbID <= 0 {
		return nil, errors.New("缺少 TMDB ID，重新认一次片再订阅")
	}
	mediaType := req.Kind
	if mediaType != "movie" {
		mediaType = "tv"
	}
	year, _ := strconv.Atoi(strings.TrimSpace(req.Year))
	sub := store.Subscription{
		TMDbID:           req.TMDbID,
		MediaType:        mediaType,
		Title:            strings.TrimSpace(req.Title),
		OriginalTitle:    strings.TrimSpace(req.Original),
		Year:             year,
		WatermarkSeason:  req.WatermarkSeason,
		WatermarkEpisode: req.WatermarkEpisode,
	}
	// 电影没有集数，起播线对它没有意义，和网页创建接口保持一致。
	if mediaType == "movie" {
		sub.WatermarkSeason, sub.WatermarkEpisode = 0, 0
	}
	if sub.Title == "" {
		sub.Title = sub.OriginalTitle
	}

	id, total, err := s.createSubscription(ctx, &sub)
	if errors.Is(err, store.ErrSubExists) {
		return &tgbot.SubscribeResult{Already: true, Title: sub.Title}, nil
	}
	if err != nil {
		return nil, err
	}
	return &tgbot.SubscribeResult{
		ID: id, Title: sub.Title, CalendarTotal: total,
	}, nil
}

// botSubSeasons 给 TG 的「从哪一集开始订阅」列季集合。
//
// 只要季号和集数：逐集播出日由建订阅时的 materializeCalendar 自己拉，
// 这里多打一遍 TMDB 只会让按钮出现得更慢。
func (s *Server) botSubSeasons(ctx context.Context, tmdbID int) ([]tgbot.SubSeason, error) {
	if tmdbID <= 0 {
		return nil, errors.New("缺少 TMDB ID")
	}
	client := s.tmdbClient(ctx)
	if client == nil {
		return nil, errors.New("先到设置里填 TMDB API Key")
	}
	show, err := client.TV(ctx, tmdbID)
	if err != nil {
		return nil, err
	}
	out := make([]tgbot.SubSeason, 0, len(show.Seasons))
	for _, sn := range show.Seasons {
		// 特别篇（season 0）和没给集数的季都不该出现在起播线里。
		if sn.Season <= 0 || sn.EpisodeCount <= 0 {
			continue
		}
		out = append(out, tgbot.SubSeason{
			Season: sn.Season, Episodes: sn.EpisodeCount, Name: sn.Name,
		})
	}
	return out, nil
}
