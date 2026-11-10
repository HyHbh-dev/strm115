package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// withFakeDouban 把 douban_base 指到本地假服务器，顺带把 douban_enabled 打开。
//
// 用设置项而不是替换包里的函数：这条链路真正的坑就在「读哪份配置、走不走
// 代理」，绕开设置就等于没测。假服务器同时扮演两个域名（rexxar 与 /j/），
// 因为真实部署里的反代也是这么分流的。
func withFakeDouban(t *testing.T, s *Server) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/j/subject_suggest":
			_, _ = w.Write([]byte(`[
				{"episode":"","img":"https://img3.doubanio.com/a.jpg","title":"沙丘2",
				 "type":"movie","year":"2024","sub_title":"Dune: Part Two","id":"35575567"},
				{"episode":"12","img":"https://img9.doubanio.com/b.jpg","title":"漫长的季节",
				 "type":"movie","year":"2023","sub_title":"漫长的季节","id":"35588177"}
			]`))
		case strings.Contains(r.URL.Path, "/subject_collection/tv_domestic/items"):
			_, _ = w.Write([]byte(`{"start":0,"count":1,"total":49,"subject_collection_items":[
				{"id":"37191789","title":"嫁金钗","type":"tv","year":"2026",
				 "episodes_info":"更新至28集","rating":{"value":7.0},
				 "cover":{"url":"https://img3.doubanio.com/c.jpg"},
				 "card_subtitle":"2026 / 中国大陆 / 剧情"}
			]}`))
		case strings.Contains(r.URL.Path, "/rexxar/api/v2/tv/37191789"):
			_, _ = w.Write([]byte(`{"id":"37191789","title":"嫁金钗","year":"2026","type":"tv",
				"episodes_count":"28","episodes_info":"更新至28集",
				"rating":{"value":7.0},"intro":"冲喜新娘…",
				"pubdate":["2026-02-14(中国大陆)"]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	if err := s.st.SetSetting(t.Context(), "douban_base", srv.URL); err != nil {
		t.Fatal(err)
	}
	if err := s.st.SetSetting(t.Context(), "douban_enabled", "1"); err != nil {
		t.Fatal(err)
	}
}

// 搜索候选：电影和剧要按 episode 字段分清，否则前端会把剧当电影去搜种子。
func TestDoubanSearchEndpoint(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeDouban(t, s)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/douban/search?q=沙丘2", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("搜索失败 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []struct {
			ID     int    `json:"id"`
			Kind   string `json:"kind"`
			Title  string `json:"title"`
			Poster string `json:"poster"`
		} `json:"items"`
		Attribution string `json:"attribution"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if len(body.Items) != 2 {
		t.Fatalf("应返回 2 条候选: %+v", body.Items)
	}
	if body.Items[0].Kind != "movie" || body.Items[0].ID != 35575567 {
		t.Errorf("电影条目不对: %+v", body.Items[0])
	}
	if body.Items[1].Kind != "tv" {
		t.Errorf("有 episode 的应认成剧集: %+v", body.Items[1])
	}
	// 豆瓣要求署名，接口得把文案带上，不能指望前端硬编码
	if body.Attribution == "" {
		t.Error("应该带上署名文案")
	}
}

// 榜单：国产剧的「更新至 N 集」只有豆瓣有，日历要靠它。
func TestDoubanCollectionEndpoint(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeDouban(t, s)
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/douban/collection?key=tv_domestic&count=5", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("榜单失败 %d: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Total int `json:"total"`
		Items []struct {
			Title        string  `json:"title"`
			Kind         string  `json:"kind"`
			EpisodesInfo string  `json:"episodes_info"`
			Rating       float64 `json:"rating"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应不是 JSON: %s", rec.Body.String())
	}
	if body.Total != 49 || len(body.Items) != 1 {
		t.Fatalf("榜单内容不对: %+v", body)
	}
	if body.Items[0].EpisodesInfo != "更新至28集" || body.Items[0].Rating != 7.0 {
		t.Errorf("更新进度/评分没透出去: %+v", body.Items[0])
	}
}

// 关掉开关就不要再打豆瓣：境外部署豆瓣被墙时，每次搜索都白等十几秒。
func TestDoubanDisabledSkipsUpstream(t *testing.T) {
	s, _ := newServerWithStore(t)
	withFakeDouban(t, s)
	if err := s.st.SetSetting(t.Context(), "douban_enabled", "0"); err != nil {
		t.Fatal(err)
	}
	// 把地址改成一个连不上的地方：真被调用了就会 502，而不是返回空列表
	if err := s.st.SetSetting(t.Context(), "douban_base", "http://127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet, "/api/douban/search?q=沙丘2", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("关掉之后不应报错 %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("应该告诉前端「豆瓣已关」: %s", rec.Body.String())
	}
}
