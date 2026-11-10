package subtitle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"strm115/internal/store"
)

func TestCandidateChinese(t *testing.T) {
	for _, c := range []struct {
		lang string
		want bool
	}{
		{"zh", true}, {"zho", true}, {"chi", true}, {"chs", true}, {"cht", true},
		{"zht", true}, {"zhs", true}, {"zh-Hant", true},
		{"zh-CN", true}, {"zh-tw", true}, {"eng", false}, {"jpn", false},
	} {
		if got := (Candidate{Lang: c.lang}).Chinese(); got != c.want {
			t.Errorf("%s → %v，期望 %v", c.lang, got, c.want)
		}
	}
}

func TestCandidateScript(t *testing.T) {
	for _, c := range []struct {
		cand Candidate
		want int
	}{
		{Candidate{Lang: "zh-CN"}, 1},
		{Candidate{Lang: "zho", Name: "Movie.2025.chs.srt"}, 1},
		{Candidate{Lang: "zho", Release: "Movie.2025.简繁英字幕"}, 1},
		{Candidate{Lang: "zht"}, -1},
		{Candidate{Lang: "zho", Release: "Movie.2025.CHT"}, -1},
		{Candidate{Lang: "zho", Name: "Movie.2025.繁体中文.srt"}, -1},
		{Candidate{Lang: "zho"}, 0},
	} {
		if got := c.cand.Script(); got != c.want {
			t.Errorf("%+v → %d，期望 %d", c.cand, got, c.want)
		}
	}
}

// TestRankPrefersSimplified 片源相似度只差一两点时，简体必须排在繁体前面。
//
// 真实案例：《碟中谍8》同一部片挂着 3 条中文字幕，zht 繁体那条以 1 分之差
// （47 vs 46）排到最前，于是整片字幕落盘成了繁体。
func TestRankPrefersSimplified(t *testing.T) {
	video := "碟中谍8：最终清算.Mission： Impossible - The Final Reckoning.2025.WEB-DL.DV HDR.2160p.H265.DDP.5.1.ATMOS.strm"
	list := []Candidate{
		{
			Lang:    "zht",
			Release: "Mission.Impossible.-.The.Final.Reckoning.2025.1080p.WEBRip.x264.AAC5.1-[YTS.MX].mp4",
			URL:     "trad",
		},
		{
			Lang:    "zho",
			Name:    "Mission.Impossible.The.Final.Reckoning.2025.1080p.WEB.h264-ETHEL.zh-CN.srt",
			Release: "Mission.Impossible.The.Final.Reckoning.2025.1080p.WEB.h264-ETHEL.zh-CN",
			URL:     "simp",
		},
	}
	ranked := Rank(list, video)
	if len(ranked) != 2 || ranked[0].URL != "simp" {
		t.Fatalf("简体应排第一，实际 %+v", ranked)
	}
	if ranked[0].Script != 1 || ranked[1].Script != -1 {
		t.Fatalf("字形标记不对：%d / %d", ranked[0].Script, ranked[1].Script)
	}
}

// TestRankKeepsCloserRelease 片源差太多时不抢：时间轴错位比繁体字形更难受。
func TestRankKeepsCloserRelease(t *testing.T) {
	video := "Dune.Part.Two.2024.2160p.WEB-DL.H265.mkv"
	list := []Candidate{
		{Lang: "zht", Release: "Dune.Part.Two.2024.2160p.WEB-DL.H265.CHT", URL: "trad"},
		{Lang: "zh-CN", Release: "Some.Other.Movie.2019.1080p.zh-CN", URL: "simp"},
	}
	ranked := Rank(list, video)
	if len(ranked) != 2 || ranked[0].URL != "trad" {
		t.Fatalf("片源差太多时仍应按相似度选，实际 %+v", ranked)
	}
}

