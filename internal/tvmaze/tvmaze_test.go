package tvmaze

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestScheduleParsesBothShapes(t *testing.T) {
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path+"?"+r.URL.RawQuery)
		if r.Header.Get("User-Agent") == "" {
			t.Error("缺少 User-Agent")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/schedule":
			_, _ = w.Write([]byte(`[
			  {"id":1,"name":"Pilot","airdate":"2026-11-08","airtime":"20:00",
			   "airstamp":"2026-11-09T01:00:00+00:00","season":2,"number":5,"runtime":44,
			   "show":{"id":71,"name":"Example Show",
			     "network":{"name":"NBC","country":{"timezone":"America/New_York"}},
			     "webChannel":null,
			     "externals":{"tvrage":null,"thetvdb":371028,"imdb":"tt1234567"}}}]`))
		case "/schedule/web":
			_, _ = w.Write([]byte(`[
			  {"id":2,"name":"Episode 1","airdate":"2026-11-08","airtime":"",
			   "airstamp":"2026-11-08T07:00:00+00:00","season":1,"number":1,
			   "_embedded":{"show":{"id":72,"name":"Streaming Show","network":null,
			     "webChannel":{"name":"Netflix"},
			     "externals":{"tvrage":null,"thetvdb":null,"imdb":null}}}}]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	tv, err := c.Schedule(context.Background(), "2026-11-08", "US", false)
	if err != nil || len(tv) != 1 {
		t.Fatalf("电视网排期失败：%v / %d 条", err, len(tv))
	}
	e := tv[0]
	if e.AirTime != "20:00" || e.Channel != "NBC" || e.TVDBID != 371028 ||
		e.IMDbID != "tt1234567" || e.Season != 2 || e.Number != 5 ||
		e.Timezone != "America/New_York" || e.ShowName != "Example Show" {
		t.Fatalf("字段解析不对：%+v", e)
	}
	if !strings.Contains(paths[0], "country=US") || !strings.Contains(paths[0], "date=2026-11-08") {
		t.Fatalf("查询参数不对：%s", paths[0])
	}

	web, err := c.Schedule(context.Background(), "2026-11-08", "", true)
	if err != nil || len(web) != 1 {
		t.Fatalf("流媒体排期失败：%v / %d 条", err, len(web))
	}
	if !web[0].Web || web[0].Channel != "Netflix" || web[0].ShowName != "Streaming Show" {
		t.Fatalf("_embedded.show 没解析出来：%+v", web[0])
	}
	if strings.Contains(paths[1], "country=") {
		t.Fatalf("/schedule/web 不该带 country：%s", paths[1])
	}
}

func TestScheduleRetries429AndCaches(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte(`[{"airdate":"2026-11-09","airtime":"21:30","show":{"name":"X"}}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	first, err := c.Schedule(context.Background(), "2026-11-09", "US", false)
	if err != nil || len(first) != 1 || first[0].AirTime != "21:30" {
		t.Fatalf("429 重试后应成功：%v / %+v", err, first)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("期望 2 次请求（429 + 200），实际 %d", got)
	}
	second, err := c.Schedule(context.Background(), "2026-11-09", "US", false)
	if err != nil || len(second) != 1 {
		t.Fatalf("缓存读取失败：%v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("命中缓存不该再发请求，实际 %d 次", got)
	}
}

func TestWeekCollectsAllDaysAndDegrades(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		// 400 不重试：这样总请求数可精确断言，单测也不用等退避。
		if r.URL.Path == "/schedule" && r.URL.Query().Get("date") == "2026-11-10" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`[{"airdate":"` + r.URL.Query().Get("date") +
			`","airtime":"19:00","show":{"name":"X"}}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "")
	dates := []string{"2026-11-09", "2026-11-10", "2026-11-11"}
	eps, err := c.Week(context.Background(), dates, "US")
	if err == nil {
		t.Fatal("有一天失败时应返回 firstErr")
	}
	if len(eps) != 5 {
		t.Fatalf("6 个请求里 1 个失败，应剩 5 条，实际 %d", len(eps))
	}
	if got := calls.Load(); got != 6 {
		t.Fatalf("3 天 × 2 端点 = 6 次请求，实际 %d", got)
	}
}

func TestNormalizeShow(t *testing.T) {
	cases := map[string]string{
		"Marvel's Daredevil": "marvelsdaredevil",
		"怪奇物语 4":             "怪奇物语4",
		"Sherlock (2010)":    "sherlock2010",
	}
	for in, want := range cases {
		if got := NormalizeShow(in); got != want {
			t.Errorf("NormalizeShow(%q) = %q, want %q", in, got, want)
		}
	}
}
