package subscribe

import (
	"testing"

	"strm115/internal/resource"
)

// Pick 是订阅里唯一会「下错东西」的地方，每个判定分支都要有反例盯着。
func TestPick(t *testing.T) {
	mk := func(title string, sizeGB int64, seeders int) resource.Item {
		return resource.Item{
			Title: title, Magnet: "magnet:?xt=urn:btih:" + title,
			Size: sizeGB << 30, Seeders: seeders,
		}
	}
	ep := PickOptions{Quality: "1080p", Season: 1, Episode: 5}

	// 1. 同画质有多个：挑做种最多的
	got := Pick([]resource.Item{
		mk("Some.Show.S01E05.1080p.WEB-DL", 3, 2),
		mk("Some.Show.S01E05.1080p.BluRay", 12, 40),
		mk("Some.Show.S01E05.1080p.HDTV", 1, 9),
	}, ep)
	if got == nil || got.Seeders != 40 {
		t.Fatalf("应挑做种最多的 1080p，实际 %+v", got)
	}

	// 2. 画质低于目标：一条都不该选
	if got := Pick([]resource.Item{
		mk("Some.Show.S01E05.720p.WEB-DL", 2, 50),
		mk("Some.Show.S01E05.480p", 1, 50),
	}, ep); got != nil {
		t.Fatalf("目标 1080p 时不该选 720p/480p，实际 %q", got.Title)
	}

	// 3. 目标 2160p：2160 优先，1440 也算过线，1080 被排除
	got = Pick([]resource.Item{
		mk("Some.Show.S01E05.1080p.WEB-DL", 5, 99),
		mk("Some.Show.S01E05.2160p.WEB-DL", 20, 3),
	}, PickOptions{Quality: "2160p", Season: 1, Episode: 5})
	if got == nil || got.Size != 20<<30 {
		t.Fatalf("目标 2160p 时应选 4K 那条，实际 %+v", got)
	}

	// 4. 认不出画质的按 1080p 兜底：目标 1080p 能用，目标 2160p 不能用。
	//    这条决定了「只有剧名+集数的 TG 帖子」到底能不能下。
	plain := []resource.Item{mk("赛博朋克 边缘行者 S01E05", 2, 7)}
	if got := Pick(plain, ep); got == nil {
		t.Fatalf("认不出画质的条目在 1080p 目标下应该能选")
	}
	if got := Pick(plain, PickOptions{Quality: "2160p", Season: 1, Episode: 5}); got != nil {
		t.Fatalf("认不出画质的条目在 2160p 目标下不该选")
	}
	// 认出来的优先于猜的（同为 1080 档）。
	got = Pick([]resource.Item{
		mk("Some.Show.S01E05", 2, 99),          // 猜的 1080
		mk("Some.Show.S01E05.1080p.WEB", 2, 1), // 真的 1080
	}, ep)
	if got == nil || got.Title != "Some.Show.S01E05.1080p.WEB" {
		t.Fatalf("同为 1080 时应优先真认出画质的那条，实际 %+v", got)
	}

	// 5. 季集不匹配一律不选 —— 下错集比漏一集糟得多
	for _, title := range []string{
		"Some.Show.S01E06.1080p",
		"Some.Show.S02E05.1080p",
		"Some.Show.S01.1080p.Complete", // 季包：最小闭环不认
		"Some.Show.E05.1080p",          // 只有集号没有季号
		"Some.Show.第5集.1080p",          // 中文集号暂不支持
	} {
		if got := Pick([]resource.Item{mk(title, 5, 9)}, ep); got != nil {
			t.Fatalf("%q 不该被选中（季集对不上）", title)
		}
	}

	// 6. 多集连播取第一集：S01E05-E06 能匹配 E05
	got = Pick([]resource.Item{mk("Some.Show.S01E05-E06.1080p", 4, 3)}, ep)
	if got == nil {
		t.Fatalf("S01E05-E06 应该能匹配 E05")
	}

	// 7. 体积区间：上下限都要生效
	opt := PickOptions{Quality: "1080p", Season: 1, Episode: 5, MinSize: 2, MaxSize: 10}
	if got := Pick([]resource.Item{mk("Some.Show.S01E05.1080p", 1, 9)}, opt); got != nil {
		t.Fatalf("小于最小体积不该选")
	}
	if got := Pick([]resource.Item{mk("Some.Show.S01E05.1080p", 30, 9)}, opt); got != nil {
		t.Fatalf("大于最大体积不该选")
	}
	if got := Pick([]resource.Item{mk("Some.Show.S01E05.1080p", 4, 9)}, opt); got == nil {
		t.Fatalf("区间内应该能选")
	}
	// 体积未知（0）不参与过滤，否则一堆条目会被无故排除
	if got := Pick([]resource.Item{{Title: "Some.Show.S01E05.1080p", Magnet: "magnet:?x"}}, opt); got == nil {
		t.Fatalf("体积未知的条目不该被体积条件排除")
	}

	// 8. 只有网盘链接 / 详情页的条目交不了离线
	if got := Pick([]resource.Item{{
		Title: "Some.Show.S01E05.1080p", ShareCode: "abc123",
		Links: []string{"https://115.com/s/abc"}, Size: 5 << 30,
	}}, ep); got != nil {
		t.Fatalf("只有 115 分享码的条目不该被选中（要走解锁转存，不是离线）")
	}
	// .torrent 直链可以（Mikan 这种站没有 info hash）
	if got := Pick([]resource.Item{{
		Title: "Some.Show.S01E05.1080p", Torrent: "https://mikan.example/1.torrent",
	}}, ep); got == nil || URI(*got) != "https://mikan.example/1.torrent" {
		t.Fatalf(".torrent 直链应该可选，且 URI 要原样返回")
	}

	// 9. 电影：不需要季集，用户要 2160p 就只挑 4K
	movie := PickOptions{Quality: "2160p"}
	got = Pick([]resource.Item{
		mk("Movie.2026.1080p.WEB-DL", 3, 88),
		mk("Movie.2026.2160p.HDR.WEB-DL", 15, 2),
	}, movie)
	if got == nil || got.Size != 15<<30 {
		t.Fatalf("电影应挑 4K，实际 %+v", got)
	}

	// 10. 空结果不 panic
	if got := Pick(nil, ep); got != nil {
		t.Fatalf("空列表应返回 nil")
	}
}

