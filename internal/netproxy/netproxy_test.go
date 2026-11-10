package netproxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// 关键回归：客户端先构造、代理后设置，也必须立刻切过去。
// tmdb / scrape 那些包级单例正是这种情况，改设置不该要求重启容器。
func TestSetAppliesToAlreadyBuiltTransport(t *testing.T) {
	var hits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("via proxy"))
	}))
	defer proxy.Close()

	hc := &http.Client{
		Transport: &http.Transport{Proxy: URL},
		Timeout:   3 * time.Second,
	}
	t.Cleanup(func() { Set("") })

	Set(proxy.URL)
	req, _ := http.NewRequest(http.MethodGet, "http://example.invalid/hello", nil)
	res, err := hc.Do(req)
	if err != nil {
		t.Fatalf("代理没生效，请求直连出去了: %v", err)
	}
	defer res.Body.Close()
	if hits.Load() != 1 {
		t.Errorf("假代理被命中 %d 次，期望 1 次", hits.Load())
	}
	if Get() != proxy.URL {
		t.Errorf("Get() = %q", Get())
	}
}

func TestSetEmptyClearsOverride(t *testing.T) {
	t.Cleanup(func() { Set("") })
	Set("http://127.0.0.1:7890")
	Set("")
	if Get() != "" {
		t.Errorf("留空没清掉: %q", Get())
	}
}

func TestSetRejectsBadProxy(t *testing.T) {
	t.Cleanup(func() { Set("") })
	for _, bad := range []string{"not-a-url", "ftp://127.0.0.1:21", "127.0.0.1:7890"} {
		Set(bad)
		if Get() != "" {
			t.Errorf("坏地址 %q 不该被收下，实际 Get()=%q", bad, Get())
		}
	}
	Set("socks5://127.0.0.1:1080")
	if Get() != "socks5://127.0.0.1:1080" {
		t.Errorf("socks5 应该合法，实际 %q", Get())
	}
}
