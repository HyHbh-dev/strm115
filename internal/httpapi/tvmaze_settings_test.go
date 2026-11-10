package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/tvmaze"
)

// tvmaze_base 和 tmdb_base / tvdb_base 同一套语义：留空 = 恢复官方公共 API。
func TestValidTVMazeBase(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  https://tvmaze.example.cc.cd/  ", "https://tvmaze.example.cc.cd", true},
		{"tvmaze.example.cc.cd", "", false},
		{"https://x/api?key=1", "", false},
	}
	for _, c := range cases {
		got, err := validTVMazeBase(c.in)
		if c.ok && err != nil {
			t.Fatalf("%q 应该合法，实际报错 %v", c.in, err)
		}
		if !c.ok && err == nil {
			t.Fatalf("%q 应该被拒绝，实际放行了（%q）", c.in, got)
		}
		if c.ok && got != c.want {
			t.Fatalf("%q 规整成 %q，期望 %q", c.in, got, c.want)
		}
	}
}

// calendar_source 留空要兜成 tvmaze，非法值必须 400 而不是静默少一列时刻。
func TestCalendarSourceSettings(t *testing.T) {
	s, st := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodGet, "/api/settings", nil, cookies)
	var resp struct {
		Source         string            `json:"calendar_source"`
		TVmazeUpstream map[string]string `json:"tvmaze_upstream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Source != "tvmaze" || resp.TVmazeUpstream["api"] != tvmaze.DefaultBase {
		t.Fatalf("默认应回显 tvmaze + 官方地址：%+v", resp)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"calendar_source": "TVDB", "tvmaze_base": "https://proxy.example/tvmaze/"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "calendar_source"); v != "tvdb" {
		t.Fatalf("大小写没归一化: %q", v)
	}
	if v, _ := st.GetSetting(context.Background(), "tvmaze_base"); v != "https://proxy.example/tvmaze" {
		t.Fatalf("尾斜杠没去掉: %q", v)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"calendar_source": "bogus"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法数据源应 400，实际 %d: %s", rec.Code, rec.Body.String())
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"calendar_source": "", "tvmaze_base": ""}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "calendar_source"); v != "tvmaze" {
		t.Fatalf("空值应兜成 tvmaze: %q", v)
	}
}

// 测试按钮要用输入框里还没保存的接口根真打一次，坏地址要在打网络前挡掉。
func TestCalendarTVMazeTestUsesUnsavedBase(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/schedule" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`[{"airdate":"2026-11-08","airtime":"20:00","season":1,"number":1,
		  "show":{"id":1,"name":"X","network":{"name":"NBC"}}}]`))
	}))
	defer srv.Close()

	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodPost, "/api/calendar/tvmaze/test",
		map[string]string{"base": srv.URL}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("测试接口应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK       bool   `json:"ok"`
		Count    int    `json:"count"`
		Endpoint string `json:"endpoint"`
		Error    string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.Count != 1 || out.Endpoint != srv.URL || hits == 0 {
		t.Fatalf("没打到未保存的新地址：%s (hits=%d)", rec.Body.String(), hits)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodPost, "/api/calendar/tvmaze/test",
		map[string]string{"base": "not-a-url"}, cookies)
	out.OK = true
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || !strings.Contains(out.Error, "http") {
		t.Fatalf("坏地址该被拒并说明原因，实际: %s", rec.Body.String())
	}
}
