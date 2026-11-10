package netcheck

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"strm115/internal/tmdb"
)

func TestProbeOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	r := probe(context.Background(), client(""), Target{Key: "x", Name: "测试", Host: "127.0.0.1", URL: srv.URL}, "direct")
	if !r.OK || r.Status != http.StatusNoContent {
		t.Fatalf("应判定可达，实际 %+v", r)
	}
	if r.Via != "direct" {
		t.Errorf("via = %q", r.Via)
	}
}

// 404 也说明网络是通的（api.telegram.org 根路径就回 404），不能算红。
func TestProbeCounts404AsReachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	r := probe(context.Background(), client(""), Target{URL: srv.URL}, "direct")
	if !r.OK || r.Status != http.StatusNotFound {
		t.Fatalf("404 应算可达，实际 %+v", r)
	}
}

func TestProbeProxyAuthIsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusProxyAuthRequired)
	}))
	defer srv.Close()

	r := probe(context.Background(), client(""), Target{URL: srv.URL}, "proxy")
	if r.OK {
		t.Fatal("407 说明代理要认证，不该算通")
	}
	if !strings.Contains(r.Err, "认证") {
		t.Errorf("错误提示 = %q", r.Err)
	}
}

func TestProbeUnreachable(t *testing.T) {
	// 先占一个端口再关掉，保证没人监听
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()

	r := probe(context.Background(), client(""), Target{URL: "http://" + addr + "/"}, "direct")
	if r.OK {
		t.Fatal("端口没人监听，不该判可达")
	}
	if r.Err == "" {
		t.Error("失败原因不能为空，否则界面只剩一个红叉")
	}
}

func TestExplainHumanizes(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"dial tcp 1.2.3.4:443: connect: connection refused", "拒绝连接"},
		{"dial tcp: lookup nope.invalid: no such host", "DNS 解析失败"},
		{"net/http: TLS handshake timeout", "超时"},
		{"proxyconnect tcp: dial tcp 127.0.0.1:7890: connect: refused", "连不上代理"},
	}
	for _, c := range cases {
		got := explain(errors.New(c.msg), "direct")
		if !strings.Contains(got, c.want) {
			t.Errorf("explain(%q) = %q，应包含 %q", c.msg, got, c.want)
		}
	}
}

// 清单本身要经得起检查：URL 必须是 https、key 不重复、分组不为空。
func TestTargetsSane(t *testing.T) {
	seen := map[string]bool{}
	for _, tg := range Targets() {
		if tg.Key == "" || tg.Name == "" || tg.Host == "" || tg.Group == "" {
			t.Errorf("字段不全: %+v", tg)
		}
		if !strings.HasPrefix(tg.URL, "https://") {
			t.Errorf("%s 不是 https: %s", tg.Key, tg.URL)
		}
		if seen[tg.Key] {
			t.Errorf("key 重复: %s", tg.Key)
		}
		seen[tg.Key] = true
	}
	// 几个「项目真的依赖」的端点必须在清单里
	for _, must := range []string{"115", "telegram", "tmdb"} {
		if !seen[must] {
			t.Errorf("清单里缺 %s", must)
		}
	}
}

// Run 在没有代理时只出直连结果，配了代理才有 proxy 行。
func TestRunShape(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	rep := Run(ctx, "")
	if rep.Total != len(rep.Results) || rep.Total == 0 {
		t.Fatalf("统计不对: %+v", rep)
	}
	for _, r := range rep.Results {
		if r.Via != "direct" {
			t.Fatalf("没配代理却出现 %s 行", r.Via)
		}
	}
}

// 配了 TMDB 反代之后，网络检测应该戳反代而不是官方域名，
// 否则页面上「api.themoviedb.org 红了」会让人以为配置没生效。
func TestTargetsFollowTMDBUpstream(t *testing.T) {
	t.Cleanup(func() { tmdb.SetUpstream("", "") })
	tmdb.SetUpstream("https://tmdb.example.cc.cd/3", "")

	var api, img Target
	for _, tg := range Targets() {
		switch tg.Key {
		case "tmdb":
			api = tg
		case "tmdbimg":
			img = tg
		}
	}
	if api.Host != "tmdb.example.cc.cd" || api.URL != "https://tmdb.example.cc.cd/3/configuration" {
		t.Fatalf("接口目标没跟随反代: %+v", api)
	}
	if img.Host != "tmdb.example.cc.cd" || img.URL != "https://tmdb.example.cc.cd/t/p/w92/" {
		t.Fatalf("图片目标没跟随推导出的图片根: %+v", img)
	}

	// 恢复官方时两个域名要退回各自那一个（接口与图片不是同一个域）
	tmdb.SetUpstream("", "")
	for _, tg := range Targets() {
		if tg.Key == "tmdb" && tg.Host != "api.themoviedb.org" {
			t.Fatalf("恢复官方后接口域名不对: %s", tg.Host)
		}
		if tg.Key == "tmdbimg" && tg.Host != "image.tmdb.org" {
			t.Fatalf("恢复官方后图片域名不对: %s", tg.Host)
		}
	}
}
