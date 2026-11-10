package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// 老用户库里存着「旧四站默认值」，新增的 uindex / therarbg / 磁力猫就永远
// 搜不到。迁移必须只补这一行，且只补一次。
func TestMigrateAddsNewDefaultSites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 模拟升级前：没有 version 标记，res_sites 是旧四站默认值
	if _, err := first.db.Exec(`DELETE FROM settings WHERE key='res_sites_version'`); err != nil {
		t.Fatal(err)
	}
	if err := first.SetSetting(ctx, "res_sites", "tpb,nyaa,dmhy,mikan"); err != nil {
		t.Fatal(err)
	}
	first.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got, _ := upgraded.GetSetting(ctx, "res_sites")
	if want := "tpb,nyaa,dmhy,mikan,uindex,therarbg,clm"; got != want {
		t.Fatalf("迁移后 res_sites = %q，期望 %q", got, want)
	}
	if v, ok := upgraded.GetSetting(ctx, "res_sites_version"); !ok || v != "3" {
		t.Fatalf("version 标记不对: %q %v", v, ok)
	}

	// 用户自己勾过的配置不能被改回来（标记已存在，迁移不再跑）。
	if err := upgraded.SetSetting(ctx, "res_sites", "tpb,nyaa"); err != nil {
		t.Fatal(err)
	}
	upgraded.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	got, _ = again.GetSetting(ctx, "res_sites")
	if got != "tpb,nyaa" {
		t.Fatalf("用户改回两站后被迁移覆盖成 %q", got)
	}
}

