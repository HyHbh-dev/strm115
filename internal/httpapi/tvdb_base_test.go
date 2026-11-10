package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"strm115/internal/tvdb"
)

// tvdb_base 和 tmdb_base 同一套语义：留空 = 清空（恢复官方），不是「不修改」。
// 校验必须收下空串、拒绝没 scheme 的地址，否则 c.Base + "/login" 会拼成
// 相对路径，http.NewRequest 不报错、发出去才失败。
func TestValidTVDBBase(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"  https://tvdb.example.cc.cd/v4/  ", "https://tvdb.example.cc.cd/v4", true},
		{"http://192.168.1.5:8080/v4", "http://192.168.1.5:8080/v4", true},
		{"tvdb.example.cc.cd/v4", "", false},
		{"ftp://x/v4", "", false},
		{"https://x/v4?apikey=abc", "", false},
	}
	for _, c := range cases {
		got, err := validTVDBBase(c.in)
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

// 保存后 GET /api/settings 要回显「此刻真正生效」的接口根：
// 填了自定义就用自定义，清空就退回官方默认，不能让界面显示空串。
func TestPutSettingsTVDBBaseEcho(t *testing.T) {
	s, st := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	const custom = "https://tvdb.example.cc.cd/v4"
	rec, _ := doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tvdb_base": custom + "/"}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("保存失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "tvdb_base"); v != custom {
		t.Fatalf("没落库或没去尾斜杠: %q", v)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodGet, "/api/settings", nil, cookies)
	var resp struct {
		TVDBBase     string            `json:"tvdb_base"`
		TVDBUpstream map[string]string `json:"tvdb_upstream"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("解析设置失败: %v", err)
	}
	if resp.TVDBBase != custom || resp.TVDBUpstream["api"] != custom {
		t.Fatalf("回显不对: base=%q upstream=%v", resp.TVDBBase, resp.TVDBUpstream)
	}

	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tvdb_base": ""}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("清空失败 %d: %s", rec.Code, rec.Body.String())
	}
	if v, _ := st.GetSetting(context.Background(), "tvdb_base"); v != "" {
		t.Fatalf("空值应真的写进去（恢复官方），实际 %q", v)
	}
	rec, _ = doJSON(t, s.Handler(), http.MethodGet, "/api/settings", nil, cookies)
	resp.TVDBUpstream = nil
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.TVDBUpstream["api"] != tvdb.DefaultBase {
		t.Fatalf("清空后没回显官方默认: %v", resp.TVDBUpstream)
	}

	// 格式校验要在写库之前拦住，不能留下一个半坏的地址
	rec, _ = doJSON(t, s.Handler(), http.MethodPut, "/api/settings",
		map[string]string{"tvdb_base": "tvdb.example.cc.cd/v4"}, cookies)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("缺 scheme 应该 400，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 设置页「测试连接」要能用输入框里还没保存的接口根真打一次，
// 否则用户换完反代点测试，测的还是库里旧地址。
func TestCalendarTVDBTestUsesUnsavedBase(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch r.URL.Path {
		case "/login":
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":"T"}}`))
		case "/languages":
			_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	s, _ := newServerWithStore(t)
	cookies := loginForTest(t, s.Handler())

	rec, _ := doJSON(t, s.Handler(), http.MethodPost, "/api/calendar/tvdb/test",
		map[string]string{"api_key": "fake-key", "pin": "1234", "base": srv.URL}, cookies)
	if rec.Code != http.StatusOK {
		t.Fatalf("测试接口应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK {
		t.Fatalf("指向假服务器的接口根应该测通，实际: %s", rec.Body.String())
	}
	if hits < 2 {
		t.Fatalf("没真的打到新地址（hits=%d）", hits)
	}

	// 只改地址、不重填 Key 时也要走新地址（用库里已存的凭据）
	_ = s.st.SetSetting(context.Background(), "tvdb_api_key", "fake-key")
	hits = 0
	rec, _ = doJSON(t, s.Handler(), http.MethodPost, "/api/calendar/tvdb/test",
		map[string]string{"base": srv.URL}, cookies)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || hits < 2 {
		t.Fatalf("只改地址时没走新反代: %s (hits=%d)", rec.Body.String(), hits)
	}

	// 坏地址要在打网络之前被挡掉，并返回给人看的中文原因
	rec, _ = doJSON(t, s.Handler(), http.MethodPost, "/api/calendar/tvdb/test",
		map[string]string{"api_key": "fake", "base": "not-a-url/v4"}, cookies)
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.OK || !strings.Contains(out.Error, "http") {
		t.Fatalf("坏地址该被拒并说明原因，实际: %s", rec.Body.String())
	}
}
