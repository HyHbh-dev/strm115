package scrape

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveRoot 覆盖跨环境搬库的三种情形。
//
// 反例的代价：库是在 Docker 里建的（local_path 写成容器挂载点 /media/movies），
// 拿到 Windows 上跑时 /media/movies 不是绝对路径，filepath.Join 会落到
// 「当前盘符的根」D:\media\movies —— 同步的 .strm 在项目 media 目录、
// 海报跑到 D:\media、Emby 两张都读不到，而日志里一句错都没有。
func TestResolveRoot(t *testing.T) {
	mediaRoot := t.TempDir()
	existing := filepath.Join(mediaRoot, "movies")
	if err := os.MkdirAll(existing, 0o755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name      string
		localPath string
		want      string
	}{
		{
			name:      "容器里的绝对路径在这里不是绝对路径，退回媒体根",
			localPath: "/media/movies",
			want:      existing,
		},
		{
			name:      "已经是本机绝对路径且存在，原样使用",
			localPath: existing,
			want:      existing,
		},
		{
			name:      "相对路径也退回媒体根",
			localPath: "movies",
			want:      existing,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveRoot(c.localPath, mediaRoot)
			if filepath.Clean(got) != filepath.Clean(c.want) {
				t.Fatalf("ResolveRoot(%q) = %q，期望 %q", c.localPath, got, c.want)
			}
		})
	}
}

// TestResolveRootNoMediaRoot 没有媒体根时不许乱猜：原样返回，
// 让后面的写盘失败暴露出来，比静默写到一个谁也不知道的目录强。
func TestResolveRootNoMediaRoot(t *testing.T) {
	if got := ResolveRoot("/media/movies", ""); got != "/media/movies" {
		t.Fatalf("没有媒体根时应原样返回，实际 %q", got)
	}
	if got := ResolveRoot("", ""); got != "" {
		t.Fatalf("两边都空时应返回空串，实际 %q", got)
	}
}
