package organize

import (
	"encoding/json"
	"strings"
	"testing"
)

// 整理的目标路径错了比不整理更糟：文件被搬进一个 Emby 认不出的目录，
// 用户还以为是刮削坏了。所以每一条落位规则都要有测试盯着。
func TestRecognizeTargets(t *testing.T) {
	cases := []struct {
		name  string
		isDir bool
		want  Item
	}{
		{
			name: "沙丘2.2024.2160p.WEB-DL.H265.mkv",
			want: Item{Kind: KindMovie, Title: "沙丘2", Year: "2024",
				Dir: "沙丘2 (2024)", File: "沙丘2.2024.2160p.WEB-DL.H265.mkv"},
		},
		{
			name: "漫长的季节.2023.S01E01.2160p.mkv",
			want: Item{Kind: KindSeries, Title: "漫长的季节", Year: "2023", Season: 1, Episode: 1,
				Dir: "漫长的季节 (2023)/Season 1", File: "漫长的季节.2023.S01E01.2160p.mkv"},
		},
		{
			// 文件名里没有年份时不能凭空编一个，目录就用片名本身
			name: "The.Last.of.Us.S01E03.1080p.mkv",
			want: Item{Kind: KindSeries, Title: "The Last of Us", Season: 1, Episode: 3,
				Dir: "The Last of Us/Season 1", File: "The.Last.of.Us.S01E03.1080p.mkv"},
		},
		{
			// 整个季目录搬过去，且不能套两层 Season
			name:  "某剧 第1季",
			isDir: true,
			want:  Item{Kind: KindSeries, Title: "某剧", Season: 1, Dir: "某剧"},
		},
		{
			// 汉字季号：中文圈最常见的写法，「第一季」不能留在片名里
			name:  "最后生还者 第一季",
			isDir: true,
			want:  Item{Kind: KindSeries, Title: "最后生还者", Season: 1, Dir: "最后生还者"},
		},
		{
			name:  "繁花.第二季",
			isDir: true,
			want:  Item{Kind: KindSeries, Title: "繁花", Season: 2, Dir: "繁花"},
		},
		{
			// S02 这种写法也要从目录名里剥掉，否则每季建一个独立剧目录
			name:  "某剧.S02.1080p",
			isDir: true,
			want:  Item{Kind: KindSeries, Title: "某剧", Season: 2, Dir: "某剧"},
		},
		{
			name:  "沙丘2.2024.2160p",
			isDir: true,
			want:  Item{Kind: KindMovie, Title: "沙丘2", Year: "2024", Dir: "沙丘2 (2024)"},
		},
	}
	for _, c := range cases {
		it := Recognize(Entry{ID: "x", Name: c.name, IsDir: c.isDir, Size: 5 << 30}, 300)
		if it.Skip != "" {
			t.Errorf("%s: 不该跳过（%s）", c.name, it.Skip)
			continue
		}
		if it.Kind != c.want.Kind || it.Title != c.want.Title || it.Year != c.want.Year ||
			it.Season != c.want.Season || it.Episode != c.want.Episode {
			t.Errorf("%s: 识别结果不对\n got kind=%s title=%q year=%q s=%d e=%d\nwant kind=%s title=%q year=%q s=%d e=%d",
				c.name, it.Kind, it.Title, it.Year, it.Season, it.Episode,
				c.want.Kind, c.want.Title, c.want.Year, c.want.Season, c.want.Episode)
		}
		if it.Dir != c.want.Dir || it.File != c.want.File {
			t.Errorf("%s: 落位不对 got dir=%q file=%q，want dir=%q file=%q",
				c.name, it.Dir, it.File, c.want.Dir, c.want.File)
		}
	}
}

// 该跳过的一条都不能动手：非视频、太小的样片、认不出片名的。
func TestRecognizeSkips(t *testing.T) {
	cases := []struct {
		e    Entry
		want string
	}{
		{Entry{Name: "说明.txt", Size: 1 << 20}, "不是视频"},
		{Entry{Name: "poster.jpg", Size: 1 << 20}, "不是视频"},
		{Entry{Name: "某片.2024.mkv", Size: 10 << 20}, "小于 300 MB"},
		{Entry{Name: "abc.mkv", Size: 5 << 30}, ""}, // 不能被误杀，下面单独看
	}
	for i, c := range cases {
		c.e.ID = "x"
		it := Recognize(c.e, 300)
		if c.want == "" {
			// `abc` 很短但仍是合法片名，不该跳过
			if strings.Contains(it.Skip, "不是视频") || strings.Contains(it.Skip, "小于") {
				t.Errorf("case %d: 不该跳过: %s", i, it.Skip)
			}
			continue
		}
		if !strings.Contains(it.Skip, c.want) {
			t.Errorf("case %d: 应以 %q 跳过，实际 %q", i, c.want, it.Skip)
		}
		if it.Dir != "" {
			t.Errorf("case %d: 跳过的条目不该给出目标目录: %q", i, it.Dir)
		}
	}
}

