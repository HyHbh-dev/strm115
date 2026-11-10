package scrape

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		rel    string
		title  string
		year   string
		kind   string
		season int
		ep     int
	}{
		// —— 电影：最常见的那几种落盘姿势 ——
		{"电影/Dune.Part.Two.2024.2160p.WEB-DL.H265.mkv", "Dune Part Two", "2024", KindMovie, 0, 0},
		{"电影/沙丘2 (2024)/Dune.Part.Two.2024.2160p.mkv", "沙丘2", "2024", KindMovie, 0, 0},
		{"电影/沙丘2 (2024).mkv", "沙丘2", "2024", KindMovie, 0, 0},
		{"电影/The.Matrix.1999.1080p.BluRay.x264-GROUP.mkv", "The Matrix", "1999", KindMovie, 0, 0},
		{"Movies/Spider-Man.No.Way.Home.2021.1080p.mkv", "Spider-Man No Way Home", "2021", KindMovie, 0, 0},
		{"[压制组] 流浪地球2 2023 国语中字 2160p.mkv", "流浪地球2", "2023", KindMovie, 0, 0},
		{"电影/1917.2019.2160p.mkv", "1917", "2019", KindMovie, 0, 0},
		// 中文发布名的广告位：全角括号 + 站点域名 + 结尾版本孤字。
		// 不清掉的话，整理出来的目录就叫
		// 「【高清影视之家发布 www BBQDDQ com】空枪 V (2026)」，Emby 里也没法搜。
		{"电影/【高清影视之家发布 www.BBQDDQ.com】空枪[杜比视界版本][国粤多音轨+中文字幕].V.2026.2160p.HDR.mkv",
			"空枪", "2026", KindMovie, 0, 0},
		{"电影/空枪 www.BBEBBB.com 2026.2160p.mkv", "空枪", "2026", KindMovie, 0, 0},
		{"电影/Mission.Impossible.The.Final.Reckoning.2025.Hybrid.2160p.WEB-DL.mkv",
			"Mission Impossible The Final Reckoning", "2025", KindMovie, 0, 0},

		// —— 剧集：SxxExx / 1x02 / 第N集 ——
		{"剧集/Breaking.Bad/Season 01/Breaking.Bad.S01E01.1080p.mkv", "Breaking Bad", "", KindSeries, 1, 1},
		{"剧集/Breaking Bad (2008)/Season 1/Breaking.Bad.S01E07.mkv", "Breaking Bad", "2008", KindSeries, 1, 7},
		{"剧集/切尔诺贝利/Chernobyl.2019.1x03.mkv", "切尔诺贝利", "2019", KindSeries, 1, 3},
		{"剧集/某剧/第1季/第08集.mkv", "某剧", "", KindSeries, 1, 8},
		// 汉字季集号（第一季 / 第八集 / 第二十三集）
		{"剧集/某剧/第一季/第八集.mkv", "某剧", "", KindSeries, 1, 8},
		{"剧集/最后生还者 第一季/The.Last.of.Us.第十集.mkv", "最后生还者", "", KindSeries, 1, 10},
		{"剧集/繁花.第二季/繁花.第二季.第二十三集.mkv", "繁花", "", KindSeries, 2, 23},
		{"剧集/The.Last.of.Us/Season 2/S02E03.1080p.mkv", "The Last of Us", "", KindSeries, 2, 3},
		// 英文长写法「Episode 199」：集号认出来，而且不能留在片名里 ——
		// 留着的话整理时拿「…Immortality Episode 199」去 TMDB 搜，永远搜不到，
		// 文件就一直躺在待整理目录里（真实踩过的例子）。
		{"[FSP DN] A Record of a Mortal’s Journey to Immortality Episode 199 1080p HEVC AAC.mkv",
			"A Record of a Mortal’s Journey to Immortality", "", KindSeries, 1, 199},
		// 点分隔的同一种写法
		{"剧集/Show.Episode.05.1080p.mkv", "Show", "", KindSeries, 1, 5},

		// —— 认不出来的：宁可留空 ——
		{"电影/movie.mkv", "", "", KindMovie, 0, 0},
		{"电影/1080p.mkv", "", "", KindMovie, 0, 0},
	}

	for _, c := range cases {
		got := Parse(c.rel)
		if got.Title != c.title {
			t.Errorf("%s\n  片名 期望 %q 实际 %q（hint=%s）", c.rel, c.title, got.Title, got.Hint)
		}
		if got.Year != c.year {
			t.Errorf("%s\n  年份 期望 %q 实际 %q", c.rel, c.year, got.Year)
		}
		if got.Kind != c.kind {
			t.Errorf("%s\n  类型 期望 %q 实际 %q", c.rel, c.kind, got.Kind)
		}
		if got.Season != c.season || got.Episode != c.ep {
			t.Errorf("%s\n  季集 期望 S%dE%d 实际 S%dE%d", c.rel, c.season, c.ep, got.Season, got.Episode)
		}
	}
}