// TestPickPrefersChinese 有中文就一定用中文，哪怕片源版本对不上。
func TestPickPrefersChinese(t *testing.T) {
	list := []Candidate{
		{Lang: "eng", Release: "Dune.Part.Two.2024.2160p.WEB-DL", URL: "u1"},
		{Lang: "zho", Release: "Dune.Part.Two.2024.1080p.BluRay", URL: "u2"},
	}
	c, isZh, ok := Pick(list, "Dune.Part.Two.2024.2160p.WEB-DL.mkv", true)
	if !ok || !isZh || c.URL != "u2" {
		t.Fatalf("应选中文，实际 %+v isZh=%v ok=%v", c, isZh, ok)
	}
}

// TestPickBestRelease 同语种里挑片源最像的，时间轴才对得上。
func TestPickBestRelease(t *testing.T) {
	list := []Candidate{
		{Lang: "zho", Release: "Some.Other.Movie.2019.DVDRip", URL: "u1"},
		{Lang: "zho", Release: "Dune.Part.Two.2024.2160p.WEB-DL.H265", URL: "u2"},
		{Lang: "zho", Release: "Dune.Part.Two.2024.1080p", URL: "u3"},
	}
	c, _, ok := Pick(list, "Dune.Part.Two.2024.2160p.WEB-DL.H265.mkv", true)
	if !ok || c.URL != "u2" {
		t.Fatalf("应选最匹配的那条，实际 %+v", c)
	}
}

func TestPickNoMatch(t *testing.T) {
	list := []Candidate{{Lang: "jpn", URL: "u1"}, {Lang: "kor", URL: "u2"}}
	if _, _, ok := Pick(list, "x.mkv", true); ok {
		t.Fatal("没有中文/英文时不该选出东西")
	}
}

func TestToUTF8(t *testing.T) {
	// UTF-8 BOM 去掉
	if got := string(toUTF8([]byte{0xEF, 0xBB, 0xBF, 'a', 'b'})); got != "ab" {
		t.Errorf("BOM 没去掉: %q", got)
	}
	// UTF-16LE
	src := []byte{0xFF, 0xFE, 'h', 0, 'i', 0}
	if got := string(toUTF8(src)); got != "hi" {
		t.Errorf("UTF-16LE 转错: %q", got)
	}
	// Latin-1
	if got := string(toUTF8([]byte{0xE9})); got != "é" {
		t.Errorf("Latin-1 转错: %q", got)
	}
	// 本来就是 UTF-8
	if got := string(toUTF8([]byte("中文"))); got != "中文" {
		t.Errorf("UTF-8 被改坏: %q", got)
	}
}

func TestFindExistingChinese(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "Movie.2024")
	if got := findExistingChinese(base); got != "" {
		t.Fatalf("空目录不该找到字幕，实际 %q", got)
	}
	os.WriteFile(base+".chs.srt", []byte("x"), 0o644)
	if got := findExistingChinese(base); got != base+".chs.srt" {
		t.Fatalf("应认出 .chs.srt，实际 %q", got)
	}
}

func TestSourceListURL(t *testing.T) {
	s := NewSource("")
	if got := s.listURL("movie", "tt15239678", 0, 0); got !=
		"https://opensubtitles-v3.strem.io/subtitles/movie/tt15239678.json" {
		t.Errorf("电影地址错: %s", got)
	}
	if got := s.listURL("series", "tt0903747", 1, 3); got !=
		"https://opensubtitles-v3.strem.io/subtitles/series/tt0903747:1:3.json" {
		t.Errorf("剧集地址错（必须带季集）: %s", got)
	}
}

// fakeSubs 起一个假的字幕服务：列表 + 下载。
func fakeSubs(t *testing.T, srt string, langs ...string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/subtitles/", func(w http.ResponseWriter, r *http.Request) {
		var subs []map[string]any
		for i, l := range langs {
			subs = append(subs, map[string]any{
				"id": fmt.Sprint(i), "url": srv.URL + "/file/" + l,
				"lang": l, "subtitleFileName": "x.srt",
				"movieReleaseName": "Dune.Part.Two.2024.2160p",
				"SubEncoding":      "UTF-8",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"subtitles": subs})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		io.WriteString(w, srt)
	})
	return srv
}

