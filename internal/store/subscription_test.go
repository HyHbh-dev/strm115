package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

// 订阅表的两个不变量比字段是否落库更重要：
// 同一部作品不能订两次（否则两个任务抢一个水位线），水位线不能倒退
// （否则并发下会重复下载同一集）。这两条单独测。
func TestSubscriptions(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.CreateSubscription(ctx, Subscription{
		TMDbID: 94605, IMDbID: "tt12590266", MediaType: "tv",
		Title: "赛博朋克：边缘行者", OriginalTitle: "Cyberpunk: Edgerunners", Year: 2022,
		Quality: "1080p", TotalEpisodes: 10,
	})
	if err != nil {
		t.Fatalf("建订阅失败: %v", err)
	}

	// 同一部剧再来一次：必须报 ErrSubExists，而且不能多出一行。
	if _, err := s.CreateSubscription(ctx, Subscription{
		TMDbID: 94605, MediaType: "tv", Title: "重复订阅",
	}); !errors.Is(err, ErrSubExists) {
		t.Fatalf("重复订阅应返回 ErrSubExists，实际 %v", err)
	}
	all, err := s.ListSubscriptions(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("应有 1 条，实际 %d —— 唯一约束没生效", len(all))
	}
	if all[0].Title != "赛博朋克：边缘行者" {
		t.Fatalf("重复插入把原标题覆盖了: %+v", all[0])
	}
	// 电影和剧集共用 tmdb id 空间，同 id 不同类型不算重复。
	if _, err := s.CreateSubscription(ctx, Subscription{
		TMDbID: 94605, MediaType: "movie", Title: "同 id 的电影",
	}); err != nil {
		t.Fatalf("不同类型同 id 不该被拒: %v", err)
	}

	// 新建的订阅必须立刻到期：用户点完「订阅」，不该等一个 tick 才去查。
	due, err := s.DueSubscriptions(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 2 {
		t.Fatalf("新建订阅应立即到期，实际 due=%d", len(due))
	}

	// 水位线只前进。乱序回来的旧值不能把进度拉回去。
	if err := s.SetSubscriptionWatermark(ctx, id, 1, 5); err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscriptionWatermark(ctx, id, 1, 3); err != nil { // 迟到的旧结果
		t.Fatal(err)
	}
	sub, err := s.GetSubscription(ctx, id)
	if err != nil || sub == nil {
		t.Fatalf("取订阅失败: %v", err)
	}
	if sub.WatermarkSeason != 1 || sub.WatermarkEpisode != 5 {
		t.Fatalf("水位线被拉回去了: S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}
	// 跨季要能推进：S01E10 → S02E01
	if err := s.SetSubscriptionWatermark(ctx, id, 2, 1); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.WatermarkSeason != 2 || sub.WatermarkEpisode != 1 {
		t.Fatalf("跨季推进失败: S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}
	if sub.Progress() != "S02E01" {
		t.Fatalf("进度文案错了: %q", sub.Progress())
	}

	// 暂停：轮询器完全不碰，恢复时立刻到期。
	if err := s.UpdateSubscription(ctx, id, "2160p", -1, -1, 0, "", SubPaused); err != nil {
		t.Fatal(err)
	}
	due, err = s.DueSubscriptions(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == id {
			t.Fatalf("暂停的订阅不该出现在到期列表里")
		}
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.Quality != "2160p" || sub.State != SubPaused {
		t.Fatalf("画质/状态没存上: %+v", sub)
	}

	// 恢复到 watching：next_check_at 要被拉回现在，否则用户点完「继续」
	// 还要等一个周期才生效。
	if err := s.UpdateSubscription(ctx, id, "", -1, -1, 0, "", SubWatching); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.NextCheckAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("恢复订阅后应立即到期，实际 %v", sub.NextCheckAt)
	}

	// 分类计数：tv 在 watching，movie 还在 watching。
	counts, err := s.CountSubscriptions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts[SubWatching] != 2 {
		t.Fatalf("watching 应为 2，实际 %d（%v）", counts[SubWatching], counts)
	}

	// 删除
	ok, err := s.DeleteSubscription(ctx, id)
	if err != nil || !ok {
		t.Fatalf("删除失败: ok=%v err=%v", ok, err)
	}
	if ok, _ := s.DeleteSubscription(ctx, id); ok {
		t.Fatalf("重复删除应返回 false")
	}
	if n, _ := s.ListSubscriptions(ctx, ""); len(n) != 1 {
		t.Fatalf("删完应剩 1 条，实际 %d", len(n))
	}
}

// 到期的判定是轮询器的入口条件，边界值最容易写错成「<=」写成「<」。
func TestDueSubscriptionsWindow(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()

	id, err := s.CreateSubscription(ctx, Subscription{TMDbID: 1, MediaType: "tv", Title: "已追平"})
	if err != nil {
		t.Fatal(err)
	}
	// 手动推到未来两小时：这一轮不该再查它。
	if err := s.MarkSubscriptionChecked(ctx, id, time.Now().Add(2*time.Hour), SubWaiting, "已追平", 0); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueSubscriptions(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 0 {
		t.Fatalf("还没到期的订阅不该返回，实际 %d 条", len(due))
	}
	// 两小时后就该轮到它。
	due, err = s.DueSubscriptions(ctx, time.Now().Add(3*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].LastResult != "已追平" {
		t.Fatalf("到期后应返回这条，实际 %+v", due)
	}
}

// Progress 是页面上唯一的进度文案，几种边界都要有确定答案。
func TestSubscriptionProgress(t *testing.T) {
	cases := []struct {
		sub  Subscription
		want string
	}{
		{Subscription{MediaType: "movie", State: SubDone}, "已入库"},
		{Subscription{MediaType: "movie", State: SubWaiting}, "等待资源"},
		{Subscription{MediaType: "tv", WatermarkSeason: 1, WatermarkEpisode: 7}, "S01E07"},
		{Subscription{MediaType: "tv", TotalEpisodes: 10}, "0 / 10 集"},
		{Subscription{MediaType: "tv"}, "还没开始"},
	}
	for _, c := range cases {
		if got := c.sub.Progress(); got != c.want {
			t.Errorf("Progress() = %q，期望 %q", got, c.want)
		}
	}
}

// 起播线两个入口的差别：程序推进只许前进（并发下乱序回来的旧值不能把
// 进度拉回去），用户手改可前进也可退，而且改完要立刻到期。
func TestSubscriptionWatermark(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	id, err := s.CreateSubscription(ctx, Subscription{TMDbID: 94605, Title: "边缘行者"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetSubscriptionWatermark(ctx, id, 1, 4); err != nil {
		t.Fatal(err)
	}
	// 迟到的旧结果不能把线拉回去。
	if err := s.SetSubscriptionWatermark(ctx, id, 1, 2); err != nil {
		t.Fatal(err)
	}
	sub, err := s.GetSubscription(ctx, id)
	if err != nil || sub == nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if sub.WatermarkSeason != 1 || sub.WatermarkEpisode != 4 {
		t.Fatalf("自动推进被旧值拉回去了: S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}
	// 跨季要能推进：S01E04 → S02E01
	if err := s.SetSubscriptionWatermark(ctx, id, 2, 1); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.WatermarkSeason != 2 || sub.WatermarkEpisode != 1 {
		t.Fatalf("跨季推进失败: S%02dE%02d", sub.WatermarkSeason, sub.WatermarkEpisode)
	}

	future := time.Now().Add(24 * time.Hour)
	if err := s.MarkSubscriptionChecked(ctx, id, future, SubWaiting, "已追平", 0); err != nil {
		t.Fatal(err)
	}
	// 用户手改：允许后退（前面那些集不补了），且要立刻到期。
	if err := s.ForceSubscriptionWatermark(ctx, id, 2, 3); err != nil {
		t.Fatal(err)
	}
	sub, err = s.GetSubscription(ctx, id)
	if err != nil || sub == nil {
		t.Fatalf("读订阅失败: %v", err)
	}
	if sub.WatermarkSeason != 2 || sub.WatermarkEpisode != 3 {
		t.Fatalf("起播线没存上: %+v", sub)
	}
	if !sub.NextCheckAt.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("改完起播线应立刻到期，实际 next_check_at=%v", sub.NextCheckAt)
	}
	if got := sub.Progress(); got != "S02E03" {
		t.Fatalf("Progress() = %q，期望 S02E03", got)
	}
	// 0/0 = 从头追，也是合法值。
	if err := s.ForceSubscriptionWatermark(ctx, id, 0, 0); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.WatermarkSeason != 0 || sub.WatermarkEpisode != 0 {
		t.Fatalf("清零没生效: %+v", sub)
	}
	if got := sub.Progress(); got != "还没开始" {
		t.Fatalf("清零后 Progress() = %q", got)
	}

	// 暂停：轮询器完全不碰，恢复时立刻到期。
	if err := s.UpdateSubscription(ctx, id, "2160p", -1, -1, 0, "", SubPaused); err != nil {
		t.Fatal(err)
	}
	due, err := s.DueSubscriptions(ctx, time.Now(), 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range due {
		if d.ID == id {
			t.Fatalf("暂停的订阅不该出现在到期列表里")
		}
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.Quality != "2160p" || sub.State != SubPaused {
		t.Fatalf("画质/状态没存上: %+v", sub)
	}
	// 恢复到 watching：next_check_at 要被拉回现在，否则用户点完「继续」
	// 还要等一个周期才生效。
	if err := s.UpdateSubscription(ctx, id, "", -1, -1, 0, "", SubWatching); err != nil {
		t.Fatal(err)
	}
	if sub, _ = s.GetSubscription(ctx, id); sub.NextCheckAt.After(time.Now().Add(time.Second)) {
		t.Fatalf("恢复订阅后应立即到期，实际 %v", sub.NextCheckAt)
	}
}