// TestParseKeepsNamePieces 片名里的连字符/数字不能被当成发布组或集号切掉。
func TestParseKeepsNamePieces(t *testing.T) {
	for _, c := range []struct {
		rel   string
		title string
	}{
		{"电影/Spider-Man.mkv", "Spider-Man"},
		{"电影/Mission.Impossible.2018.mkv", "Mission Impossible"},
		{"电影/Fast.and.Furious.9.2021.mkv", "Fast and Furious 9"},
	} {
		if got := Parse(c.rel); got.Title != c.title {
			t.Errorf("%s 期望 %q 实际 %q", c.rel, c.title, got.Title)
		}
	}
}

func TestNormalizeTitle(t *testing.T) {
	for _, c := range []struct{ a, b string }{
		{"Dune: Part Two", "dune part two"},
		{"Dune.Part.Two", "Dune Part Two"},
		{"流浪地球 2", "流浪地球2"},
	} {
		if normalizeTitle(c.a) != normalizeTitle(c.b) {
			t.Errorf("%q 与 %q 归一化后不等: %q vs %q",
				c.a, c.b, normalizeTitle(c.a), normalizeTitle(c.b))
		}
	}
	if normalizeTitle("Dune") == normalizeTitle("Dune Part Two") {
		t.Error("不同片名不该归一化成同一个")
	}
}

// TestParseIMDbID 文件名/目录里的 IMDb ID 要能认出来，并且不能混进片名。
func TestParseIMDbID(t *testing.T) {
	cases := []struct {
		rel   string
		id    string
		title string
	}{
		{"电影/沙丘2 (2024) {imdb-tt15239678}/Dune.Part.Two.2024.mkv", "tt15239678", "沙丘2"},
		{"电影/Dune.Part.Two.2024.tt15239678.2160p.mkv", "tt15239678", "Dune Part Two"},
		{"电影/[tt15239678] Dune Part Two 2024.mkv", "tt15239678", "Dune Part Two"},
		{"电影/Dune.Part.Two.2024.mkv", "", "Dune Part Two"},
	}
	for _, c := range cases {
		got := Parse(c.rel)
		if got.IMDbID != c.id {
			t.Errorf("%s\n  IMDb ID 期望 %q 实际 %q", c.rel, c.id, got.IMDbID)
		}
		if got.Title != c.title {
			t.Errorf("%s\n  片名 期望 %q 实际 %q", c.rel, c.title, got.Title)
		}
	}
}

func TestNormalizeIMDbID(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"tt15239678", "tt15239678"},
		{" TT15239678 ", "tt15239678"},
		{"https://www.imdb.com/title/tt15239678/", "tt15239678"},
		{"imdb.com/title/tt0903747/?ref_=fn", "tt0903747"},
		{"15239678", ""},
		{"", ""},
	} {
		if got := NormalizeIMDbID(c.raw); got != c.want {
			t.Errorf("%q 期望 %q 实际 %q", c.raw, c.want, got)
		}
	}
}

// 汉字季集号的解析：只覆盖到 99，够用且好验证。
func TestNum(t *testing.T) {
	cases := map[string]int{
		"1": 1, "12": 12, "1080": 1080,
		"一": 1, "两": 2, "九": 9, "十": 10,
		"十一": 11, "十九": 19, "二十": 20, "二十三": 23, "九十": 90,
		"": 0, "某剧": 0, "第十": 0,
	}
	for in, want := range cases {
		if got := num(in); got != want {
			t.Errorf("num(%q) = %d, 期望 %d", in, got, want)
		}
	}
}
