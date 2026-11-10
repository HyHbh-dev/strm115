package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 搜索结果行点进详情时只带一个粗提的片名，没有 type。
//
// 以前后端默认按「剧集」去 TMDB 认片：电影要么搜不到详情，要么被认成剧集，
// 接着详情里的「订阅」就会把电影建成一条剧集订阅。现在没给 type 时电影、
// 剧集各认一次，并用命中的真实类型回填。
func TestTitleByBareNameKeepsMovieKind(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet,
		"/api/title?q=%E6%B2%99%E4%B8%982&year=2024", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("按片名认片失败 %d: %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Type   string `json:"type"`
		TMDbID int    `json:"tmdb_id"`
		Title  string `json:"title"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "movie" || d.TMDbID != 693134 {
		t.Fatalf("电影不该被认成剧集，实际 %+v（%s）", d, rec.Body.String())
	}
	if d.Title != "沙丘2" {
		t.Errorf("标题应由 TMDB 校正为「沙丘2」，实际 %q", d.Title)
	}
}

// 明确给了 type 时仍只认那一类：老链接 /?type=tv&tmdb_id= 行为不变。
func TestTitleExplicitTypeUnchanged(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet,
		"/api/title?type=tv&tmdb_id=1396", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("按 id 查详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var d struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Type != "tv" {
		t.Errorf("明确给了 type=tv 就该还是 tv，实际 %q", d.Type)
	}
}
