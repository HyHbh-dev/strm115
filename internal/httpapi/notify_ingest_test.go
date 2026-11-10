package httpapi

// 入库通知的单元测试。
//
// 这组测试盯的是「推出去的那一条到底长什么样」：手机上一条消息只值一眼，
// 所以格式（集数区间、画质、体积）和「只推新片」这两件事都必须被钉住 ——
// 它们出错时不会报错，只会悄悄刷屏或者推出错信息。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"strm115/internal/store"
)

func TestEpisodeSpan(t *testing.T) {
	cases := []struct {
		name string
		rows []store.Media
		want string
	}{
		{"整季", []store.Media{{Season: 1, Episode: 1}, {Season: 1, Episode: 10}}, "S01E01-E10"},
		{"单集", []store.Media{{Season: 2, Episode: 5}}, "S02E05"},
		{"跨季说不出区间，改报集数", []store.Media{{Season: 1, Episode: 1}, {Season: 2, Episode: 1}}, "2 集"},
		{"没有季集信息", []store.Media{{}}, ""},
		{"有季无集", []store.Media{{Season: 1}}, ""},
	}
	for _, c := range cases {
		if got := episodeSpan(c.rows); got != c.want {
			t.Fatalf("%s: 期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}

func TestQualityOf(t *testing.T) {
	cases := []struct {
		rel  string
		want string
	}{
		{"电影/碟中谍8.2025/碟中谍8.2025.2160p.HDR.WEB-DL.mkv", "2160P HDR WEB-DL"},
		{"剧集/赛博朋克/Season 1/赛博朋克.S01E01.1080p.mkv", "1080P"},
		{"电影/老片/老片.2019.REMUX.1080p.mkv", "1080P REMUX"},
		{"电影/某片/某片.mkv", ""},
	}
	for _, c := range cases {
		got := qualityOf([]store.Media{{RelPath: c.rel}})
		if got != c.want {
			t.Fatalf("%s: 期望 %q，实际 %q", c.rel, c.want, got)
		}
	}
}

func TestIngestText(t *testing.T) {
	rows := []store.Media{{
		RelPath: "电影/空枪 (2026)/空枪.2026.2160p.HDR.WEB-DL.mkv",
		Kind:    "movie", Title: "空枪", Year: "2026", Rating: "7.2",
		Genres: "犯罪, 剧情", PosterFile: "电影/空枪 (2026)/poster.jpg",
	}}
	sizes := map[string]int64{rows[0].RelPath: 32100000000}
	title, body, poster := ingestText("/media", rows, sizes)

	if title != "🆕 空枪（2026）" {
		t.Fatalf("标题不对: %q", title)
	}
	for _, want := range []string{"🎬 电影", "犯罪", "⭐ 7.2", "2160P HDR WEB-DL", "1 个文件", "GB"} {
		if !strings.Contains(body, want) {
			t.Fatalf("正文缺 %q: %q", want, body)
		}
	}
	if strings.Contains(body, "剧情") {
		t.Fatalf("类型只该取第一个，否则手机上挤成两行: %q", body)
	}
	// 测试环境里磁盘上没有这张图，必须给空串 —— 渠道据此退回纯文本。
	if poster != "" {
		t.Fatalf("图不存在时不该给路径: %q", poster)
	}
}

func TestIngestTextSeries(t *testing.T) {
	rows := []store.Media{
		{RelPath: "剧集/赛博朋克 (2022)/Season 1/S01E01.1080p.mkv", Kind: "series",
			Title: "赛博朋克：边缘行者", Year: "2022", Season: 1, Episode: 1, Rating: "8.5"},
		{RelPath: "剧集/赛博朋克 (2022)/Season 1/S01E10.1080p.mkv", Kind: "series",
			Title: "赛博朋克：边缘行者", Year: "2022", Season: 1, Episode: 10, Rating: "8.5"},
	}
	sizes := map[string]int64{rows[0].RelPath: 4 << 30, rows[1].RelPath: 4 << 30}
	_, body, _ := ingestText("", rows, sizes)
	for _, want := range []string{"📺 剧集", "S01E01-E10", "2 个文件", "GB"} {
		if !strings.Contains(body, want) {
			t.Fatalf("正文缺 %q: %q", want, body)
		}
	}
}

func TestIsNewGroup(t *testing.T) {
	rows := []store.Media{{RelPath: "a.1.mkv"}, {RelPath: "a.2.mkv"}}
	before := map[string]struct{}{"a.1.mkv": {}}
	if !isNewGroup(rows, before) {
		t.Fatal("组里有一条是新的，就该算新入库")
	}
	both := map[string]struct{}{"a.1.mkv": {}, "a.2.mkv": {}}
	if isNewGroup(rows, both) {
		t.Fatal("全都见过就不该再推一遍")
	}
}

// TestPushNewIngestsOnlyFresh 走完整链路：台账 diff → 组消息 → 渠道扇出。
// 用一个本地 webhook 收消息，比断言内部状态更接近真实行为。
func TestPushNewIngestsOnlyFresh(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()

	acctID, err := st.CreateAccount(ctx, "115", "测试账号", "{}")
	if err != nil {
		t.Fatalf("建账号失败: %v", err)
	}
	libID, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acctID, Name: "电影", LocalPath: "/tmp/media", CollectionType: "movies",
	})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	lib, err := st.GetLibrary(ctx, libID)
	if err != nil {
		t.Fatal(err)
	}
	oldRel := "电影/旧片 (2020)/旧片.2020.1080p.mkv"
	newRel := "电影/空枪 (2026)/空枪.2026.2160p.HDR.WEB-DL.mkv"
	for _, m := range []store.Media{
		{LibraryID: libID, RelPath: oldRel, Kind: "movie", Title: "旧片", Year: "2020", Status: "ok"},
		{LibraryID: libID, RelPath: newRel, Kind: "movie", Title: "空枪", Year: "2026", Status: "ok", Rating: "7.2"},
	} {
		if err := st.UpsertMedia(ctx, m); err != nil {
			t.Fatalf("写台账失败: %v", err)
		}
	}

	var mu sync.Mutex
	got := make([]map[string]string, 0, 2)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()
	if err := st.SetSetting(ctx, settingNotifyConfig,
		`{"webhook":{"enabled":true,"url":"`+ts.URL+`","method":"POST"}}`); err != nil {
		t.Fatal(err)
	}
	s.reloadNotify(ctx)

	s.pushNewIngests(lib, map[string]struct{}{oldRel: {}})

	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for count() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if count() != 1 {
		t.Fatalf("应只推 1 条（旧片不该再推），实际 %d", count())
	}
	mu.Lock()
	msg := got[0]
	mu.Unlock()
	if msg["type"] != "ingest" {
		t.Fatalf("消息类型应是 ingest，实际 %q", msg["type"])
	}
	if !strings.Contains(msg["title"], "空枪") {
		t.Fatalf("标题应带片名，实际 %q", msg["title"])
	}
	if strings.Contains(msg["title"]+msg["content"], "旧片") {
		t.Fatalf("旧片不该出现在通知里: %#v", msg)
	}

	// 再跑一次（这次两片都见过）→ 不该再多一条
	s.pushNewIngests(lib, map[string]struct{}{oldRel: {}, newRel: {}})
	time.Sleep(300 * time.Millisecond)
	if n := count(); n != 1 {
		t.Fatalf("没有新片时不该再推，实际累计 %d 条", n)
	}
}

// 海报落在「发布组子目录」里也要找得到。
//
// 整理后的真实布局是 `<片名 (年份)>/<发布组>/xxx.strm`，刮削把 poster.jpg 写在
// 放 .strm 的那一层（电影独占目录），而不是 `<片名 (年份)>` 根上。入库卡片
// 取不到图就只剩文字 —— 用户看到的就是「没显示影视封面」。
func TestPosterPathFindsReleaseFolderPoster(t *testing.T) {
	root := t.TempDir()
	rel := "Heart Of The Beast (2026)/Heart.Of.The.Beast.2026.2160p.WEB-DL/poster.jpg"
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte("\xff\xd8JPEG"), 0o644); err != nil {
		t.Fatal(err)
	}
	rows := []store.Media{{
		RelPath:    "Heart Of The Beast (2026)/Heart.Of.The.Beast.2026.2160p.WEB-DL/x.strm",
		Kind:       "movie",
		Title:      "Heart Of The Beast",
		Year:       "2026",
		PosterFile: rel,
	}}
	_, _, poster := ingestText(root, rows, nil)
	if poster != abs {
		t.Fatalf("应返回海报绝对路径 %q，实际 %q", abs, poster)
	}
}