// 目录名里带 / 会把 115 的层级搞乱，必须替换掉。
func TestSafeName(t *testing.T) {
	it := Recognize(Entry{ID: "x", Name: "A/B.2020.1080p.mkv", Size: 3 << 30}, 0)
	if strings.Contains(it.Dir, "/") && !strings.HasPrefix(it.Dir, "A") {
		t.Fatalf("目标目录不该含路径分隔符: %q", it.Dir)
	}
	if strings.TrimSpace(it.Dir) == "" {
		t.Fatal("目录名不该为空")
	}
	// 超长片名要截断，115 的目录名有长度上限
	long := strings.Repeat("长", 200) + ".2020.mkv"
	if got := Recognize(Entry{ID: "y", Name: long, Size: 3 << 30}, 0); len([]rune(got.Dir)) > 90 {
		t.Fatalf("目录名没截断: %d 字", len([]rune(got.Dir)))
	}
}

func TestConfigReady(t *testing.T) {
	var c Config
	if err := c.Ready(); err == nil || !strings.Contains(err.Error(), "待整理") {
		t.Fatalf("没选源目录要报错: %v", err)
	}
	c.SourceCID = "123"
	if err := c.Ready(); err == nil || !strings.Contains(err.Error(), "媒体库") {
		t.Fatalf("没选目标库要报错: %v", err)
	}
	c.MovieLibID = 1
	if err := c.Ready(); err != nil {
		t.Fatalf("配齐了不该报错: %v", err)
	}
	// 默认值补齐
	if f := (Config{}).Filled(); f.Batch != 30 || f.MinSizeMB < 0 {
		t.Fatalf("默认值不对: %+v", f)
	}
	// 一次几百个移动必被 115 限流，上限必须夹住
	if f := (Config{Batch: 999}).Filled(); f.Batch != 200 {
		t.Fatalf("batch 没夹到 200: %d", f.Batch)
	}
}

// 前端直接吃这份 JSON：字段名一旦退回 Go 的默认命名（ID / Name / IsDir），
// 扫描结果在界面上就是一排 undefined。这条测试守的就是这个契约。
func TestItemJSONKeys(t *testing.T) {
	it := Recognize(Entry{ID: "f1", Name: "沙丘2.2024.2160p.mkv", Size: 5 << 30}, 0)
	b, err := json.Marshal(it)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"file_id", "name", "is_dir", "size", "kind", "title", "year", "confidence", "reason", "dir", "target"} {
		if _, ok := m[k]; !ok {
			t.Errorf("缺字段 %q: %s", k, b)
		}
	}
}

// 目录体积门槛：真机现场是「空枪 (2026)」这个目录，递归体积 636 KB，
// 里面只有一个发布组的广告壳。以前只对文件做体积过滤，它就被当成一部
// 电影搬进了媒体库，在 Emby 里变成一张没海报的灰卡片。
func TestRecognizeDirSizeGate(t *testing.T) {
	ad := Entry{ID: "d1", Name: "空枪 (2026)", IsDir: true, Size: 636976}
	if it := Recognize(ad, 300); it.Skip == "" {
		t.Fatalf("636 KB 的目录应被体积门槛拦下，实际 %+v", it)
	}
	// 网盘没给体积（Size==0）时不能当成「太小」：一整季显示 0 B 的目录
	// 被误杀比漏掉一个广告壳严重得多。
	unknown := Entry{ID: "d2", Name: "漫长的季节 (2023)", IsDir: true, Size: 0}
	if it := Recognize(unknown, 300); it.Skip != "" {
		t.Fatalf("体积未知的目录不该被拦，实际 %+v", it)
	}
}

// 动漫没有独立的库：番剧在 TMDB 里就是 tv，和剧集落在一起。
// 名字里带番剧标记、又没有 S/E 的目录（「某番 合集」）也要认成剧集，
// 否则它会被当成电影搬进电影库。
func TestRecognizeAnimeIsSeries(t *testing.T) {
	cases := []struct{ name string }{
		{"赛博朋克：边缘行者 日番 合集"},
		{"葬送的芙莉莲 新番 1080p"},
		{"【喵萌奶茶屋】某番 OVA"},
	}
	for _, c := range cases {
		it := Recognize(Entry{ID: "x", Name: c.name, IsDir: true, Size: 4 << 30}, 0)
		if it.Kind != KindSeries {
			t.Errorf("%q 应认成剧集，实际 kind=%s dir=%s", c.name, it.Kind, it.Dir)
		}
		// 分类词不能留在片名里：带着「新番」去 TMDB 搜不到任何东西。
		if strings.Contains(it.Title, "新番") || strings.Contains(it.Title, "日番") || strings.Contains(it.Title, "OVA") {
			t.Errorf("%q 的片名里还留着分类词：%q", c.name, it.Title)
		}
	}
	// 「动画电影」这类片名不能被误判成剧集：进了 Season 目录就再也认不回来了。
	if it := Recognize(Entry{ID: "y", Name: "动画电影 你的名字 2016", IsDir: true, Size: 4 << 30}, 0); it.Kind != KindMovie {
		t.Errorf("动画电影应保持电影，实际 %+v", it)
	}
}

