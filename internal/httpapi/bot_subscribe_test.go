package httpapi

import (
	"context"
	"testing"

	"strm115/internal/store"
	"strm115/internal/tgbot"
)

// TG 里的「⭐订阅」必须和网页走同一条创建链路：TMDB 校正、专有追更日历、
// 起播线跳过、重复订阅给 Already。否则机器人订出来的剧会没有日历，
// 变成一条永远不搜资源的死订阅。
func TestBotSubscribeSharesCreationAndCalendar(t *testing.T) {
	s, st := newServerWithStore(t)
	withFakeTVSeasons(t, s, st)
	ctx := context.Background()

	seasons, err := s.botSubSeasons(ctx, 94605)
	if err != nil || len(seasons) != 2 {
		t.Fatalf("应拿到两季，实际 %v / %v", seasons, err)
	}
	if seasons[0].Season != 1 || seasons[0].Episodes != 2 {
		t.Fatalf("季集数不对：%+v", seasons[0])
	}

	res, err := s.botSubscribe(ctx, tgbot.SubscribeRequest{
		TMDbID: 94605, Kind: "tv", Title: "乱填的名字", Original: "Test Show",
		Year: "2020", WatermarkSeason: 1, WatermarkEpisode: 1,
	})
	if err != nil {
		t.Fatalf("TG 订阅失败：%v", err)
	}
	if res.ID <= 0 || res.CalendarTotal != 4 {
		t.Fatalf("应建成订阅并物化 4 集日历，实际 %+v", res)
	}
	// createSubscription 会就地用 TMDB 校正标题，回执不该显示乱填的名字。
	if res.Title != "测试剧" {
		t.Errorf("剧集标题应被 TMDB 校正，实际 %q", res.Title)
	}

	eps, err := st.ListSubscriptionEpisodes(ctx, res.ID)
	if err != nil || len(eps) != 4 {
		t.Fatalf("日历应有 4 行，实际 %d / %v", len(eps), err)
	}
	for _, e := range eps {
		if e.Code() == "S01E01" && e.Status != store.SubEpSkipped {
			t.Errorf("起播线及之前应跳过，实际 %s = %s", e.Code(), e.Status)
		}
	}

	// 再点一次订阅：不该报错，也不该建出第二条。
	again, err := s.botSubscribe(ctx, tgbot.SubscribeRequest{
		TMDbID: 94605, Kind: "tv", Title: "测试剧",
	})
	if err != nil || !again.Already {
		t.Fatalf("重复订阅应回 Already，实际 %+v / %v", again, err)
	}
}

// 电影订阅不带起播线，也不去查剧集接口（TMDB 假服务器没有 /movie 路由）。
func TestBotSubscribeMovieIgnoresWatermark(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()
	res, err := s.botSubscribe(ctx, tgbot.SubscribeRequest{
		TMDbID: 575265, Kind: "movie", Title: "碟中谍8：最终清算",
		Year: "2025", WatermarkSeason: 2, WatermarkEpisode: 3,
	})
	if err != nil || res == nil || res.ID <= 0 {
		t.Fatalf("电影订阅没建成：%+v / %v", res, err)
	}
	sub, err := st.GetSubscription(ctx, res.ID)
	if err != nil || sub == nil {
		t.Fatalf("订阅没落库：%v", err)
	}
	if sub.MediaType != "movie" || sub.WatermarkSeason != 0 || sub.WatermarkEpisode != 0 {
		t.Errorf("电影不该有起播线：%+v", sub)
	}
}
