package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/tmdb"
)

// withFakeTMDBMeta 把打 TMDB 的那一步换成假数据：既能让 handler 在不连外网的
// 情况下返回完整响应，又能顺便记录传给底层的 kind，验证非法 type 的归一化。
// 这些用例都不加 t.Parallel —— 它改的是包级变量，必须串行执行。
func withFakeTMDBMeta(t *testing.T, hits []tmdb.MetaHit, captured *string) {
	t.Helper()
	old := fetchTMDBMeta
	fetchTMDBMeta = func(_ *Server, _ context.Context, _, _, kind string) ([]tmdb.MetaHit, error) {
		if captured != nil {
			*captured = kind
		}
		return hits, nil
	}
	t.Cleanup(func() { fetchTMDBMeta = old })
}

// 缺 q 会在碰 s.tmdbKey / 网络之前就返回，用零值 Server 即可。
func TestHandleTMDBSearchMissingQ(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/tmdb/search", nil)
	s.handleTMDBSearch(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 q 应返回 400，实际 %d", rec.Code)
	}
}

func TestHandleTMDBSearchBlankQ(t *testing.T) {
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/tmdb/search?q=%20%20", nil)
	s.handleTMDBSearch(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("q 全是空格应返回 400，实际 %d", rec.Code)
	}
}

// type 是非法值时不该报错，而应被当成「两类都搜」（kind=""）传给底层。
func TestHandleTMDBSearchIllegalTypeSearchesBoth(t *testing.T) {
	var gotKind string
	withFakeTMDBMeta(t, nil, &gotKind)
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/tmdb/search?q=Dune&type=banana", nil)
	s.handleTMDBSearch(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("非法 type 不该报错，应 200，实际 %d body=%s", rec.Code, rec.Body.String())
	}
	if gotKind != "" {
		t.Errorf("非法 type 应映射到「两类都搜」(kind=\"\")，实际 kind=%q", gotKind)
	}
}

// 底层返回空结果时，响应里的 items 必须仍是 [] 而不是 null。
func TestHandleTMDBSearchItemsAlwaysArray(t *testing.T) {
	withFakeTMDBMeta(t, nil, nil)
	s := &Server{}
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/tmdb/search?q=whatever", nil)
	s.handleTMDBSearch(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，实际 %d", rec.Code)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	items, ok := raw["items"]
	if !ok || len(items) == 0 || items[0] != '[' {
		t.Errorf("items 必须是数组，实际 %s", string(items))
	}
	if string(raw["total"]) != "0" {
		t.Errorf("空结果 total 应为 0，实际 %s", string(raw["total"]))
	}
	if string(raw["query"]) != `"whatever"` {
		t.Errorf("query 应回显，实际 %s", string(raw["query"]))
	}
}

func TestNormalizeSearchKind(t *testing.T) {
	cases := map[string]string{
		"movie":  tmdb.KindMovie,
		"MOVIE":  tmdb.KindMovie,
		" tv ":   tmdb.KindTV,
		"series": tmdb.KindTV,
		"":       "",
		"banana": "",
	}
	for in, want := range cases {
		if got := normalizeSearchKind(in); got != want {
			t.Errorf("normalizeSearchKind(%q)=%q, want %q", in, got, want)
		}
	}
}

// 电影热度普遍远高于剧集：验证不会被电影占满，且整体仍按热度降序。
func TestSortAndTrimHits(t *testing.T) {
	var hits []tmdb.MetaHit
	for i := 0; i < 10; i++ {
		hits = append(hits, tmdb.MetaHit{ID: i, Kind: tmdb.KindMovie, Popularity: float64(100 - i)})
	}
	for i := 0; i < 10; i++ {
		hits = append(hits, tmdb.MetaHit{ID: 100 + i, Kind: tmdb.KindTV, Popularity: float64(50 - i)})
	}
	got := sortAndTrimHits(hits, 12)
	if len(got) != 12 {
		t.Fatalf("应截断到 12 条，实际 %d", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].Popularity < got[i].Popularity {
			t.Errorf("结果不是热度降序: %v 后面跟了 %v", got[i-1].Popularity, got[i].Popularity)
		}
	}
	var tv, movie int
	for _, h := range got {
		if h.Kind == tmdb.KindTV {
			tv++
		} else {
			movie++
		}
	}
	if tv == 0 || movie == 0 {
		t.Errorf("两类都应出现，实际 movie=%d tv=%d", movie, tv)
	}
}

func TestSortAndTrimHitsSingleKind(t *testing.T) {
	var hits []tmdb.MetaHit
	for i := 0; i < 20; i++ {
		hits = append(hits, tmdb.MetaHit{ID: i, Kind: tmdb.KindMovie, Popularity: float64(i)})
	}
	got := sortAndTrimHits(hits, 5)
	if len(got) != 5 || got[0].Popularity != 19 {
		t.Errorf("单类型应取热度最高的前 5，实际 len=%d 首位=%v", len(got), got[0].Popularity)
	}
}

// overview 按 rune 截断，中文不会被切坏。
func TestBuildTMDBSearchRespTruncatesOverview(t *testing.T) {
	hits := []tmdb.MetaHit{{ID: 1, Kind: tmdb.KindMovie, Overview: strings.Repeat("中", 200)}}
	resp := buildTMDBSearchResp(hits, "测试", 12)
	if got := []rune(resp.Items[0].Overview); len(got) != tmdbOverviewRunes {
		t.Errorf("简介应截到 %d 个 rune，实际 %d", tmdbOverviewRunes, len(got))
	}
	if resp.Items == nil || resp.Total != 1 || resp.Query != "测试" {
		t.Errorf("响应字段不对: %+v", resp)
	}
}