// 老配置（v0.2.8 及以前）只有同名字段，没有 rules 数组：
// 必须原样变成一条规则，否则升级后整理直接停摆。
func TestLegacyConfigBecomesSingleRule(t *testing.T) {
	c := Config{Enabled: true, AccountID: 3, SourceCID: "999", SourceName: "/待整理",
		MovieLibID: 7, TVLibID: 8}.Filled()
	rules := c.ActiveRules()
	if len(rules) != 1 {
		t.Fatalf("老配置应合成一条规则，实际 %d 条：%+v", len(rules), rules)
	}
	r := rules[0]
	if r.SourceCID != "999" || r.MovieLibID != 7 || r.TVLibID != 8 || r.AccountID != 3 {
		t.Fatalf("合成出来的规则不对: %+v", r)
	}
	// 老字段要跟着第一条走：入库后自动整理、重试这些路径还在读它。
	if c.SourceCID != "999" || c.MovieLibID != 7 {
		t.Fatalf("老字段没同步: %+v", c)
	}
}

// 多条规则各自独立：一条是 115 的电影库、一条是夸克的剧集库。
func TestMultipleRules(t *testing.T) {
	off := false
	c := Config{Enabled: true, Rules: []Rule{
		{ID: "a", AccountID: 1, SourceCID: "115dir", MovieLibID: 7},
		{ID: "b", AccountID: 2, SourceCID: "qdir", TVLibID: 8},
		{ID: "c", AccountID: 1, SourceCID: "ignored", MovieLibID: 7, Enabled: &off},
		{ID: "d"}, // 前端加了空行没填，必须被丢掉
	}}.Filled()

	rules := c.ActiveRules()
	if len(rules) != 2 {
		t.Fatalf("应该只有两条启用且完整的规则，实际 %+v", rules)
	}
	if rules[0].ID != "a" || rules[1].ID != "b" {
		t.Errorf("规则顺序变了: %+v", rules)
	}
	if r, ok := c.RuleByID("b"); !ok || r.AccountID != 2 {
		t.Errorf("按 id 取规则失败: %+v %v", r, ok)
	}
}

// 一条都不完整时，错误信息要说清「缺什么」，而不是笼统一句配置不全。
func TestReadyExplainsWhatIsMissing(t *testing.T) {
	c := Config{Rules: []Rule{{ID: "a", SourceCID: "x"}}}.Filled()
	err := c.Ready()
	if err == nil || !strings.Contains(err.Error(), "目标媒体库") {
		t.Fatalf("应该提示缺目标库，实际 %v", err)
	}
}

// 中文发布名的典型写法：全角括号 + 竖线排版 + 尾部画质词。
// 以前会落成「F1：狂丨飙丨飞丨车（ ）4K (2025)」，Emby 里难看、TMDB 也搜不到。
func TestCleanChineseReleaseName(t *testing.T) {
	it := Recognize(Entry{
		ID: "x", IsDir: true, Size: 40 << 30,
		Name: "F1：狂丨飙丨飞丨车（2025）4K REMUX原盘 杜比视界 国英音轨",
	}, 0)
	if it.Year != "2025" {
		t.Fatalf("年份没认出来: %q", it.Year)
	}
	if strings.ContainsAny(it.Dir, "丨（）") {
		t.Errorf("目录名还带着排版符号: %q", it.Dir)
	}
	if strings.Contains(it.Dir, "4K") || strings.Contains(it.Dir, "REMUX") {
		t.Errorf("目录名还带着画质词: %q", it.Dir)
	}
	if !strings.Contains(it.Dir, "狂飙飞车") || !strings.Contains(it.Dir, "2025") {
		t.Errorf("目录名应该保留片名与年份: %q", it.Dir)
	}

	// 半角括号的老写法不能被改坏
	if got := Recognize(Entry{ID: "y", Name: "空枪 (2026).mkv", Size: 5 << 30}, 0); got.Dir != "空枪 (2026)" {
		t.Errorf("半角写法被改坏: %q", got.Dir)
	}
}
