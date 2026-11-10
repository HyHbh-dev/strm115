package linkparse

import (
	"strings"
	"testing"
)

const (
	hash40 = "a1b2c3d4e5f60718293a4b5c6d7e8f90a1b2c3d4" // 40 位十六进制
	hash32 = "abcdefghijklmnopqrstuvwxyz234567"
)

func containsType(links []MatchedLink, t LinkType) bool {
	for _, l := range links {
		if l.Type == t {
			return true
		}
	}
	return false
}

func TestParseMagnet(t *testing.T) {
	input := "资源: magnet:?xt=urn:btih:" + hash40 + "&dn=Inception.2010.1080p.BluRay.x264&tr=udp://tracker:80"
	got := Parse(input)
	if len(got.FoundLinks) != 1 {
		t.Fatalf("期望 1 个链接，实际 %d: %+v", len(got.FoundLinks), got.FoundLinks)
	}
	if got.FoundLinks[0].Type != LinkTypeMagnet {
		t.Errorf("类型错误: %s", got.FoundLinks[0].Type)
	}
	if !strings.Contains(got.FoundLinks[0].Value, "dn=Inception") {
		t.Errorf("磁力链接参数被截断: %s", got.FoundLinks[0].Value)
	}
	if got.MediaName != "" {
		t.Errorf("命中链接时不应提取 MediaName，实际 %q", got.MediaName)
	}
}

func TestParseMagnetBase32(t *testing.T) {
	input := "magnet:?xt=urn:btih:" + hash32 + "&dn=test"
	got := Parse(input)
	if !containsType(got.FoundLinks, LinkTypeMagnet) {
		t.Fatalf("32 位 Base32 磁力 hash 未匹配: %+v", got.FoundLinks)
	}
}

func TestParseED2K(t *testing.T) {
	input := "ed2k://|file|Movie.2010.avi|734003200|a1b2c3d4e5f60718293a4b5c6d7e8f90|/"
	got := Parse(input)
	if !containsType(got.FoundLinks, LinkTypeED2K) {
		t.Fatalf("ed2k 未匹配: %+v", got.FoundLinks)
	}
}

func TestParseTorrentURL(t *testing.T) {
	input := "种子地址 https://example.com/dl/movie.torrent?passkey=abc123 请查收"
	got := Parse(input)
	if !containsType(got.FoundLinks, LinkTypeTorrent) {
		t.Fatalf(".torrent 链接未匹配: %+v", got.FoundLinks)
	}
}

func TestParseBareInfoHash(t *testing.T) {
	got := Parse("btih: " + hash40)
	if !containsType(got.FoundLinks, LinkTypeInfoHash) {
		t.Fatalf("独立 Info Hash 未匹配: %+v", got.FoundLinks)
	}
}

func TestInfoHashInsideMagnetNotDuplicated(t *testing.T) {
	got := Parse("magnet:?xt=urn:btih:" + hash40 + "&dn=test")
	count := 0
	for _, l := range got.FoundLinks {
		if l.Value == hash40 && l.Type == LinkTypeInfoHash {
			count++
		}
	}
	if count != 0 {
		t.Errorf("magnet 内的 hash 被重复计为独立 Info Hash: %+v", got.FoundLinks)
	}
	if len(got.FoundLinks) != 1 {
		t.Errorf("期望仅 1 个链接，实际 %d: %+v", len(got.FoundLinks), got.FoundLinks)
	}
}

func TestParseIMDbTitlePage(t *testing.T) {
	got := Parse("详情见 https://www.imdb.com/title/tt1375666/ 谢谢")
	if !containsType(got.FoundLinks, LinkTypeIMDbPage) {
		t.Fatalf("IMDb 标题页未匹配: %+v", got.FoundLinks)
	}
	if got.ExtractID() != "tt1375666" {
		t.Errorf("IMDb ID 提取错误: %s", got.ExtractID())
	}
	// 裸 ID 与标题页不应重复
	pages := 0
	for _, l := range got.FoundLinks {
		if l.Type == LinkTypeIMDbPage {
			pages++
		}
	}
	if pages != 1 {
		t.Errorf("IMDb 标题页被重复匹配: %+v", got.FoundLinks)
	}
}

