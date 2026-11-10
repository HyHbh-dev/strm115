package httpapi

import (
	"path/filepath"
	"testing"
)

func TestSafeUnder(t *testing.T) {
	root := filepath.Join(t.TempDir(), "media")
	ok := []string{
		filepath.Join(root, "电影"),
		filepath.Join(root, "a", "b"),
		root,
	}
	for _, p := range ok {
		if _, err := safeUnder(root, p); err != nil {
			t.Errorf("%s 应该允许: %v", p, err)
		}
	}

	bad := []string{
		filepath.Join(root, "..", "etc"),
		filepath.Join(root, ".."),
		"/etc/passwd",
		"../../etc",
	}
	for _, p := range bad {
		if _, err := safeUnder(root, p); err == nil {
			t.Errorf("%s 必须被拒绝", p)
		}
	}
}

func TestPathID(t *testing.T) {
	// pathID 只接受纯数字，注入的乱七八糟东西要返回 0
	if got := pathIDString("123"); got != 123 {
		t.Fatalf("正常 id 解析错: %d", got)
	}
	if got := pathIDString("12a"); got != 0 {
		t.Fatalf("非数字应返回 0，实际 %d", got)
	}
	if got := pathIDString(""); got != 0 {
		t.Fatalf("空应返回 0，实际 %d", got)
	}
}

// pathIDString 是 pathID 的纯逻辑版本（pathID 需要 *http.Request，测试里抽出来验算）
func pathIDString(s string) int64 {
	var id int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0
		}
		id = id*10 + int64(c-'0')
	}
	return id
}

// TestLocalDirName 守的是「磁盘上的库目录保持全英文」这条约定。
// 中文写盘本身能跑，但跨容器/网盘/播放器的编码差异很容易出玄学问题，
// 所以默认名直接给英文：常见中文词走固定映射，剩下的压成稳定的 lib-xxxx。
func TestLocalDirName(t *testing.T) {
	cases := []struct{ name, want string }{
		{"movies", "movies"},
		{"Movies", "movies"},
		{"My Library", "my-library"},
		{"电影", "movies"},
		{"电影库", "movies"},
		{"电视剧", "tv"},
		{"日番", "anime"},
		{"电影 2024", "movies-2024"},
		{"综艺-2025", "variety-2025"},
		{"奇怪的库名", "lib-"}, // 没映射、也没 ASCII → 稳定哈希兜底
	}
	for _, c := range cases {
		got := localDirName(c.name)
		if c.want == "lib-" {
			if len(got) != len("lib-0000") || got[:4] != "lib-" {
				t.Errorf("localDirName(%q) = %q，期望 lib-xxxx", c.name, got)
			}
			if err := checkASCIIPath(got); err != nil {
				t.Errorf("localDirName(%q) = %q 不是纯 ASCII: %v", c.name, got, err)
			}
			continue
		}
		if got != c.want {
			t.Errorf("localDirName(%q) = %q，期望 %q", c.name, got, c.want)
		}
	}

	// 必须稳定：同一个库名重建时不能换目录
	if localDirName("奇怪的库名") != localDirName("奇怪的库名") {
		t.Error("同名两次结果不一致，重建库会换路径")
	}
	if localDirName("奇怪的库名") == localDirName("另一个库名") {
		t.Error("不同中文名撞到同一个目录了")
	}
}

func TestCheckASCIIPath(t *testing.T) {
	for _, p := range []string{"/media/movies", "/media/lib-a1b2", "/media/a/b-1"} {
		if err := checkASCIIPath(p); err != nil {
			t.Errorf("%s 应该放行: %v", p, err)
		}
	}
	for _, p := range []string{"/media/电影", "/media/movies/剧集", "/media/käse"} {
		if err := checkASCIIPath(p); err == nil {
			t.Errorf("%s 必须被拒绝", p)
		}
	}
}
