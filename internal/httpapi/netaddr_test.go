package httpapi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"strm115/internal/store"
)

func TestNormalizePlayDomain(t *testing.T) {
	ok := map[string]string{
		"http://192.168.1.10:6086":       "http://192.168.1.10:6086",
		"192.168.1.10:6086":              "http://192.168.1.10:6086",
		"http://nas:6086/":               "http://nas:6086",
		"https://media.example.com":      "https://media.example.com",
		"  http://10.0.0.5:6086/strm/  ": "http://10.0.0.5:6086/strm",
	}
	for in, want := range ok {
		got, err := normalizePlayDomain(in)
		if err != nil || got != want {
			t.Errorf("normalizePlayDomain(%q) = (%q, %v), 期望 %q", in, got, err, want)
		}
	}

	bad := []string{
		"", "   ", "127.0.0.1:6086", "http://localhost:6086", "http://0.0.0.0:6086",
		"ftp://nas:6086",
	}
	for _, in := range bad {
		if _, err := normalizePlayDomain(in); err == nil {
			t.Errorf("%q 应该被拒绝", in)
		}
	}
}

func TestDomainHintFlagsDockerRange(t *testing.T) {
	if hint := domainHint("http://172.18.0.2:6086"); hint == "" {
		t.Error("172.18.x 应该给出提示")
	}
	if hint := domainHint("http://192.168.1.10:6086"); hint != "" {
		t.Errorf("192.168.x 不该有提示，实际 %q", hint)
	}
}

func TestReplaceDomainOnce(t *testing.T) {
	in := "http://172.18.0.2:6086/d/abc123.mkv?/空枪.2026.mkv\n"
	got, changed := replaceDomainOnce(in, "http://nas:6086")
	if !changed {
		t.Fatal("应该判定为有变化")
	}
	want := "http://nas:6086/d/abc123.mkv?/空枪.2026.mkv\n"
	if got != want {
		t.Errorf("改写结果 = %q, 期望 %q", got, want)
	}

	// 同一个地址再来一次应当是无操作，避免把文件 mtime 刷成「总是变化」
	if _, changed := replaceDomainOnce(got, "http://nas:6086"); changed {
		t.Error("地址相同时不该再改写")
	}
}

// TestRewritePlayDomain 端到端：库里的 .strm 全部指向新地址，文件名不动。
func TestRewritePlayDomain(t *testing.T) {
	s, st := newServerWithStore(t)
	ctx := context.Background()

	root := t.TempDir()
	dir := filepath.Join(root, "电影", "空枪.2026.{tmdbid=1}")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	strmPath := filepath.Join(dir, "空枪.2026.strm")
	if err := os.WriteFile(strmPath, []byte("http://172.18.0.2:6086/d/abc.mkv?/空枪.2026.mkv\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	acct, _ := st.CreateAccount(ctx, "115", "t", "{}")
	if _, err := st.CreateLibrary(ctx, store.Library{
		AccountID: acct, Name: "库", RemoteCID: "0",
		LocalPath: root, CollectionType: "movies", KeepExt: true,
	}); err != nil {
		t.Fatal(err)
	}

	files, changed, err := s.rewritePlayDomain(ctx, "http://nas:6086")
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || changed != 1 {
		t.Fatalf("files=%d changed=%d，期望 1/1", files, changed)
	}
	b, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(b), "http://nas:6086/d/abc.mkv") {
		t.Errorf("内容没改对: %q", string(b))
	}
	// 文件名与目录必须原样保留，否则 Emby 那边会当成新条目
	if _, err := os.Stat(strmPath); err != nil {
		t.Errorf("原文件不见了: %v", err)
	}
}

// TestRewritePlayDomainWithoutLibraries 覆盖「接手一台已有媒体目录的机器」：
// 磁盘上是上一套系统留下的 .strm，本项目台账还是空的。
// 这时页面填播放地址必须能改到这些文件，否则「保存并改写」显示 0/0，
// 公网播放器拿到的还是 http://172.17.0.1:6086/… 这种不可达地址。
func TestRewritePlayDomainWithoutLibraries(t *testing.T) {
	s, _ := newServerWithStore(t)
	ctx := context.Background()

	root := t.TempDir()
	dir := filepath.Join(root, "电影")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	strmPath := filepath.Join(dir, "赛博朋克.S01E09.strm")
	old := "http://172.17.0.1:6086/d/ctdvbd6p1km4udxn4.mkv?/赛博朋克.S01E09.mkv\n"
	if err := os.WriteFile(strmPath, []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	s.cfg.MediaRoot = root

	files, changed, err := s.rewritePlayDomain(ctx, "https://emby.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if files != 1 || changed != 1 {
		t.Fatalf("files=%d changed=%d，期望 1/1（台账为空也要扫 MEDIA_ROOT）", files, changed)
	}
	b, err := os.ReadFile(strmPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "https://emby.example.com/d/ctdvbd6p1km4udxn4.mkv?/赛博朋克.S01E09.mkv"
	if strings.TrimSpace(string(b)) != want {
		t.Errorf("改写结果 = %q，期望 %q", strings.TrimSpace(string(b)), want)
	}
	// 幂等：再跑一次不该重复写盘
	if _, changed, err := s.rewritePlayDomain(ctx, "https://emby.example.com"); err != nil || changed != 0 {
		t.Errorf("第二次不该再改写: changed=%d err=%v", changed, err)
	}
}
