package tmdb

import (
	"strings"
	"testing"
)

// SetUpstream 是进程级状态，测完必须还原，否则同包其它测试拿到的
// PosterURL 前缀会莫名其妙变成反代地址。
func TestSetUpstream(t *testing.T) {
	t.Cleanup(func() { SetUpstream("", "") })

	// 1. 默认值（包初始化时）
	if api, img := CurrentUpstream(); api != DefaultBase || img != ImageBase {
		t.Fatalf("默认上游不对: api=%q image=%q", api, img)
	}

	// 2. imaliang/tmdb-proxy 的形状：/3 结尾，图片根自动推导成 /t/p。
	//    vercel.json 把 /t/p/:path* rewrite 到 image.tmdb.org/t/p/:path*。
	const root = "https://tmdb.example.cc.cd"
	SetUpstream(root+"/3", "")
	api, img := CurrentUpstream()
	if api != root+"/3" {
		t.Fatalf("接口根不对: %q", api)
	}
	if img != root+"/t/p" {
		t.Fatalf("图片根没按 /t/p 推导: %q", img)
	}
	if got := NewClient("dummy").Base; got != root+"/3" {
		t.Fatalf("NewClient 没跟上新上游: %q", got)
	}
	for _, tc := range []struct {
		name, got, wantPrefix string
	}{
		{"海报", PosterURL("/abc.jpg"), img + "/" + PosterSize + "/"},
		{"背景", BackdropURL("/abc.jpg"), img + "/" + BackdropSize + "/"},
		{"演员头像", imageURL("/abc.jpg", ProfileSize), img + "/" + ProfileSize + "/"},
	} {
		if !strings.HasPrefix(tc.got, tc.wantPrefix) {
			t.Fatalf("%s 没走反代: %q，期望前缀 %q", tc.name, tc.got, tc.wantPrefix)
		}
	}

	// 3. 尾部斜杠要规整：拼上 path 之后不能出现 //。
	SetUpstream(root+"/3/", "")
	if api, _ := CurrentUpstream(); api != root+"/3" {
		t.Fatalf("尾部斜杠没去掉: %q", api)
	}

	// 4. 不以 /3 结尾的自建反代：不去猜图片路径，保持官方 CDN。
	SetUpstream(root+"/custom", "")
	if _, img := CurrentUpstream(); img != ImageBase {
		t.Fatalf("不该瞎猜图片根，实际 %q", img)
	}

	// 5. 显式指定图片根时优先用它（并去掉尾斜杠）。
	SetUpstream(root+"/3", "https://cdn.example.com/img/")
	if _, img := CurrentUpstream(); img != "https://cdn.example.com/img" {
		t.Fatalf("显式图片根没生效: %q", img)
	}

	// 6. 清空 = 恢复官方直连。
	SetUpstream("", "")
	if api, img := CurrentUpstream(); api != DefaultBase || img != ImageBase {
		t.Fatalf("清空后没恢复默认: api=%q image=%q", api, img)
	}
}

// 已经是完整 http 地址的图片路径（少数上游会这么给）不该被再拼一次前缀。
func TestImageURLAbsolutePassthrough(t *testing.T) {
	t.Cleanup(func() { SetUpstream("", "") })
	SetUpstream("https://tmdb.example.cc.cd/3", "")
	const full = "https://cdn.example.com/a.jpg"
	if got := PosterURL(full); got != full {
		t.Fatalf("绝对地址被改写了: %q", got)
	}
}
