package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

func testWallServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{st: st, log: slog.Default()}
}

// 平台清单是「每平台一行」海报墙的骨架：一进页面就要拿到所有行标题，
// 这个接口不能打 TMDB、也不能因为缓存没热就返回空列表。
func TestWallProvidersEndpoint(t *testing.T) {
	s := testWallServer(t)
	rec := httptest.NewRecorder()
	s.handleWallProviders(rec, httptest.NewRequest(http.MethodGet, "/api/wall/providers", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d，想 200", rec.Code)
	}
	var out struct {
		ProviderOptions []struct {
			Code string `json:"code"`
			Name string `json:"name"`
		} `json:"provider_options"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if len(out.ProviderOptions) != len(providerCodes) {
		t.Fatalf("平台数 = %d，白名单里是 %d 个", len(out.ProviderOptions), len(providerCodes))
	}
	seen := map[string]bool{}
	for _, p := range out.ProviderOptions {
		seen[p.Code] = true
	}
	for _, code := range []string{"nfx", "dnp", "tx", "iqy"} {
		if !seen[code] {
			t.Errorf("缺平台 %s", code)
		}
	}
}

// 缓存的核心是「少一个参数就会串榜单」：平台、类型、页码、地区都得进键。
func TestWallCacheKeyDistinct(t *testing.T) {
	base := wallCacheKey(tmdb.KindMovie, []string{"nfx"}, "action", "popular", 1, "US", "", 30)
	cases := map[string]string{
		"换平台": wallCacheKey(tmdb.KindMovie, []string{"iqy"}, "action", "popular", 1, "US", "", 30),
		"换类型": wallCacheKey(tmdb.KindTV, []string{"nfx"}, "action", "popular", 1, "US", "", 30),
		"换页码": wallCacheKey(tmdb.KindMovie, []string{"nfx"}, "action", "popular", 2, "US", "", 30),
		"换地区": wallCacheKey(tmdb.KindMovie, []string{"nfx"}, "action", "popular", 1, "HK", "", 30),
		"换兜底": wallCacheKey(tmdb.KindMovie, []string{"yku"}, "action", "popular", 1, "US", "CN", 30),
	}
	for name, key := range cases {
		if key == base {
			t.Errorf("%s 后缓存键没变：%s", name, key)
		}
	}
	if wallCacheKey(tmdb.KindMovie, []string{"nfx"}, "action", "popular", 1, "US", "", 30) != base {
		t.Fatal("同样的参数应该得到同样的键")
	}
}

func TestWallCacheRoundTrip(t *testing.T) {
	s := testWallServer(t)
	ctx := context.Background()
	key := wallCacheKey(tmdb.KindMovie, []string{"nfx"}, "", "popular", 1, "US", "", 30)
	if _, ok := s.readWallCache(ctx, key); ok {
		t.Fatal("空缓存不该命中")
	}
	want := wallPayload{
		Items:    []wallItem{{Name: "沙丘2", TMDBID: 693134}},
		Source:   "tmdb",
		Page:     1,
		CachedAt: time.Now().Format(time.RFC3339),
	}
	s.saveWallCache(ctx, key, want)
	got, ok := s.readWallCache(ctx, key)
	if !ok {
		t.Fatal("写入后应该命中缓存")
	}
	if len(got.Items) != 1 || got.Items[0].Name != "沙丘2" {
		t.Fatalf("缓存内容不对：%+v", got.Items)
	}
	if got.CachedAt != want.CachedAt {
		t.Fatal("缓存时间应保留写入时的值，用于告诉前端这份数据有多旧")
	}
	// 平台选项 / 类型选项不进缓存，是每个响应现算的常量，不受旧 JSON 影响。
}

func TestWallCacheExpired(t *testing.T) {
	s := testWallServer(t)
	ctx := context.Background()
	key := wallCacheKey(tmdb.KindTV, nil, "", "top", 1, "US", "", 30)
	s.saveWallCache(ctx, key, wallPayload{
		Items:    []wallItem{{Name: "旧剧"}},
		CachedAt: time.Now().Add(-wallTTL - time.Minute).Format(time.RFC3339),
	})
	if _, ok := s.readWallCache(ctx, key); ok {
		t.Fatal("超过 TTL 的缓存不该命中")
	}
}

// 国内平台分两类：TMDB 有 provider ID 的真查，没有的用国产内容兜底并给出说明。
func TestWallCNProviders(t *testing.T) {
	if tmdbProviderID("tx") != 623 || tmdbProviderID("iqy") != 581 || tmdbProviderID("wetv") != 509 {
		t.Fatal("国内真实 provider ID 变了，TMDB 侧要同步核对")
	}
	p := providerByCode("yku")
	if p == nil || p.TMDB != 0 || p.Group != "cn" || p.Note == "" {
		t.Fatalf("兜底平台必须标明无片单，拿到 %+v", p)
	}
	if !knownProvider("mgtv") || knownProvider("not-a-provider") {
		t.Fatal("平台白名单判定不对")
	}
}