const sampleSRT = `1
00:00:01,000 --> 00:00:03,000
Hello there.

2
00:00:04,000 --> 00:00:06,000
How are you?
`

func newFetcher(t *testing.T) (*Fetcher, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return NewFetcher(st, slog.New(slog.NewTextHandler(io.Discard, nil))), st
}

// TestFetchChineseDirectly 有中文字幕就直接落盘，文件名必须是 `X.zh.srt`。
func TestFetchChineseDirectly(t *testing.T) {
	srv := fakeSubs(t, sampleSRT, "eng", "zho")
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)

	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "电影"), 0o755)

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "电影/Dune.Part.Two.2024.strm", VideoName: "Dune.Part.Two.2024.2160p.mkv",
		IMDbID: "tt15239678", Kind: "movie",
	}, Options{Translate: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Saved != "电影/Dune.Part.Two.2024.zh.srt" {
		t.Fatalf("落盘路径不对: %s", res.Saved)
	}
	data, err := os.ReadFile(filepath.Join(root, "电影/Dune.Part.Two.2024.zh.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "Hello there.") {
		t.Fatalf("字幕内容不对: %s", data)
	}
}

// TestFetchTranslateWhenNoChinese 只有英文字幕时，翻译成中文再落盘。
func TestFetchTranslateWhenNoChinese(t *testing.T) {
	srv := fakeSubs(t, sampleSRT, "eng")
	api := fakeTranslateAPI(t)

	f, st := newFetcher(t)
	f.Source = NewSource(srv.URL)

	root := t.TempDir()
	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Dune.Part.Two.2024.strm", VideoName: "Dune.Part.Two.2024.2160p.mkv",
		IMDbID: "tt15239678", Kind: "movie",
	}, Options{Translate: true, APIBase: api})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Translated || res.Origin != "translated" {
		t.Fatalf("应走翻译路径，实际 %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "Dune.Part.Two.2024.zh.srt"))
	if !strings.Contains(string(data), "【译】Hello there.") {
		t.Fatalf("字幕没被翻译: %s", data)
	}
	// 时间轴必须原样保留
	if !strings.Contains(string(data), "00:00:01,000 --> 00:00:03,000") {
		t.Fatalf("时间轴被破坏: %s", data)
	}

	// 第二次应当全部命中缓存，不再调翻译接口
	before := translateCalls.Load()
	os.Remove(filepath.Join(root, "Dune.Part.Two.2024.zh.srt"))
	res2, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Dune.Part.Two.2024.strm", VideoName: "Dune.Part.Two.2024.2160p.mkv",
		IMDbID: "tt15239678", Kind: "movie",
	}, Options{Translate: true, APIBase: api, Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Note != "全部命中缓存" {
		t.Fatalf("第二次应命中缓存，实际 %q", res2.Note)
	}
	if got := translateCalls.Load(); got != before {
		t.Fatalf("缓存没生效：翻译接口被调了 %d 次（之前 %d）", got, before)
	}
	_ = st
}

// TestFetchSkipsWhenChineseExists 已经手动放好的中文字幕不能被覆盖。
func TestFetchSkipsWhenChineseExists(t *testing.T) {
	srv := fakeSubs(t, sampleSRT, "zho")
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)

	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "Movie.2024.chs.srt"), []byte("手动放置的字幕"), 0o644)

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Movie.2024.strm", VideoName: "Movie.2024.mkv",
		IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatalf("应跳过，实际 %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "Movie.2024.chs.srt"))
	if string(data) != "手动放置的字幕" {
		t.Fatal("用户手动放置的字幕被改写了")
	}
}

const (
	simplifiedSRT = `1
00:00:01,000 --> 00:00:03,000
我们在阴影下生与死

2
00:00:04,000 --> 00:00:06,000
为了所爱的人
`
	traditionalSRT = `1
00:00:01,000 --> 00:00:03,000
我們在陰影下生與死

2
00:00:04,000 --> 00:00:06,000
為了所愛的人

3
00:00:07,000 --> 00:00:09,000
以及從未謀面的人們
`
)

