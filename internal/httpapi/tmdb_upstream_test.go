package httpapi

import (
	"context"
	"net/http"
	"testing"

	"strm115/internal/tmdb"
)

// 反代地址不是密钥：留空 = 清空（恢复官方），而不是「不修改」。
// 这条语义跟 imdb_api_key 那些正好相反，最容易在保存时被写错。
func TestValidTMDBBase(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  https://tmdb.example.cc.cd/3/  ", "https://tmdb.example.cc.cd/3", true},
		{"http://192.168.1.5:8080/3", "http://192.168.1.5:8080/3", true},
		// 只填域名会拼成相对路径，http.NewRequest 不报错、发出去才失败
		{"tmdb.example.cc.cd/3", "", false},
		{"ftp://x/3", "", false},
		{"https://x/3?api_key=abc", "", false},
		{"https://x/3#frag", "", false},
	}
	for _, c := range cases {
		got, err := validTMDBBase(c.in)
		if c.ok && err != nil {
			t.Fatalf("%q 应该合法，实际报错 %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q 应该被拒绝，实际放行了（%q）", c.in, got)
		}
		if c.ok && got != c.want {
			t.Fatalf("%q 规整成 %q，期望 %q", c.in, got, c.want)
		}
	}
}

// 保存设置后必须立刻对所有 TMDB 调用生效（不能要求重启容器），
// 而且清空要真的退回官方直连。
func TestPutSettingsAppliesTMDBUpstream(t *testing.T) {
	t.Cleanup(func() { tmdb.SetUpstream("", "") })
	s, st := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	const root = "https://tmdb.example.cc.cd"
	rec, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tmdb_base": root + "/3"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "tmdb_base"); v != root+"/3" {
		t.Fatalf("没落库: %q", v)
	}
	api, img := tmdb.CurrentUpstream()
	if api != root+"/3" || img != root+"/t/p" {
		t.Fatalf("进程内上游没跟着变: api=%q image=%q", api, img)
	}

	// 新构造的客户端（电影墙 / 订阅走这条）也要用新地址
	if got := tmdb.NewClient("dummy").Base; got != root+"/3" {
		t.Fatalf("NewClient 还在用旧地址: %q", got)
	}

	// 留空 = 清空，不是「不修改」
	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tmdb_base": ""}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "tmdb_base"); v != "" {
		t.Fatalf("空值应真的写进去（恢复官方），实际 %q", v)
	}
	if api, img := tmdb.CurrentUpstream(); api != tmdb.DefaultBase || img != tmdb.ImageBase {
		t.Fatalf("清空后没恢复默认: api=%q image=%q", api, img)
	}
}

func TestPutSettingsRejectsBadTMDBBase(t *testing.T) {
	t.Cleanup(func() { tmdb.SetUpstream("", "") })
	s, st := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tmdb_base": "tmdb.example.cc.cd/3"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 scheme 的地址应该 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if _, ok := st.GetSetting(context.Background(), "tmdb_base"); ok {
		t.Fatal("校验失败时不该写库")
	}
}
