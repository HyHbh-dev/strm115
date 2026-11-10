package scrape

import (
	"errors"
	"io/fs"
	"strings"
	"testing"

	"strm115/internal/tmdb"
)

func TestPickExactTitleAndYear(t *testing.T) {
	res := []tmdb.MetaHit{
		{ID: 693134, Title: "沙丘2", Original: "Dune: Part Two", Year: "2024", Kind: tmdb.KindMovie},
		{ID: 841, Title: "沙丘", Original: "Dune", Year: "1984", Kind: tmdb.KindMovie},
		{ID: 438631, Title: "沙丘", Original: "Dune", Year: "2021", Kind: tmdb.KindMovie},
	}
	got, ok := Pick(Parsed{Title: "Dune Part Two", Year: "2024", Kind: KindMovie}, res)
	if !ok || got.ID != 693134 {
		t.Fatalf("应选中续集，实际 %+v ok=%v", got, ok)
	}
}

// TestPickRejectsWrongType 电影名撞上同名剧集时不能选错类型。
func TestPickRejectsWrongType(t *testing.T) {
	res := []tmdb.MetaHit{
		{ID: 60622, Title: "冰血暴", Original: "Fargo", Year: "2014", Kind: tmdb.KindTV},
		{ID: 116282, Title: "冰血暴", Original: "Fargo", Year: "1996", Kind: tmdb.KindMovie},
	}
	got, ok := Pick(Parsed{Title: "Fargo", Year: "1996", Kind: KindMovie}, res)
	if !ok || got.ID != 116282 {
		t.Fatalf("应选中电影版，实际 %+v ok=%v", got, ok)
	}
}

// TestPickGivesUpOnJunk 完全不像的结果必须放弃，不能硬塞一个。
func TestPickGivesUpOnJunk(t *testing.T) {
	res := []tmdb.MetaHit{
		{ID: 27770872, Title: "Trailer of the Film That Will Never Exist", Year: "2023", Kind: tmdb.KindMovie},
		{ID: 0, Title: "没有 ID 的脏数据", Year: "2019", Kind: tmdb.KindMovie},
	}
	if got, ok := Pick(Parsed{Title: "完全不相干的片名", Year: "2019", Kind: KindMovie}, res); ok {
		t.Fatalf("不该匹配上，却选中 %+v", got)
	}
}

// TestPickChineseTitle 中文片名对不上 TMDB 原名时靠年份 + 类型兜底。
func TestPickChineseTitle(t *testing.T) {
	res := []tmdb.MetaHit{
		{ID: 13539646, Title: "流浪地球2", Original: "The Wandering Earth II", Year: "2023", Kind: tmdb.KindMovie},
		{ID: 7605074, Title: "流浪地球", Original: "The Wandering Earth", Year: "2019", Kind: tmdb.KindMovie},
		{ID: 5491994, Title: "地球脉动 II", Original: "Planet Earth II", Year: "2016", Kind: tmdb.KindTV},
	}
	got, ok := Pick(Parsed{Title: "流浪地球2", Year: "2023", Kind: KindMovie}, res)
	if !ok || got.ID != 13539646 {
		t.Fatalf("应选中 2023 那部，实际 %+v ok=%v", got, ok)
	}
}