func TestLooksTraditional(t *testing.T) {
	if looksTraditional([]byte(simplifiedSRT)) {
		t.Error("简体被误判成繁体")
	}
	if !looksTraditional([]byte(traditionalSRT)) {
		t.Error("繁体没认出来")
	}
}

// TestFetchReplacesTraditionalZh 自己写下的 .zh.srt 是繁体时要换成简体。
//
// 这是「第一次补到繁体就永久繁体」的最后一环：整库补字幕看到已有中文字幕
// 就跳过，用户就算知道该换也没有入口。
func TestFetchReplacesTraditionalZh(t *testing.T) {
	srv := fakeSubs(t, simplifiedSRT, "zho")
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)

	root := t.TempDir()
	zhPath := filepath.Join(root, "Movie.2024.zh.srt")
	if err := os.WriteFile(zhPath, []byte(traditionalSRT), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Movie.2024.strm", VideoName: "Movie.2024.mkv",
		IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true})
	if err != nil {
		t.Fatal(err)
	}
	if res.Skipped {
		t.Fatalf("繁体已有字幕应被替换，实际 %+v", res)
	}
	data, err := os.ReadFile(zhPath)
	if err != nil {
		t.Fatal(err)
	}
	if looksTraditional(data) {
		t.Fatalf("换完还是繁体: %s", data)
	}
	if !strings.Contains(string(data), "我们在阴影下生与死") {
		t.Fatalf("字幕内容没换: %s", data)
	}
}

// TestFetchKeepsTraditionalWhenNoSimplified 字幕站只有繁体时，别反复覆盖：
// 保留原文件并标记跳过，否则每轮整库补字幕都要白下一次。
func TestFetchKeepsTraditionalWhenNoSimplified(t *testing.T) {
	srv := fakeSubs(t, traditionalSRT, "zht")
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)

	root := t.TempDir()
	zhPath := filepath.Join(root, "Movie.2024.zh.srt")
	os.WriteFile(zhPath, []byte(traditionalSRT), 0o644)

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Movie.2024.strm", VideoName: "Movie.2024.mkv",
		IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped || !strings.Contains(res.Note, "繁体") {
		t.Fatalf("只有繁体源时应跳过并说明原因，实际 %+v", res)
	}
	data, _ := os.ReadFile(zhPath)
	if string(data) != traditionalSRT {
		t.Fatal("没有简体源时不该动原文件")
	}
}

// TestFetchNoSubtitle 片源没有字幕时返回明确错误，不写空文件。
func TestFetchNoSubtitle(t *testing.T) {
	srv := fakeSubs(t, sampleSRT, "jpn")
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	_, err := f.Fetch(context.Background(), root, Target{
		RelPath: "Movie.strm", VideoName: "Movie.mkv", IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true})
	if err == nil {
		t.Fatal("应该报错")
	}
	if _, statErr := os.Stat(filepath.Join(root, "Movie.zh.srt")); statErr == nil {
		t.Fatal("失败时不该留下字幕文件")
	}
}

// TestFetchBilingual 双语输出：译文在上、原文在下。
func TestFetchBilingual(t *testing.T) {
	srv := fakeSubs(t, sampleSRT, "eng")
	api := fakeTranslateAPI(t)
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	_, err := f.Fetch(context.Background(), root, Target{
		RelPath: "M.strm", VideoName: "M.mkv", IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true, Bilingual: true, APIBase: api, Overwrite: true})
	if err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(root, "M.zh.srt"))
	s := string(data)
	if !strings.Contains(s, "【译】Hello there.\nHello there.") {
		t.Fatalf("双语格式不对:\n%s", s)
	}
}

// ==================== 假的翻译接口 ====================

var translateCalls atomic.Int32

var reNumLine = regexp.MustCompile(`^(\d+)\|(.*)$`)

