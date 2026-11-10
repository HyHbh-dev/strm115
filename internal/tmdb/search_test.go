package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用假服务器顶掉 TMDB 上游，单测不碰外网也不依赖反代配置。
func fakeSearchServer(t *testing.T, body string) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/multi" {
			t.Errorf("路径不对：%s", r.URL.Path)
		}
		if r.URL.Query().Get("query") == "" {
			t.Error("没带 query")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	c := NewClient("fake-key")
	c.Base = srv.URL
	return c, srv.Close
}

func TestSearchOriginalMovie(t *testing.T) {
	body := `{"results":[
      {"media_type":"movie","title":"沙丘2","original_title":"Dune: Part Two","release_date":"2024-02-27"},
      {"media_type":"movie","title":"沙丘","original_title":"Dune","release_date":"1984-12-14"}
    ]}`
	c, done := fakeSearchServer(t, body)
	defer done()
	hit, err := c.SearchOriginal(context.Background(), "沙丘2")
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Name != "Dune: Part Two" || hit.Year != "2024" {
		t.Fatalf("换名结果不对：%+v", hit)
	}
}

func TestSearchOriginalTVAndYear(t *testing.T) {
	body := `{"results":[
      {"media_type":"tv","name":"三体","original_name":"3 Body Problem","first_air_date":"2024-03-21"},
      {"media_type":"tv","name":"三体","original_name":"Three-Body","first_air_date":"2023-01-15"}
    ]}`
	c, done := fakeSearchServer(t, body)
	defer done()
	hit, err := c.SearchOriginal(context.Background(), "三体 2023")
	if err != nil {
		t.Fatal(err)
	}
	// 查询里带年份时优先同年那条
	if hit == nil || !strings.Contains(hit.Name, "Three-Body") || hit.Year != "2023" {
		t.Fatalf("没按年份挑剧集：%+v", hit)
	}
}

func TestSearchOriginalSkipsChineseOriginal(t *testing.T) {
	body := `{"results":[
      {"media_type":"movie","title":"流浪地球","original_title":"流浪地球","release_date":"2019-02-05"},
      {"media_type":"movie","title":"流浪地球","original_title":"The Wandering Earth","release_date":"2019-02-05"}
    ]}`
	c, done := fakeSearchServer(t, body)
	defer done()
	hit, err := c.SearchOriginal(context.Background(), "流浪地球")
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Name != "The Wandering Earth" {
		t.Fatalf("中文原名不该被当成英文名：%+v", hit)
	}
}

// 华语片的 original_title 本来就是中文，第一遍（zh-CN）挑不出拉丁名，
// 必须换 en-US 再查一次拿英文译名；且「流浪地球」要优先精确匹配原名那条，
// 不能把续集 "The Wandering Earth II" 当成第一部。
func TestSearchOriginalFallsBackToEnglishLocale(t *testing.T) {
	var langs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lang := r.URL.Query().Get("language")
		langs = append(langs, lang)
		if lang == "en-US" {
			_, _ = w.Write([]byte(`{"results":[
              {"media_type":"movie","title":"The Wandering Earth II","original_title":"流浪地球2","release_date":"2023-01-22"},
              {"media_type":"movie","title":"The Wandering Earth","original_title":"流浪地球","release_date":"2019-02-05"}
            ]}`))
			return
		}
		_, _ = w.Write([]byte(`{"results":[
          {"media_type":"movie","title":"流浪地球","original_title":"流浪地球","release_date":"2019-02-05"}
        ]}`))
	}))
	defer srv.Close()
	c := NewClient("fake-key")
	c.Base = srv.URL

	hit, err := c.SearchOriginal(context.Background(), "流浪地球")
	if err != nil {
		t.Fatal(err)
	}
	if hit == nil || hit.Name != "The Wandering Earth" || hit.Year != "2019" {
		t.Fatalf("英文译名不对：%+v", hit)
	}
	if len(langs) != 2 || langs[0] != DefaultLanguage || langs[1] != "en-US" {
		t.Fatalf("应该查两遍且语言顺序固定，实际 %v", langs)
	}
}

// 回归：TMDB 搜「三体」会捎回一部 2023 年的法国纪录片 Notre corps，
// 之前因为它有拉丁原名且年份对得上，被误当成《三体》的英文名。
func TestSearchOriginalRejectsUnrelated(t *testing.T) {
	body := `{"results":[
      {"media_type":"movie","title":"某部纪录片","original_title":"Notre corps","release_date":"2023-05-10"}
    ]}`
	c, done := fakeSearchServer(t, body)
	defer done()
	hit, err := c.SearchOriginal(context.Background(), "三体 2023")
	if err != nil {
		t.Fatal(err)
	}
	if hit != nil {
		t.Fatalf("不相关的结果不该被当成英文名：%+v", hit)
	}
}

func TestYearInQuery(t *testing.T) {
	for in, want := range map[string]string{"三体 2023": "2023", "Dune 2024 1080p": "2024", "沙丘2": "", "1999 年": "1999"} {
		if got := yearInQuery(in); got != want {
			t.Errorf("yearInQuery(%q) = %q，想要 %q", in, got, want)
		}
	}
}