func TestRuntimeMinutes(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"166 min", "166"},
		{"166", "166"},
		{"2h 46min", "166"},
		{"2h46m", "166"},
		{"", ""},
		{"unknown", ""},
		{"2 h 46 min", "166"},
	} {
		if got := runtimeMinutes(c.in); got != c.want {
			t.Errorf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestBuildMovieNFO(t *testing.T) {
	data, err := BuildMovieNFO(Meta{
		IMDbID: "tt15239678", TMDbID: 693134, Title: "Dune: Part Two", Year: "2024",
		Rating: "8.0", Runtime: "166 min",
		Genres:   []string{"Action", "Adventure"},
		Cast:     []string{"Timothée Chalamet", "Zendaya"},
		Director: []string{"Denis Villeneuve"},
		Overview: "Paul Atreides unites with Chani...",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<movie>", "<title>Dune: Part Two</title>", "<year>2024</year>",
		"<rating>8.0</rating>", "<runtime>166</runtime>",
		`<uniqueid type="imdb" default="true">tt15239678</uniqueid>`,
		`<uniqueid type="tmdb">693134</uniqueid>`,
		"<genre>Action</genre>", "<actor>", "<name>Zendaya</name>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("nfo 缺少 %q\n%s", want, s)
		}
	}
}

// TestBuildMovieNFO_PrefersChineseTitle 中文名进 <title>，原名进 <originaltitle>。
func TestBuildMovieNFO_PrefersChineseTitle(t *testing.T) {
	data, err := BuildMovieNFO(Meta{
		IMDbID: "tt38775487", TMDbID: 999, Title: "Kong Qiang", TitleCN: "空枪",
		Year: "2026", Overview: "一部中文简介。",
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<title>空枪</title>",
		"<originaltitle>Kong Qiang</originaltitle>",
		"<sorttitle>空枪</sorttitle>",
		"<plot>一部中文简介。</plot>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("nfo 缺少 %q\n%s", want, s)
		}
	}
}

// TestBuildMovieNFO_WithoutIMDbID TMDB 没登记 external_ids 时，
// tmdb ID 要顶上当 default，不能写出一个空的 imdb uniqueid。
func TestBuildMovieNFO_WithoutIMDbID(t *testing.T) {
	data, err := BuildMovieNFO(Meta{TMDbID: 777, Title: "无 IMDb 的片"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	if !strings.Contains(s, `<uniqueid type="tmdb" default="true">777</uniqueid>`) {
		t.Fatalf("tmdb 应成为默认 uniqueid:\n%s", s)
	}
	if strings.Contains(s, `type="imdb"`) {
		t.Fatalf("不该写出空的 imdb uniqueid:\n%s", s)
	}
}

// TestBuildTVShowNFO 剧集根目录必须有 tvshow.nfo。
func TestBuildTVShowNFO(t *testing.T) {
	data, err := BuildTVShowNFO(Meta{
		IMDbID: "tt105248", TMDbID: 1234, Title: "Cyberpunk: Edgerunners", TitleCN: "赛博朋克：边缘行者",
		Year: "2022", Overview: "简介", Genres: []string{"动画", "科幻"},
	})
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{
		"<tvshow>", "<title>赛博朋克：边缘行者</title>", "<year>2022</year>",
		`<uniqueid type="imdb" default="true">tt105248</uniqueid>`,
		"<genre>动画</genre>",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("tvshow.nfo 缺少 %q\n%s", want, s)
		}
	}
}

func TestBuildEpisodeNFO(t *testing.T) {
	data, err := BuildEpisodeNFO(Meta{IMDbID: "tt0903747", Title: "Pilot"}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	s := string(data)
	for _, want := range []string{"<episodedetails>", "<season>1</season>", "<episode>1</episode>"} {
		if !strings.Contains(s, want) {
			t.Errorf("nfo 缺少 %q\n%s", want, s)
		}
	}
}

// TestNFOTitleIsEscaped 片名里的 & < > 必须转义，否则 Emby 直接解析失败。
func TestNFOTitleIsEscaped(t *testing.T) {
	data, err := BuildMovieNFO(Meta{IMDbID: "tt1", Title: `Fast & Furious <9>`})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Fast &amp; Furious &lt;9&gt;") {
		t.Fatalf("标题没转义：%s", data)
	}
}

func TestShowRoot(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"剧集/Breaking Bad/Season 01/Breaking.Bad.S01E01.strm", "剧集/Breaking Bad"},
		{"剧集/Breaking Bad/Breaking.Bad.S01E01.strm", "剧集/Breaking Bad"},
		{"电影/Dune.2024.strm", "电影"},
	} {
		if got := showRoot(c.in); got != c.want {
			t.Errorf("%s → %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestBuildEpisodeNFO_UsesEpisodeTitle 单集 nfo 必须用「这一集」的标题和简介。
func TestBuildEpisodeNFO_UsesEpisodeTitle(t *testing.T) {
	m := Meta{
		IMDbID:   "tt12590266",
		Title:    "Cyberpunk: Edgerunners",
		Rating:   "8.3",
		Overview: "剧集总简介",
		Episodes: []Episode{
			{Season: 1, Episode: 1, Title: "Let You Down", Overview: "单集简介一", Aired: "2022-09-13T07:00:00.000Z"},
			{Season: 1, Episode: 2, Title: "Like a Boy", Overview: "单集简介二", Aired: "2022-09-13T07:00:00.000Z"},
		},
		EpisodesFetched: true,
	}

	raw, err := BuildEpisodeNFO(m, 1, 2)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	for _, want := range []string{
		"<title>Like a Boy</title>",
		"<plot>单集简介二</plot>",
		"<aired>2022-09-13</aired>",
		"<season>1</season>",
		"<episode>2</episode>",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("nfo 缺少 %q\n实际内容:\n%s", want, got)
		}
	}
	if strings.Contains(got, "剧集总简介") {
		t.Fatalf("有单集简介时不该再用剧级简介:\n%s", got)
	}
}

// TestBuildEpisodeNFO_FallsBackToShow 没有单集清单时退回剧级信息，不能写出空标题。
func TestBuildEpisodeNFO_FallsBackToShow(t *testing.T) {
	m := Meta{IMDbID: "tt1", Title: "某剧", Overview: "剧集总简介"}
	raw, err := BuildEpisodeNFO(m, 3, 7)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "<title>某剧</title>") || !strings.Contains(got, "<plot>剧集总简介</plot>") {
		t.Fatalf("没有单集信息时应退回剧级信息:\n%s", got)
	}
	if strings.Contains(got, "<aired>") {
		t.Fatalf("没有播出日期时不该写 aired 标签:\n%s", got)
	}
}

// TestAiredDate 只接受规规矩矩的 YYYY-MM-DD，别把奇怪的值塞进 nfo。
func TestAiredDate(t *testing.T) {
	cases := map[string]string{
		"2022-09-13T07:00:00.000Z": "2022-09-13",
		"2022-09-13":               "2022-09-13",
		"":                         "",
		"2022/09/13":               "",
		"未知":                       "",
		"2022-09":                  "",
	}
	for in, want := range cases {
		if got := airedDate(in); got != want {
			t.Fatalf("airedDate(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 权限类错误要给出可照抄的 chown 命令：这类失败在部署里最常见，
// 只报 "permission denied" 用户根本不知道该改哪里。
func TestWriteHintForPermission(t *testing.T) {
	hint := writeHint(&fs.PathError{Op: "open", Path: "/media/x.nfo.tmp", Err: fs.ErrPermission}, "/media")
	if !strings.Contains(hint, "chown -R") || !strings.Contains(hint, "/media") {
		t.Fatalf("权限错误应给出 chown 提示，实际 %q", hint)
	}
	if got := writeHint(errors.New("disk full"), "/media"); got != "" {
		t.Fatalf("非权限错误不该有提示，实际 %q", got)
	}
}