// fakeTranslateAPI 模拟 Index-Translate 的 /chat/completions。
// 它把每行 `N|原文` 变成 `N|【译】原文`，这样测试能断言"确实翻译过"且"顺序没错"。
func fakeTranslateAPI(t *testing.T) string {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		translateCalls.Add(1)
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		var prompt string
		for _, m := range req.Messages {
			prompt += m.Content + "\n"
		}
		var out []string
		for _, line := range strings.Split(prompt, "\n") {
			line = strings.TrimSpace(line)
			if m := reNumLine.FindStringSubmatch(line); m != nil {
				out = append(out, m[1]+"|【译】"+m[2])
			}
		}
		resp := map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{"content": strings.Join(out, "\n")}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv.URL
}

// ==================== 字幕挑错片 / 时长校验 ====================

// srtWithEnd 造一份「最后一条字幕结束在第 end 秒」的 SRT。
func srtWithEnd(endSec int, text string) string {
	ts := func(sec int) string {
		return fmt.Sprintf("%02d:%02d:%02d", sec/3600, sec%3600/60, sec%60)
	}
	return fmt.Sprintf("1\n00:00:01,000 --> 00:00:03,000\n%s\n\n2\n%s,000 --> %s,900\n%s\n",
		text, ts(endSec-2), ts(endSec), text)
}

type fakeItem struct {
	lang    string
	release string
	srt     string
}

// fakeSubsMulti 按条目各回一份字幕，用来模拟「同一 IMDb 下挂着不同片的字幕」。
func fakeSubsMulti(t *testing.T, items []fakeItem) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	mux.HandleFunc("/subtitles/", func(w http.ResponseWriter, r *http.Request) {
		var subs []map[string]any
		for i, it := range items {
			subs = append(subs, map[string]any{
				"id": fmt.Sprint(i), "url": fmt.Sprintf("%s/file/%d", srv.URL, i),
				"lang": it.lang, "subtitleFileName": it.release + ".srt",
				"movieReleaseName": it.release, "SubEncoding": "UTF-8",
			})
		}
		json.NewEncoder(w).Encode(map[string]any{"subtitles": subs})
	})
	mux.HandleFunc("/file/", func(w http.ResponseWriter, r *http.Request) {
		var idx int
		fmt.Sscanf(strings.TrimPrefix(r.URL.Path, "/file/"), "%d", &idx)
		w.Header().Set("Content-Type", "application/x-subrip; charset=utf-8")
		if idx >= 0 && idx < len(items) {
			io.WriteString(w, items[idx].srt)
		}
	})
	return srv
}

// TestRankChineseFirstThenScore 中文一定排在英文前面，同语种内按片源相似度降序。
func TestRankChineseFirstThenScore(t *testing.T) {
	list := []Candidate{
		{Lang: "eng", Release: "The.Odyssey.2026.2160p.WEB-DL", URL: "en"},
		{Lang: "zho", Release: "Odyssey_简体中文版", URL: "zh-low"},
		{Lang: "zho", Release: "The.Odyssey.2026.1080p.CAM", URL: "zh-high"},
	}
	got := Rank(list, "The.Odyssey.2026.2160p.UHD.Blu-ray.Remux.mkv")
	if len(got) != 3 {
		t.Fatalf("应保留 3 条中英候选，实际 %d", len(got))
	}
	if got[0].URL != "zh-high" || !got[0].Chinese() {
		t.Fatalf("第一条应是相似度最高的中文，实际 %+v", got[0])
	}
	if got[1].URL != "zh-low" {
		t.Fatalf("第二条应是另一条中文，实际 %+v", got[1])
	}
	if got[2].URL != "en" || got[2].Chinese() {
		t.Fatalf("英文必须排在中文之后，实际 %+v", got[2])
	}
	if got[0].Score <= got[1].Score {
		t.Fatalf("同语种内应按分数降序：%d vs %d", got[0].Score, got[1].Score)
	}
}

