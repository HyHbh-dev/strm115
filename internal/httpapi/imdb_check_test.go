package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/imdb"
)

// fakeSuggestion 模拟免 Key 的自动建议接口：中文查询直接回一条英文结果。
func fakeSuggestion(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"d":[{"id":"tt15239678","l":"Dune: Part Two","y":2024,"qid":"movie"}],"q":"x","v":1}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestIMDbProbeSuggestion 没配 Key 时走免 Key 接口，检测应通过并换出英文名。
func TestIMDbProbeSuggestion(t *testing.T) {
	srv := fakeSuggestion(t)
	old := imdb.SuggestionAPIEndpoint
	imdb.SuggestionAPIEndpoint = srv.URL + "/suggestion/%s/%s.json"
	defer func() { imdb.SuggestionAPIEndpoint = old }()

	got := imdbProbe("", "")
	if !got.OK {
		t.Fatalf("免 Key 接口应该可用，实际失败: %s", got.Error)
	}
	if got.Mode != "suggestion" || got.KeySet {
		t.Errorf("应判定为免 Key 模式，实际 mode=%q key_set=%v", got.Mode, got.KeySet)
	}
	if got.English != "Dune: Part Two 2024" {
		t.Errorf("中文名应换出英文名，实际 %q", got.English)
	}
}

// TestIMDbProbeBadKeyFallsBack 填了坏 Key：官方接口 401，检测要如实报 Key 不可用，
// 同时说明免 Key 那条还能用 —— 这正是「填错 Key 不至于让搜索整条挂掉」的依据。
func TestIMDbProbeBadKeyFallsBack(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"message":"invalid api key"}`))
	}))
	defer bad.Close()
	sug := fakeSuggestion(t)

	oldOfficial, oldSug := imdb.OfficialAPIEndpoint, imdb.SuggestionAPIEndpoint
	imdb.OfficialAPIEndpoint = bad.URL
	imdb.SuggestionAPIEndpoint = sug.URL + "/suggestion/%s/%s.json"
	defer func() {
		imdb.OfficialAPIEndpoint = oldOfficial
		imdb.SuggestionAPIEndpoint = oldSug
	}()

	got := imdbProbe("bad-key", "")
	if !got.OK {
		t.Errorf("免 Key 兜底可用时整体应判为可用，实际 %s", got.Error)
	}
	if got.Mode != "suggestion" {
		t.Errorf("降级后 mode 应为 suggestion，实际 %q", got.Mode)
	}
	if !got.KeySet {
		t.Error("KeySet 应保持 true，用户才知道自己填过 Key")
	}
	if !strings.Contains(got.Note, "Key 不可用") {
		t.Errorf("Note 应说明 Key 不可用，实际 %q", got.Note)
	}
	if got.English == "" {
		t.Error("降级后仍应换出英文名")
	}
}

// TestIMDbProbeOfficial 官方接口正常时应该报 official，而不是 suggestion。
func TestIMDbProbeOfficial(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "good-key" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"titleSearch":{"results":[
			{"id":"tt15239678","titleText":{"text":"Dune: Part Two"},
			 "releaseYear":{"year":2024},"titleType":{"text":"movie"}}]}}}`))
	}))
	defer srv.Close()

	old := imdb.OfficialAPIEndpoint
	imdb.OfficialAPIEndpoint = srv.URL
	defer func() { imdb.OfficialAPIEndpoint = old }()

	got := imdbProbe("good-key", "")
	if !got.OK || got.Mode != "official" {
		t.Fatalf("官方接口正常时应报 official，实际 ok=%v mode=%q err=%q", got.OK, got.Mode, got.Error)
	}
	if got.English != "Dune: Part Two 2024" {
		t.Errorf("英文名解析错: %q", got.English)
	}
}
