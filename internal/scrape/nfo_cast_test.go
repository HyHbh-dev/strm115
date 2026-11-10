package scrape

import (
	"strings"
	"testing"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

// 演员表写进 nfo 的三种情况。这里守的是「Emby 里到底有没有头像」这件事：
// <thumb> 缺失时 Emby 会显示灰底占位图，而且不会自己去 TMDB 补 ——
// 因为这些人是我们用本地 nfo 直接创建出来的，没有外部 ID。
func TestBuildNFOCast(t *testing.T) {
	withDetail := Meta{
		IMDbID: "tt1567937",
		Title:  "空枪",
		Year:   "2026",
		CastDetail: []tmdb.CastMember{
			{Name: "朱一龙", Role: "Wan Ziqiang", Image: "https://image.tmdb.org/t/p/w185/abc.jpg"},
			{Name: "梁家辉", Role: "Chen Huiyan", Image: ""},
		},
	}
	raw, err := BuildMovieNFO(withDetail)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)

	if !strings.Contains(out, "<name>朱一龙</name>") {
		t.Errorf("应该写中文演员名，实际：\n%s", out)
	}
	if !strings.Contains(out, "<role>Wan Ziqiang</role>") {
		t.Errorf("应该写角色名，实际：\n%s", out)
	}
	if !strings.Contains(out, "<thumb>https://image.tmdb.org/t/p/w185/abc.jpg</thumb>") {
		t.Errorf("有头像的演员必须写 <thumb>，否则 Emby 里没有图：\n%s", out)
	}
	// 没有头像的演员不能写一个空 <thumb>：Emby 会把它当成「有图但下载失败」，
	// 反而比没有标签更糟。
	if strings.Contains(out, "<thumb></thumb>") {
		t.Errorf("没有头像时不该写空 thumb：\n%s", out)
	}
	// 顺序必须从 0 开始连续，Emby 按 order 决定演员横排的先后。
	if !strings.Contains(out, "<order>0</order>") || !strings.Contains(out, "<order>1</order>") {
		t.Errorf("order 应该从 0 连续递增：\n%s", out)
	}
}

// 没配 TMDB Key 时退回老格式：只有名字，一个 <thumb> 都不该出现。
func TestBuildNFOCastFallback(t *testing.T) {
	raw, err := BuildTVShowNFO(Meta{
		IMDbID: "tt1234567",
		Title:  "Some Show",
		Cast:   []string{"Tom Cruise", "Hayley Atwell"},
	})
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "<name>Tom Cruise</name>") {
		t.Errorf("应该保留原来的演员名：\n%s", out)
	}
	if strings.Contains(out, "<thumb>") {
		t.Errorf("没有明细时不该凭空写 thumb：\n%s", out)
	}
}

// 单集 nfo 也带演员表，Emby 的单集详情页同样会显示演员。
func TestBuildEpisodeNFOCast(t *testing.T) {
	raw, err := BuildEpisodeNFO(Meta{
		IMDbID:     "tt1234567",
		Title:      "Some Show",
		CastDetail: []tmdb.CastMember{{Name: "张三", Role: "李四", Image: "https://img/x.jpg"}},
	}, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	out := string(raw)
	if !strings.Contains(out, "<name>张三</name>") || !strings.Contains(out, "<thumb>https://img/x.jpg</thumb>") {
		t.Errorf("单集 nfo 也要带中文名与头像：\n%s", out)
	}
}

// 「不覆盖用户手改」和「补演员头像」这两条要能同时成立：
// 老 nfo 里有 <plot>（说明是旧版本写的），但它缺 <thumb>，
// 这时必须重写，否则升级后 emby 里还是一片灰头像。
func TestNeedsCastThumb(t *testing.T) {
	oldWithPlot := `<movie><title>x</title><plot>简介</plot>
  <actor><name>梁家辉</name><order>0</order></actor></movie>`
	newData := []byte(`<movie><title>x</title><plot>简介</plot>
  <actor><name>梁家辉</name><order>0</order><thumb>https://img/a.jpg</thumb></actor></movie>`)

	if !needsCastThumb(oldWithPlot, newData) {
		t.Error("老 nfo 缺 thumb、新内容有 thumb 时应该重写")
	}

	// 已经有 thumb 的不要动：那可能是用户自己换过头像。
	already := strings.Replace(oldWithPlot, "<order>0</order>",
		"<order>0</order><thumb>https://old/x.jpg</thumb>", 1)
	if needsCastThumb(already, newData) {
		t.Error("已经有 thumb 时不该重写")
	}

	// 新内容里根本没有 thumb（没配 TMDB Key）时别白写一遍。
	if needsCastThumb(oldWithPlot, []byte(oldWithPlot)) {
		t.Error("新内容没有 thumb 时不该判定为需要重写")
	}
}

// 写 nfo 前会把「元数据」和「台账行」重新拼一份 Meta。这一步漏字段不会
// 报错，只会让东西悄悄消失：CastDetail 没被复制时，nfo 的 <actor> 只剩名字，
// Emby 里的头像永远是灰的，而所有单测都还是绿的。
func TestNfoMetaKeepsCastDetail(t *testing.T) {
	src := Meta{
		Title: "空枪",
		CastDetail: []tmdb.CastMember{
			{Name: "朱一龙", Role: "Wan Ziqiang", Image: "https://img/z.jpg"},
		},
		Episodes: []Episode{{Season: 1, Episode: 1, Title: "第一集"}},
	}
	m := &store.Media{IMDbID: "tt1", MetaTitle: "空枪", Cast: "朱一龙, 檀健次"}

	out := nfoMeta(src, m)
	if len(out.CastDetail) != 1 || out.CastDetail[0].Image == "" {
		t.Fatalf("CastDetail 必须原样带过去，实际 %+v", out.CastDetail)
	}
	if out.Title != "空枪" || out.IMDbID != "tt1" {
		t.Fatalf("基础字段不对：%+v", out)
	}
	if len(out.Episodes) != 1 {
		t.Fatalf("单集清单不能丢：%+v", out.Episodes)
	}

	// 生成的 nfo 里确实有 thumb —— 这一条把「装配」和「渲染」串起来验。
	raw, err := BuildMovieNFO(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "<thumb>https://img/z.jpg</thumb>") {
		t.Errorf("装配之后 nfo 里仍然没有头像：\n%s", raw)
	}
}

// IMDb 与 TMDB 的类型对不上时，要按我们认出来的类型挑 id。
// 拿错 id 的后果不是报错，而是演员表静默为空。
func TestPickCreditsID(t *testing.T) {
	cases := []struct {
		kind      string
		movie, tv int
		wantKind  string
		wantID    int
	}{
		{KindSeries, 1819914, 0, tmdb.KindMovie, 1819914}, // 剧集只有电影 id：退回电影
		{KindSeries, 0, 105248, tmdb.KindTV, 105248},
		{KindSeries, 1819914, 105248, tmdb.KindTV, 105248}, // 两边都有：优先 tv
		{KindMovie, 1567937, 0, tmdb.KindMovie, 1567937},
		{KindMovie, 0, 4242, tmdb.KindTV, 4242}, // 电影只有 tv id：退回 tv
	}
	for _, c := range cases {
		k, id := pickCreditsID(c.kind, c.movie, c.tv)
		if k != c.wantKind || id != c.wantID {
			t.Errorf("pickCreditsID(%q,%d,%d) = %q,%d，期望 %q,%d",
				c.kind, c.movie, c.tv, k, id, c.wantKind, c.wantID)
		}
	}
}