// TestFitsRuntime 时长校验的边界：不知道片长就放行，知道就按 0.75~1.15 卡。
func TestFitsRuntime(t *testing.T) {
	mk := func(min int) *File { return &File{Cues: []Cue{{End: time.Duration(min) * time.Minute}}} }
	for _, c := range []struct {
		cover, runtime int
		want           bool
	}{
		{164, 173, true},  // 正片，片尾几分钟没对白
		{173, 173, true},  // 严丝合缝
		{83, 173, false},  // 冒牌片：只有一半
		{200, 173, false}, // 加长版/整季，超出上限
		{164, 0, true},    // 不知道片长 → 放行
		{0, 173, false},   // 解析出 0 条时长，不能信
		{130, 173, true},  // 0.75 × 173 = 130，边界内
		{129, 173, false}, // 差一分钟就出局
	} {
		if got := fitsRuntime(mk(c.cover), c.runtime); got != c.want {
			t.Errorf("字幕 %d 分钟 / 片长 %d 分钟 → %v，期望 %v", c.cover, c.runtime, got, c.want)
		}
	}
}

// TestFetchSkipsWrongFilm 奥德赛现场：同名山寨片的字幕分数最高，
// 但它只有 83 分钟，必须被跳过；剩下的中文是枪版片源的，蓝光片源也不用它。
func TestFetchSkipsWrongFilm(t *testing.T) {
	srv := fakeSubsMulti(t, []fakeItem{
		// 分数最高（release 名里多一个前缀，bigram 命中反而更多）
		{lang: "zho", release: "The.Asylums.The.Odyssey.2026.1080p.AMZN.WEB-DL", srt: srtWithEnd(83*60, "山寨片的台词")},
		{lang: "zho", release: "The.Odyssey.2026.1080p.CAM", srt: srtWithEnd(164*60, "枪版的台词")},
		{lang: "eng", release: "The.Odyssey.2026.2160p.UHD.Blu-ray.Remux", srt: srtWithEnd(165*60, "Right film here.")},
	})
	api := fakeTranslateAPI(t)
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath:   "The Odyssey (2026)/The.Odyssey.2026.strm",
		VideoName: "The.Odyssey.2026.2160p.UHD.Blu-ray.Remux.HDR.HEVC.DTS-HD.MA.5.1-CiNEPHiLES.mkv",
		IMDbID:    "tt33764258", Kind: "movie",
	}, Options{Translate: true, RuntimeMin: 173, APIBase: api})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "The Odyssey (2026)/The.Odyssey.2026.zh.srt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "枪版的台词") {
		t.Fatalf("蓝光片源不该用枪版字幕：%s", data)
	}
	if !strings.Contains(string(data), "【译】Right film here.") {
		t.Fatalf("应退回英文字幕翻译，实际：%s", data)
	}
	if res.Origin != "translated" {
		t.Fatalf("应走翻译，实际 origin=%s", res.Origin)
	}
	joined := strings.Join(res.Tried, "；")
	if !strings.Contains(joined, "83") || !strings.Contains(joined, "枪版字幕") {
		t.Fatalf("应记下被跳过的山寨片与枪版字幕，实际 %v", res.Tried)
	}
}

// TestFetchScreenCopyForCamVideo 片源本身就是枪版时，枪版字幕照常可用。
func TestFetchScreenCopyForCamVideo(t *testing.T) {
	srv := fakeSubsMulti(t, []fakeItem{
		{lang: "zho", release: "Movie.2026.1080p.CAM.x264", srt: srtWithEnd(100*60, "枪版的台词")},
	})
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "M.strm", VideoName: "Movie.2026.1080p.CAM.x264.mkv",
		IMDbID: "tt1", Kind: "movie",
	}, Options{Translate: true, RuntimeMin: 100})
	if err != nil {
		t.Fatal(err)
	}
	if res.Origin != "zh" {
		t.Fatalf("枪版片源应直接用枪版中字，实际 %+v", res)
	}
}

