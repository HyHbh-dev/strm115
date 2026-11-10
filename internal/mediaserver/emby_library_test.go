package mediaserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// 这组测试守的是两个在真机上踩出来的规矩：
//
//  1. Emby 4.10.2 的「混合内容(mixed)」库扫不到 .strm —— 目录全建好、影片一条不出、
//     也不报错。建成 movies 类型，同一个目录 25 个文件全部入库。所以 mixed 必须降级。
//  2. 建库时必须 refreshLibrary=true —— 建库那一下是 Emby 唯一一次真正递归读目录、
//     把 .strm 变成影片的时机；用 false 建，之后点多少次「扫描媒体库」都只补目录。
func TestEmbyCollectionType(t *testing.T) {
	cases := map[string]string{
		"movies":  "movies",
		"tvshows": "tvshows",
		"mixed":   "movies", // 关键：mixed 不能用
		"":        "movies",
		"乱填":      "movies",
	}
	for in, want := range cases {
		if got := EmbyCollectionType(in); got != want {
			t.Errorf("EmbyCollectionType(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// libFake 只实现建库要用的两个接口。
type libFake struct {
	mu      sync.Mutex
	created bool
	query   map[string]string
}

func (f *libFake) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/Library/VirtualFolders", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if !f.created {
				writeTestJSON(w, 200, []any{})
				return
			}
			writeTestJSON(w, 200, []any{map[string]any{
				"Name": "电影", "ItemId": "3", "CollectionType": f.query["collectionType"],
				"Locations": []string{"/media/movies"},
			}})
		case http.MethodPost:
			q := r.URL.Query()
			f.query = map[string]string{
				"name":           q.Get("name"),
				"collectionType": q.Get("collectionType"),
				"refreshLibrary": q.Get("refreshLibrary"),
				"paths":          q.Get("paths"),
			}
			f.created = true
			w.WriteHeader(http.StatusNoContent)
		}
	})
	return mux
}

func TestEnsureLibraryUsesMoviesAndRefreshes(t *testing.T) {
	f := &libFake{}
	srv := httptest.NewServer(f.handler())
	defer srv.Close()

	e := newEmby(Config{Kind: "emby", URL: srv.URL, APIKey: "k"})
	lib, err := e.EnsureLibrary(context.Background(), "电影", "mixed", []string{"/media/movies"})
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}

	if f.query["collectionType"] != "movies" {
		t.Errorf("collectionType = %q，mixed 必须降级成 movies", f.query["collectionType"])
	}
	if f.query["refreshLibrary"] != "true" {
		t.Errorf("refreshLibrary = %q，必须是 true（否则永远扫不出片）", f.query["refreshLibrary"])
	}
	if f.query["paths"] != "/media/movies" {
		t.Errorf("paths = %q", f.query["paths"])
	}
	if lib.ID != "3" {
		t.Errorf("没回读到 ItemId，实际 %q", lib.ID)
	}
}

// 字幕重下后要告诉 Emby「这两个文件改过了」。这个接口的名字容易误导
// （文档写的是 new movies），但收的就是任意文件路径；请求体必须是
// {"Updates":[{"Path":...,"UpdateType":"Modified"}]}，且不能重复或夹空串。
func TestEmbyMediaUpdated(t *testing.T) {
	var mu sync.Mutex
	var body string
	var token string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/Library/Media/Updated" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		body, token = string(b), r.Header.Get("X-Emby-Token")
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	e := newEmby(Config{Kind: "emby", URL: srv.URL, APIKey: "k"})
	if err := e.MediaUpdated(context.Background(), []string{
		"/media/movies/A/a.strm", "/media/movies/A/a.zh.srt",
		"/media/movies/A/a.strm", "  ", "",
	}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	got, gotToken := body, token
	mu.Unlock()
	if gotToken != "k" {
		t.Fatalf("鉴权头没带上: %q", gotToken)
	}
	var req struct {
		Updates []struct {
			Path       string `json:"Path"`
			UpdateType string `json:"UpdateType"`
		} `json:"Updates"`
	}
	if err := json.Unmarshal([]byte(got), &req); err != nil {
		t.Fatalf("请求体不是合法 JSON: %q %v", got, err)
	}
	if len(req.Updates) != 2 {
		t.Fatalf("应去重且丢掉空路径，实际 %d 条: %+v", len(req.Updates), req.Updates)
	}
	for _, u := range req.Updates {
		if u.UpdateType != "Modified" {
			t.Errorf("UpdateType = %q，改字幕应该是 Modified", u.UpdateType)
		}
	}
	// 空列表不该发请求
	if err := e.MediaUpdated(context.Background(), nil); err != nil {
		t.Fatalf("空列表应直接返回: %v", err)
	}
}
