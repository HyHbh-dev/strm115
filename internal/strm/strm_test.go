package strm

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContent(t *testing.T) {
	cases := []struct {
		name    string
		domain  string
		playID  string
		keepExt bool
		file    string
		want    string
	}{
		{"带扩展名", "http://nas:6086", "abc123", true, "流浪地球 (2019).mkv",
			"http://nas:6086/d/abc123.mkv?/流浪地球 (2019).mkv"},
		{"不带扩展名", "http://nas:6086/", "abc123", false, "A.mkv", "http://nas:6086/d/abc123?/A.mkv"},
		{"域名尾部斜杠与扩展名统一小写", "http://nas:6086///", "abc", true, "a.MKV", "http://nas:6086/d/abc.mkv?/a.MKV"},
		{"井号必须转义", "http://h:6086", "pc", true, "剧集#A.mkv", "http://h:6086/d/pc.mkv?/剧集%23A.mkv"},
		{"ISO 无扩展名文件名", "http://h:6086", "pc", true, "光盘.iso", "http://h:6086/d/pc.iso?/光盘.iso"},
		{"空文件名只给直链", "http://h:6086", "pc", false, "", "http://h:6086/d/pc"},
	}
	for _, c := range cases {
		if got := Content(c.domain, c.playID, c.keepExt, c.file); got != c.want {
			t.Errorf("%s: Content() = %q, want %q", c.name, got, c.want)
		}
	}
}

func TestTarget(t *testing.T) {
	cases := []struct{ root, rel, file, want string }{
		{"/media", "", "A (2020).mkv", "/media/A (2020).strm"},
		{"/media", "电影/华语", "B.mkv", "/media/电影/华语/B.strm"},
		{"/media", "", "C.iso", "/media/C.iso.strm"}, // 光盘镜像保留双扩展名
		{"/media", "", "D:坏/名字?.mp4", "/media/D_坏_名字_.strm"},
		{"/media", "../../etc", "E.mkv", "/media/etc/E.strm"}, // 穿越被压平，出不了媒体根
	}
	for _, c := range cases {
		got := filepath.ToSlash(Target(c.root, c.rel, c.file))
		if got != c.want {
			t.Errorf("Target(%q,%q,%q) = %q, want %q", c.root, c.rel, c.file, got, c.want)
		}
	}
}

func TestCleanRelNoTraversal(t *testing.T) {
	got := CleanRel("../../a/./b/../c")
	if got != "a/c" {
		t.Fatalf("CleanRel = %q, want %q", got, "a/c")
	}
}

func TestSanitize(t *testing.T) {
	if got := Sanitize(`a<b>c:d"e|f?g*h`); got != "a_b_c_d_e_f_g_h" {
		t.Fatalf("Sanitize = %q", got)
	}
	if got := Sanitize("正常 名字  "); got != "正常 名字" {
		t.Fatalf("尾部空格没清: %q", got)
	}
}

func TestIsVideo(t *testing.T) {
	yes := []string{"a.mkv", "b.MP4", "c.ts", "d.iso", "e.rmvb"}
	no := []string{"a.nfo", "b.jpg", "c.srt", "d"}
	for _, n := range yes {
		if !IsVideo(n) {
			t.Errorf("%s 应该是视频", n)
		}
	}
	for _, n := range no {
		if IsVideo(n) {
			t.Errorf("%s 不该是视频", n)
		}
	}
}

func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "x.strm")
	if err := Write(p, "http://a/d/1"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "http://a/d/1" {
		t.Fatalf("读回失败: %v %q", err, b)
	}
	// 不允许留下 .tmp
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf(".tmp 残留")
	}
}

// 一个库挂两个网盘之后，.strm 的地址要能区分那是谁的文件。
func TestPlayIDForKind(t *testing.T) {
	cases := []struct{ kind, id, want string }{
		{"115", "bf4l2rr2j5vfa8s5l", "bf4l2rr2j5vfa8s5l"}, // 115 不加前缀，历史 .strm 不受影响
		{"quarktv", "40fdec135a084516ac8d66ea19dddbb9", "q/40fdec135a084516ac8d66ea19dddbb9"},
		{"", "abc", "abc"},
	}
	for _, c := range cases {
		if got := PlayIDForKind(c.kind, c.id); got != c.want {
			t.Errorf("PlayIDForKind(%q,%q) = %q, want %q", c.kind, c.id, got, c.want)
		}
	}
}

func TestSplitPlayKind(t *testing.T) {
	kind, id := SplitPlayKind("q/40fdec135a084516ac8d66ea19dddbb9")
	if kind != "quarktv" || id != "40fdec135a084516ac8d66ea19dddbb9" {
		t.Errorf("夸克标识解析错: %q %q", kind, id)
	}
	kind, id = SplitPlayKind("bf4l2rr2j5vfa8s5l")
	if kind != "" || id != "bf4l2rr2j5vfa8s5l" {
		t.Errorf("老格式应原样返回: %q %q", kind, id)
	}
}

// TestIsAd 广告视频不进媒体库；带发布组前缀的正片不能被误杀。
func TestIsAd(t *testing.T) {
	ads := []string{
		"【更多无水印高品质资源请访问 www.Butailing.com】【更多无水印高品质资源请访问 www.Butailing.com】.mkv",
		"广告.mp4",
		"片头广告.mkv",
		"关注公众号影视大全.mp4",
		"更多无水印资源请访问.avi",
		"https://pan.example.com/s/abcd.mp4",
	}
	for _, n := range ads {
		if !IsAd(n) {
			t.Errorf("应判为广告: %q", n)
		}
	}
	films := []string{
		"【高清影视之家发布 www.BBEGGE.com】极速：伪装者[简繁英字幕].Drive.The.Pretenders.2026.2160p.mkv",
		"广告狂人.S01E01.1080p.WEB-DL.mkv",
		"Dune.Part.Two.2024.2160p.WEB-DL.mkv",
		"空枪.V.2026.WEB-DL.DV.2160p.H265.DDP.5.1.mkv",
	}
	for _, n := range films {
		if IsAd(n) {
			t.Errorf("正片被误判成广告: %q", n)
		}
	}
}
