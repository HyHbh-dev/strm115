package subtitle

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestLiveOdysseyPicksRightFilm 是奥德赛那次的现场回归：联网跑真实字幕源。
//
// 这个 IMDb 下同时挂着 The Asylum 拍的同名山寨片字幕（83 分钟）和我们
// 要的那部（173 分钟；CAM/TELESYNC 版 164 分钟），而且山寨片那条因为
// release 名里多一个前缀，片源相似度反而最高。要验证的就是「时长校验
// 能把它筛掉，最后挑到同一部片的中文」。
//
// 默认跳过（会打外网），手动跑：
//
//	SUBTITLE_LIVE=1 go test ./internal/subtitle -run TestLiveOdysseyPicksRightFilm -v
func TestLiveOdysseyPicksRightFilm(t *testing.T) {
	if os.Getenv("SUBTITLE_LIVE") == "" {
		t.Skip("联网用例：设 SUBTITLE_LIVE=1 才跑")
	}
	f, _ := newFetcher(t)
	// 公共字幕站偶尔要 10s+，默认 20s 有点紧
	f.Source = &Source{Base: DefaultSourceBase, HTTP: &http.Client{Timeout: 60 * time.Second}}

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	root := t.TempDir()
	rel := "The Odyssey (2026)/The.Odyssey.2026.2160p.strm"
	res, err := f.Fetch(ctx, root, Target{
		RelPath:   rel,
		VideoName: "The.Odyssey.2026.2160p.UHD.Blu-ray.Remux.HDR.HEVC.DTS-HD.MA.5.1-CiNEPHiLES.mkv",
		IMDbID:    "tt33764258",
		Kind:      "movie",
	}, Options{Translate: false, RuntimeMin: 173})
	if err != nil {
		t.Fatalf("补字幕失败: %v", err)
	}

	t.Logf("选中: %s（相似度 %d，%d 条，来源 %s）", res.Release, res.Score, res.Cues, res.Origin)
	for _, tried := range res.Tried {
		t.Logf("跳过: %s", tried)
	}
	if res.Origin != "zh" {
		t.Fatalf("应该直接落中文字幕，实际 %s", res.Origin)
	}
	if strings.Contains(strings.ToLower(res.Release), "asylums") {
		t.Fatalf("又挑到山寨片的字幕了：%s", res.Release)
	}
	if res.Cues < 500 {
		t.Fatalf("条数太少，不像正片字幕：%d", res.Cues)
	}

	body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(res.Saved)))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "-->") {
		t.Fatalf("落盘的不是字幕：%q", string(body[:minInt(len(body), 120)]))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