// 老库的 subscriptions 没有 limit_* 两列。CREATE TABLE IF NOT EXISTS 不会
// 补列，而 subCols 的 SELECT 一出错就是整个订阅页打不开；必须由 ensureColumn
// 补，且重复启动幂等、老行默认值是 0。
//
// 这两列现在已经不是「限制线」了 —— 功能并进了 watermark_*，它们只剩迁移
// 用途（见 TestMigrateMergesLimitIntoWatermark）。
func TestMigrateAddsLegacyLimitColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`DROP TABLE subscriptions`); err != nil {
		t.Fatal(err)
	}
	if _, err := first.db.Exec(`CREATE TABLE subscriptions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, tmdb_id INTEGER NOT NULL,
		imdb_id TEXT NOT NULL DEFAULT '', media_type TEXT NOT NULL DEFAULT 'tv',
		title TEXT NOT NULL DEFAULT '', original_title TEXT NOT NULL DEFAULT '',
		year INTEGER NOT NULL DEFAULT 0, library_id INTEGER NOT NULL DEFAULT 0,
		save_cid TEXT NOT NULL DEFAULT '', quality TEXT NOT NULL DEFAULT '1080p',
		min_size INTEGER NOT NULL DEFAULT 0, max_size INTEGER NOT NULL DEFAULT 0,
		state TEXT NOT NULL DEFAULT 'watching',
		watermark_season INTEGER NOT NULL DEFAULT 0,
		watermark_episode INTEGER NOT NULL DEFAULT 0,
		total_episodes INTEGER NOT NULL DEFAULT 0,
		last_check_at TEXT NOT NULL DEFAULT '', next_check_at TEXT NOT NULL DEFAULT '',
		last_result TEXT NOT NULL DEFAULT '', fail_count INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		UNIQUE(media_type, tmdb_id))`); err != nil {
		t.Fatal(err)
	}
	// 一条升级前就存在的订阅：补列后应当默认 0/0。
	if _, err := first.db.Exec(`INSERT INTO subscriptions
		(tmdb_id, media_type, title, state, created_at, updated_at)
		VALUES (94605,'tv','边缘行者','watching','2026-01-01T00:00:00Z','2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	first.Close()

	up, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	rows, err := up.db.Query(`PRAGMA table_info(subscriptions)`)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]string{}
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt any
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		if name == "limit_season" || name == "limit_episode" {
			found[name] = fmt.Sprint(dflt)
		}
	}
	rows.Close()
	if found["limit_season"] != "0" || found["limit_episode"] != "0" {
		t.Fatalf("旧列没补上或默认值不对: %+v", found)
	}
	old, err := up.GetSubscription(ctx, 1)
	if err != nil || old == nil || old.WatermarkSeason != 0 || old.WatermarkEpisode != 0 {
		t.Fatalf("老订阅读不出来或起播线不是 0: %+v %v", old, err)
	}
}

// 限制线并进起播线：升级时取两条线里更靠后的那个当 watermark_*，再把
// limit_* 清零。用户以前设的「从第 5 集开始」不能因为合并就失效；
// 也不能因为重复启动被搬第二次（清过的列必须保持 0）。
func TestMigrateMergesLimitIntoWatermark(t *testing.T) {
	path := filepath.Join(t.TempDir(), "merge.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// 直接写 SQL：CreateSubscription 已经不带 limit_* 了，这里要造「升级前的老行」。
	ins := `INSERT INTO subscriptions
		(tmdb_id, media_type, title, state, watermark_season, watermark_episode,
		 limit_season, limit_episode, created_at, updated_at)
		VALUES (?, 'tv', ?, 'watching', ?, ?, ?, ?, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`
	cases := []struct {
		tmdbID       int
		wmS, wmE     int
		limS, limE   int
		wantS, wantE int
	}{
		{94605, 1, 2, 1, 5, 1, 5}, // 限制线更靠后 → 搬到起播线
		{100, 0, 0, 2, 3, 2, 3},   // 只有限制线 → 变成起播线
		{200, 3, 1, 1, 9, 3, 1},   // 限制线更靠前 → 保留原水位线
	}
	for i, c := range cases {
		if _, err := first.db.Exec(ins, c.tmdbID, fmt.Sprintf("剧%d", i),
			c.wmS, c.wmE, c.limS, c.limE); err != nil {
			t.Fatal(err)
		}
	}
	first.Close()

	up, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for i, c := range cases {
		sub, err := up.GetSubscription(ctx, int64(i+1))
		if err != nil || sub == nil {
			t.Fatalf("第 %d 条读不出来: %v", i, err)
		}
		if sub.WatermarkSeason != c.wantS || sub.WatermarkEpisode != c.wantE {
			t.Fatalf("第 %d 条起播线 = %d/%d，期望 %d/%d",
				i, sub.WatermarkSeason, sub.WatermarkEpisode, c.wantS, c.wantE)
		}
	}
	up.Close()

	// 再启动一次：迁移是幂等的，不能把已经清零的 limit_* 又搬回来覆盖用户后来改的线。
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var limS, limE int
	if err := again.db.QueryRow(`SELECT limit_season, limit_episode FROM subscriptions WHERE id = 1`).
		Scan(&limS, &limE); err != nil {
		t.Fatal(err)
	}
	if limS != 0 || limE != 0 {
		t.Fatalf("limit_* 没清零: %d/%d", limS, limE)
	}
	sub, err := again.GetSubscription(ctx, 1)
	if err != nil || sub == nil || sub.WatermarkSeason != 1 || sub.WatermarkEpisode != 5 {
		t.Fatalf("重复迁移改动了起播线: %+v %v", sub, err)
	}
}

// 停在 v2（上一次加站之后）的库只该补磁力猫，不能把用户后来取消的站也补回去。
func TestMigrateAppendsClmOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v2.db")
	ctx := context.Background()

	first, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.SetSetting(ctx, "res_sites", "tpb,nyaa,dmhy,mikan,uindex,therarbg"); err != nil {
		t.Fatal(err)
	}
	if err := first.SetSetting(ctx, "res_sites_version", "2"); err != nil {
		t.Fatal(err)
	}
	first.Close()

	upgraded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	got, _ := upgraded.GetSetting(ctx, "res_sites")
	if want := "tpb,nyaa,dmhy,mikan,uindex,therarbg,clm"; got != want {
		t.Fatalf("迁移后 res_sites = %q，期望 %q", got, want)
	}

	// 已经是当前版本的库再启动一次，勾选结果不该被改写
	if err := upgraded.SetSetting(ctx, "res_sites", "tpb,clm"); err != nil {
		t.Fatal(err)
	}
	upgraded.Close()
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got, _ := again.GetSetting(ctx, "res_sites"); got != "tpb,clm" {
		t.Fatalf("已经是 v3 还被改动：%q", got)
	}
}
