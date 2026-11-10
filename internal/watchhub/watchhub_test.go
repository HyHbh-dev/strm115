package watchhub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeUpstream 同时假扮 WatchHub 插件和 Cinemeta 元数据服务。
//
// 关键在于「类型纠偏」那条路径：movie 只给残缺 meta（没海报没年份），
// series 才是完整的 —— 真实环境里类型写错就是这个表现。
func fakeUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"org.stremio.watchhub","name":"WatchHub","version":"1.0.0",
			"resources":["stream"],"types":["movie","series"],"idPrefixes":["tt"],
			"catalogs":[]
		}`))
	})

	mux.HandleFunc("GET /meta/movie/tt1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{
			"id":"tt1","imdb_id":"tt1","name":"Dune","type":"movie","year":"2021",
			"releaseInfo":"2021","imdbRating":"8.0","runtime":"155",
			"genres":["Sci-Fi","Adventure"],"cast":["Timothée Chalamet"],
			"director":["Denis Villeneuve"],"description":"沙漠星球上的故事",
			"poster":"https://images.example/poster.jpg",
			"background":"https://images.example/bg.jpg"
		}}`))
	})

	mux.HandleFunc("GET /stream/movie/tt1.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"streams":[
			{"name":"Netflix","title":"订阅可看","externalUrl":"https://netflix.example/dune",
			 "androidUrl":"intent://dune"},
			{"name":"Prime Video","title":"可租借","externalUrl":"https://prime.example/dune"}
		],"copyright":"数据来自 WatchHub"}`))
	})

	// 类型写错的片子：movie 残缺，series 完整
	mux.HandleFunc("GET /meta/movie/tt2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{"id":"tt2","name":"Some Show","type":"movie"}}`))
	})
	mux.HandleFunc("GET /stream/movie/tt2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"streams":[]}`))
	})
	mux.HandleFunc("GET /meta/series/tt2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"meta":{
			"id":"tt2","imdb_id":"tt2","name":"Some Show","type":"series","year":"2019",
			"poster":"https://images.example/show.jpg"
		}}`))
	})
	mux.HandleFunc("GET /stream/series/tt2.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"streams":[{"name":"HBO Max","title":"订阅可看","externalUrl":"https://hbo.example/show"}]}`))
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestDetail(t *testing.T) {
	srv := fakeUpstream(t)
	c := NewClientWith(srv.URL, srv.URL)

	item, err := c.Detail(context.Background(), "movie", "tt1")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Name != "Dune" || item.Year != "2021" || item.IMDbRating != "8.0" {
		t.Fatalf("元数据不对: %+v", item)
	}
	if item.Poster == "" || item.Background == "" {
		t.Fatalf("海报/背景图丢了: %+v", item)
	}
	if item.Runtime != "155" {
		t.Fatalf("时长不对: %q", item.Runtime)
	}
	if len(item.Genres) != 2 || item.Genres[0] != "Sci-Fi" {
		t.Fatalf("类型标签不对: %v", item.Genres)
	}
	if item.PlatformCount != 2 || len(item.Platforms) != 2 {
		t.Fatalf("平台数量不对: %+v", item.Streams)
	}
	if item.WatchURL != "https://netflix.example/dune" {
		t.Fatalf("观看链接不对: %q", item.WatchURL)
	}
	if item.Streams[0].DeepLinks["android"] != "intent://dune" {
		t.Fatalf("深链没解析出来: %+v", item.Streams[0])
	}
}

// TestDetailTypeCorrection 类型写错时应该自动换类型重查，而不是返回残缺结果。
func TestDetailTypeCorrection(t *testing.T) {
	srv := fakeUpstream(t)
	c := NewClientWith(srv.URL, srv.URL)

	// 剧集 ID 按 movie 查 —— 真实场景里用户只给了 ID，不知道是电影还是剧
	item, err := c.Detail(context.Background(), "movie", "tt2")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.Type != "series" || item.Poster == "" {
		t.Fatalf("没纠偏成功: type=%q poster=%q", item.Type, item.Poster)
	}
	if item.PlatformCount != 1 || item.Platforms[0] != "HBO Max" {
		t.Fatalf("纠偏后应该用 series 重新查 stream: %+v", item.Streams)
	}
}

// TestDetailEpisodeForm 剧集形式的 tt123:1:1 要归一成 tt123。
func TestDetailEpisodeForm(t *testing.T) {
	srv := fakeUpstream(t)
	c := NewClientWith(srv.URL, srv.URL)

	item, err := c.Detail(context.Background(), "movie", "tt1:1:3")
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if item.IMDbID != "tt1" {
		t.Fatalf("ID 没归一: %q", item.IMDbID)
	}
}

// TestDetailBadID ID 不符合 manifest 前缀时要直接拒绝，不发请求。
func TestDetailBadID(t *testing.T) {
	srv := fakeUpstream(t)
	c := NewClientWith(srv.URL, srv.URL)

	if _, err := c.Detail(context.Background(), "movie", "nm0000123"); err == nil {
		t.Fatal("演员 ID 应该被拒绝")
	}
}

// TestManifestCached manifest 只拉一次。
func TestManifestCached(t *testing.T) {
	var hits int
	mux := http.NewServeMux()
	mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"id":"x","resources":["stream"],"types":["movie"],"idPrefixes":["tt"]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClientWith(srv.URL, srv.URL)
	for i := 0; i < 3; i++ {
		if _, err := c.Manifest(context.Background()); err != nil {
			t.Fatalf("拉 manifest 失败: %v", err)
		}
	}
	if hits != 1 {
		t.Fatalf("manifest 应该只拉一次，实际 %d 次", hits)
	}
}

func TestValidID(t *testing.T) {
	cases := []struct {
		id     string
		prefix []string
		want   bool
	}{
		{"tt1234567", nil, true},
		{"tt1234567", []string{"tt"}, true},
		{"nm1234567", []string{"tt"}, false},
	}
	for _, c := range cases {
		if got := ValidID(c.id, c.prefix); got != c.want {
			t.Errorf("ValidID(%q,%v) = %v，期望 %v", c.id, c.prefix, got, c.want)
		}
	}
}

func TestImageWithSize(t *testing.T) {
	raw := "https://images.metahub.xyz/poster/small/abc.jpg"
	if got := ImageWithSize(raw, "large"); !strings.Contains(got, "/poster/large/") {
		t.Errorf("尺寸没替换: %q", got)
	}
	if got := ImageWithSize(raw, ""); got != raw {
		t.Errorf("空尺寸应原样返回: %q", got)
	}
}