func TestParseBareIMDbID(t *testing.T) {
	got := Parse("编号 tt0816692")
	if got.ExtractID() != "tt0816692" {
		t.Fatalf("裸 IMDb ID 未匹配: %+v", got.FoundLinks)
	}
}

func TestParseIMDbAPIEndpoint(t *testing.T) {
	cases := []string{
		"官方接口 https://api.imdb.com/graphql 请用这个",
		"https://api.graphql.imdb.com/",
		"https://imdb-api.com/en/API/SearchTitle/MYKEY/Inception",
	}
	for _, input := range cases {
		got := Parse(input)
		if !containsType(got.FoundLinks, LinkTypeIMDbAPI) {
			t.Errorf("IMDb API 端点未匹配 [%s]: %+v", input, got.FoundLinks)
		}
	}
}

func TestParseIMDbFindQuery(t *testing.T) {
	got := Parse("搜索 https://www.imdb.com/find?q=The%20Matrix&s=tt")
	if !containsType(got.FoundLinks, LinkTypeIMDbPage) {
		t.Fatalf("IMDb find 页未匹配: %+v", got.FoundLinks)
	}
	var term string
	for _, l := range got.FoundLinks {
		if l.SearchTerm != "" {
			term = l.SearchTerm
		}
	}
	if term != "The Matrix" {
		t.Errorf("q 参数解析错误: %q", term)
	}
}

func TestParseNoLinksExtractMediaName(t *testing.T) {
	cases := []struct {
		input string
		want  string
	}{
		{"Inception.2010.1080p.BluRay.x264-GROUP.mkv", "Inception 2010"},
		{"The_Shawshank_Redemption_1994_720p_WEB-DL_x265", "The Shawshank Redemption 1994"},
		{"Interstellar", "Interstellar"},
	}
	for _, c := range cases {
		got := Parse(c.input)
		if len(got.FoundLinks) != 0 {
			t.Errorf("[%s] 不应匹配到链接: %+v", c.input, got.FoundLinks)
		}
		if got.MediaName != c.want {
			t.Errorf("[%s] MediaName = %q, 期望 %q", c.input, got.MediaName, c.want)
		}
	}
}

func TestParseMixedInput(t *testing.T) {
	input := "magnet:?xt=urn:btih:" + hash40 + "&dn=x 参考 https://www.imdb.com/title/tt1375666/ 以及 https://api.imdb.com/graphql"
	got := Parse(input)
	if len(got.FoundLinks) != 3 {
		t.Errorf("期望 3 个链接，实际 %d: %+v", len(got.FoundLinks), got.FoundLinks)
	}
	if !containsType(got.FoundLinks, LinkTypeMagnet) ||
		!containsType(got.FoundLinks, LinkTypeIMDbPage) ||
		!containsType(got.FoundLinks, LinkTypeIMDbAPI) {
		t.Errorf("混合输入类型缺失: %+v", got.FoundLinks)
	}
}

func TestParseDedup(t *testing.T) {
	input := "magnet:?xt=urn:btih:" + hash40 + "&dn=x\n" +
		"magnet:?xt=urn:btih:" + hash40 + "&dn=x"
	got := Parse(input)
	if len(got.FoundLinks) != 1 {
		t.Errorf("重复链接未去重: %+v", got.FoundLinks)
	}
}

func TestHelperFunctions(t *testing.T) {
	if !HasOfflineDownloadLink("magnet:?xt=urn:btih:" + hash40) {
		t.Error("HasOfflineDownloadLink 对磁力链接应返回 true")
	}
	if HasOfflineDownloadLink("Inception 2010 1080p") {
		t.Error("HasOfflineDownloadLink 对纯文本应返回 false")
	}
	if !HasIMDbLink("tt1375666") {
		t.Error("HasIMDbLink 对裸 ID 应返回 true")
	}
	if id := ExtractIMDbID("见 https://www.imdb.com/title/tt0816692/"); id != "tt0816692" {
		t.Errorf("ExtractIMDbID = %q", id)
	}
	if id := ExtractIMDbID("纯文本无 ID"); id != "" {
		t.Errorf("ExtractIMDbID 应为空，实际 %q", id)
	}
}