// 季集号解析是最容易写成「看起来对」的一段，单独钉住几种写法。
func TestParseEpisode(t *testing.T) {
	cases := []struct {
		title string
		s, e  int
		ok    bool
	}{
		{"Show.S02E05.1080p", 2, 5, true},
		{"Show.S02.E05.1080p", 2, 5, true},
		{"Show.S02 E05 1080p", 2, 5, true},
		{"Show.s02e05", 2, 5, true},
		{"Show.S02E05-E06", 2, 5, true},
		{"Show.S2E5.1080p", 2, 5, true},
		{"Show.S02.Complete.1080p", 0, 0, false},
		{"Show.EP05.1080p", 0, 0, false},
		{"Show.第5集", 0, 0, false},
		{"Show.2022.1080p", 0, 0, false},
	}
	for _, c := range cases {
		s, e, ok := parseEpisode(c.title)
		if ok != c.ok || s != c.s || e != c.e {
			t.Errorf("parseEpisode(%q) = (%d,%d,%v)，期望 (%d,%d,%v)",
				c.title, s, e, ok, c.s, c.e, c.ok)
		}
	}
}

// 画质识别：2160p 不能被当成 160p，1080i 也要认。
func TestQualityOf(t *testing.T) {
	cases := []struct {
		title string
		rank  int
		known bool
	}{
		{"Show.2026.2160p.HDR.WEB-DL", 2160, true},
		{"Show.2026.4K.WEB-DL", 2160, true},
		{"Show.2026.UHD.BluRay", 2160, true},
		{"Show.2026.1440p", 1440, true},
		{"Show.2026.1080p.BluRay", 1080, true},
		{"Show.2026.1080i.HDTV", 1080, true},
		{"Show.2026.720p.WEB", 720, true},
		{"Show.2026.480p", 480, true},
		{"Show.2026.WEB-DL", 1080, false},
		// 分辨率以外的数字不能被误认
		{"Show.2026.HEVC.10bit.DDP5.1", 1080, false},
	}
	for _, c := range cases {
		rank, known := qualityOf(c.title)
		if rank != c.rank || known != c.known {
			t.Errorf("qualityOf(%q) = (%d,%v)，期望 (%d,%v)", c.title, rank, known, c.rank, c.known)
		}
	}
	// 订阅设置里可能写 "4k" / "2160P" 这类，都要能换成档位
	for _, q := range []string{"2160p", "4K", "2160P", "1080p", "720P"} {
		if qualityRank(q) == 0 {
			t.Errorf("qualityRank(%q) 不该是 0", q)
		}
	}
	if qualityRank("") != 0 {
		t.Errorf("画质留空应表示不限（0）")
	}
}
