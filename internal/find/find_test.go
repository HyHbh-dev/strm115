package find

import (
	"context"
	"errors"
	"strings"
	"testing"

	"strm115/internal/linkparse"
	"strm115/internal/watchhub"
)

// TestNameFromLinks 磁力链里的 dn= 是整段文本里最像片名的东西，得能捞干净。
func TestNameFromLinks(t *testing.T) {
	links := []linkparse.MatchedLink{
		{Type: linkparse.LinkTypeMagnet,
			Value: "magnet:?xt=urn:btih:abcdefabcdefabcdefabcdefabcdefabcdefabcd&dn=Dune.Part.Two.2024.2160p.WEB-DL.x265&tr=udp://t"},
	}
	got := nameFromLinks(links)
	if !strings.Contains(got, "Dune") {
		t.Fatalf("没认出片名: %q", got)
	}
	for _, junk := range []string{"2160p", "x265", "WEB-DL", ".", "magnet"} {
		if strings.Contains(got, junk) {
			t.Errorf("片名里还留着 %q: %q", junk, got)
		}
	}
}

// TestNameFromLinksNoDN 没有 dn= 时不能瞎猜。
func TestNameFromLinksNoDN(t *testing.T) {
	links := []linkparse.MatchedLink{
		{Type: linkparse.LinkTypeMagnet, Value: "magnet:?xt=urn:btih:abcdefabcdefabcdefabcdefabcdefabcdefabcd"},
		{Type: linkparse.LinkTypeED2K, Value: "ed2k://|file|Some.Movie.2020.mkv|1|hash|/"},
	}
	if got := nameFromLinks(links); got != "" {
		t.Fatalf("不该硬造片名: %q", got)
	}
}

func TestStripYear(t *testing.T) {
	cases := map[string]string{
		"Dune Part Two 2024":   "Dune Part Two",
		"Dune 1999":            "Dune",
		"Dune":                 "Dune",
		"2001 A Space Odyssey": "2001 A Space Odyssey", // 年份在中间，不动
	}
	for in, want := range cases {
		if got := stripYear(in); got != want {
			t.Errorf("stripYear(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestStremioType(t *testing.T) {
	cases := map[string]string{
		"movie": "movie", "Movie": "movie", "feature": "movie",
		"series": "series", "tvSeries": "series", "tv series": "series",
		"episode": "series",
		"person":  "",
		"":        "",
	}
	for in, want := range cases {
		if got := stremioType(in); got != want {
			t.Errorf("stremioType(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// TestAnalyzeLinkWithIMDbID 文本里带了 IMDb 链接时，ID 必须传到「在哪看」那一步。
// 这条守的是一个真实踩过的 bug：链接解析出了 ID，但没赋回结果，
// 于是明明能查详情却什么都不显示。
func TestAnalyzeLinkWithIMDbID(t *testing.T) {
	var gotID, gotType string
	old := watchDetail
	watchDetail = func(ctx context.Context, typ, id string) (*watchhub.Item, error) {
		gotID, gotType = id, typ
		return &watchhub.Item{IMDbID: id, Name: "沙丘", Year: "2021", Poster: "https://p"}, nil
	}
	defer func() { watchDetail = old }()

	res, err := Analyze(context.Background(), "https://www.imdb.com/title/tt1160419/ 2160p", Options{})
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !res.HasLinks || len(res.Links) != 1 {
		t.Fatalf("应该认出 1 条链接: %+v", res.Links)
	}
	if res.IMDbID != "tt1160419" {
		t.Fatalf("IMDb ID 没带出来: %q", res.IMDbID)
	}
	if gotID != "tt1160419" {
		t.Fatalf("ID 没传给 WatchHub，实际 %q", gotID)
	}
	if gotType != "" {
		t.Fatalf("没搜过 IMDb 时不该猜类型，实际 %q", gotType)
	}
	if res.Watch == nil || res.Watch.Name != "沙丘" {
		t.Fatalf("观看信息没合并进结果: %+v", res.Watch)
	}
}

// TestAnalyzeWatchFailureIsSoft WatchHub 挂了也要把链接还给用户。
func TestAnalyzeWatchFailureIsSoft(t *testing.T) {
	old := watchDetail
	watchDetail = func(context.Context, string, string) (*watchhub.Item, error) {
		return nil, errors.New("上游 502")
	}
	defer func() { watchDetail = old }()

	res, err := Analyze(context.Background(), "tt1160419", Options{})
	if err != nil {
		t.Fatalf("不该因为观看信息失败而报错: %v", err)
	}
	if len(res.Links) != 1 || res.IMDbID != "tt1160419" {
		t.Fatalf("链接和 ID 应该还在: %+v", res)
	}
	if len(res.Notes) == 0 {
		t.Fatal("应该说明一句为什么没有观看信息")
	}
}

// TestAnalyzeEmpty 空输入直接返回 ErrNoInput，不发任何外部请求。
func TestAnalyzeEmpty(t *testing.T) {
	_, err := Analyze(context.Background(), "   ", Options{})
	if !errors.Is(err, ErrNoInput) {
		t.Fatalf("空输入应返回 ErrNoInput，实际 %v", err)
	}
}