// TestFetchMismatchNoTranslate 只有一条对不上的中文字幕、又没开翻译：
// 必须报 ErrMismatch，而不是把错片字幕当成功落盘。
func TestFetchMismatchNoTranslate(t *testing.T) {
	srv := fakeSubsMulti(t, []fakeItem{
		{lang: "zho", release: "The.Asylums.The.Odyssey.2026.1080p", srt: srtWithEnd(83*60, "山寨片的台词")},
	})
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	_, err := f.Fetch(context.Background(), root, Target{
		RelPath: "M.strm", VideoName: "The.Odyssey.2026.2160p.mkv",
		IMDbID: "tt33764258", Kind: "movie",
	}, Options{Translate: false, RuntimeMin: 173})
	if !errors.Is(err, ErrMismatch) {
		t.Fatalf("应报 ErrMismatch，实际 %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "M.zh.srt")); statErr == nil {
		t.Fatal("对不上的字幕不该落盘")
	}
}

// TestFetchMismatchThenTranslate 中文都对不上时，才退回英文字幕 + 翻译。
func TestFetchMismatchThenTranslate(t *testing.T) {
	srv := fakeSubsMulti(t, []fakeItem{
		{lang: "zho", release: "The.Asylums.The.Odyssey.2026.1080p", srt: srtWithEnd(83*60, "山寨片的台词")},
		{lang: "eng", release: "The.Odyssey.2026.2160p.UHD.Blu-ray", srt: srtWithEnd(168*60, "Right film here.")},
	})
	api := fakeTranslateAPI(t)
	f, _ := newFetcher(t)
	f.Source = NewSource(srv.URL)
	root := t.TempDir()

	res, err := f.Fetch(context.Background(), root, Target{
		RelPath: "M.strm", VideoName: "The.Odyssey.2026.2160p.UHD.Blu-ray.Remux.mkv",
		IMDbID: "tt33764258", Kind: "movie",
	}, Options{Translate: true, RuntimeMin: 173, APIBase: api})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Translated || res.Origin != "translated" {
		t.Fatalf("应退回翻译，实际 %+v", res)
	}
	data, _ := os.ReadFile(filepath.Join(root, "M.zh.srt"))
	if !strings.Contains(string(data), "【译】Right film here.") {
		t.Fatalf("译的是哪一份？%s", data)
	}
}

// 覆盖式重下要让 Emby 真的换字幕：旧的中文字幕必须让位，主媒体文件
// 的 mtime 要变；非覆盖模式一个字节都不该动。
func TestFinalizeSubtitleOverwrite(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "M")
	video := base + ".strm"
	oldZh := base + ".chs.srt"
	newZh := base + ".zh.srt"
	en := base + ".en.srt"
	for _, p := range []string{video, oldZh, newZh, en} {
		if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 固定一个旧时间戳，方便判断有没有被 bump
	past := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(video, past, past); err != nil {
		t.Fatal(err)
	}

	// 不覆盖：什么都不该发生
	finalizeSubtitle(base, video, false, nil)
	if _, err := os.Stat(oldZh); err != nil {
		t.Fatal("未覆盖时不该动旧字幕")
	}
	if st, _ := os.Stat(video); !st.ModTime().Equal(past) {
		t.Fatal("未覆盖时不该改主媒体文件的 mtime")
	}

	finalizeSubtitle(base, video, true, nil)
	if _, err := os.Stat(oldZh); err == nil {
		t.Fatal("旧中文字幕还在，Emby 会继续读它")
	}
	if _, err := os.Stat(oldZh + ".old"); err != nil {
		t.Fatal("旧字幕应改名保留，而不是直接删掉")
	}
	if _, err := os.Stat(newZh); err != nil {
		t.Fatal("新写的 .zh.srt 不该被改名")
	}
	if _, err := os.Stat(en); err != nil {
		t.Fatal("英文字幕不是中文字幕，不该被清理")
	}
	if st, _ := os.Stat(video); !st.ModTime().After(past) {
		t.Fatal("主媒体文件 mtime 没变，Emby 可能跳过重探")
	}
	if data, _ := os.ReadFile(video); string(data) != "x" {
		t.Fatal("只改时间戳，不能动 .strm 的内容")
	}
}
