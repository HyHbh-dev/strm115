package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"

	"strm115/internal/tmdb"
)

// 订阅弹窗里点某一集，接口要回这一集的集名 / 播出日 / 简介 / 时长 / 剧照。
func TestTMDBEpisodeDetail(t *testing.T) {
	s, st := newServerWithStore(t)
	fakeCatalogTMDB(t, s, st, "")
	h := s.Handler()
	cookies := loginForTest(t, h)

	rec, _ := doJSON(t, h, http.MethodGet,
		"/api/tmdb/episode?tmdb_id=1396&season=1&episode=1", nil, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("单集详情失败 %d: %s", rec.Code, rec.Body.String())
	}
	var e tmdb.SeasonEpisode
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if e.Season != 1 || e.Episode != 1 || e.Name != "试播集" ||
		e.AirDate != "2008-01-20" || e.Runtime != 47 || e.Overview == "" {
		t.Fatalf("单集字段没带全：%+v", e)
	}
	if e.Still == "" {
		t.Fatal("剧照应该拼成完整 URL")
	}

	// 没有的集号要明确 404，不能回一个空对象让前端显示「第 N 集」。
	missing, _ := doJSON(t, h, http.MethodGet,
		"/api/tmdb/episode?tmdb_id=1396&season=1&episode=99", nil, cookies)
	if missing.Code != http.StatusNotFound {
		t.Fatalf("不存在的集应 404，实际 %d", missing.Code)
	}
	// 参数校验。
	bad, _ := doJSON(t, h, http.MethodGet, "/api/tmdb/episode?tmdb_id=1396", nil, cookies)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("缺参数应 400，实际 %d", bad.Code)
	}
}
