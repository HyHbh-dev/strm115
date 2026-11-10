package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 演员表这条链路有三个独立出错点，每个都单独验：
// 用 IMDb 换 TMDB id 的路径、电影与剧集的字段差异、头像缺失。
func TestFindAndCredits(t *testing.T) {
	var gotPaths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		if r.URL.Query().Get("api_key") == "" {
			t.Errorf("请求里没有 api_key: %s", r.URL.String())
		}
		switch r.URL.Path {
		case "/find/tt1567937":
			if r.URL.Query().Get("external_source") != "imdb_id" {
				t.Errorf("find 必须带 external_source=imdb_id")
			}
			_, _ = w.Write([]byte(`{"movie_results":[{"id":1567937}],"tv_results":[]}`))
		case "/find/tt999":
			_, _ = w.Write([]byte(`{"movie_results":[],"tv_results":[]}`))
		case "/find/tt555":
			_, _ = w.Write([]byte(`{"movie_results":[],"tv_results":[{"id":4242}]}`))
		case "/movie/1567937/credits":
			if lang := r.URL.Query().Get("language"); lang != DefaultLanguage {
				t.Errorf("演员表要中文，实际 language=%q", lang)
			}
			_, _ = w.Write([]byte(`{"cast":[
				{"id":1,"name":"朱一龙","character":"Wan Ziqiang","profile_path":"/a.jpg","order":0},
				{"id":2,"name":"梁家辉","character":"","profile_path":null,"order":1},
				{"id":3,"name":"朱一龙","character":"重复项","profile_path":"/b.jpg","order":2},
				{"id":4,"name":"","character":"空名","profile_path":"/c.jpg","order":3}
			],"crew":[
				{"name":"韩延","job":"Director"},
				{"name":"某摄影","job":"Director of Photography"},
				{"name":"韩延","job":"Director"},
				{"name":"甲","job":"Director"},{"name":"乙","job":"Director"},
				{"name":"丙","job":"Director"}
			]}`))
		case "/tv/4242/aggregate_credits":
			// 剧集的角色在 roles[0].character 里，直接读 character 会全是空的。
			_, _ = w.Write([]byte(`{"cast":[
				{"id":9,"name":"张三","character":"","profile_path":"/d.jpg",
				 "roles":[{"character":"老张"},{"character":"老张（青年）"}]}
			],"crew":[{"name":"王某","jobs":[{"job":"Creator"}]},
				{"name":"某导演一","jobs":[{"job":"Director"}]},
				{"name":"某导演二","jobs":[{"job":"Director"}]}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient("k")
	c.Base = srv.URL
	ctx := context.Background()

	movieID, tvID, err := c.Find(ctx, "tt1567937")
	if err != nil || movieID != 1567937 || tvID != 0 {
		t.Fatalf("应该反查到电影：movie=%d tv=%d err=%v", movieID, tvID, err)
	}
	kind, id := KindMovie, movieID

	cr, err := c.Credits(ctx, kind, id, 15)
	if err != nil {
		t.Fatal(err)
	}
	cast := cr.Cast
	if len(cast) != 2 {
		t.Fatalf("重名和空名要去掉，期望 2 条，实际 %d: %+v", len(cast), cast)
	}
	if cast[0].Name != "朱一龙" || cast[0].Role != "Wan Ziqiang" {
		t.Errorf("第一条不对：%+v", cast[0])
	}
	if !strings.HasPrefix(cast[0].Image, ImageBase+"/"+ProfileSize+"/") {
		t.Errorf("头像要拼成完整地址，实际 %q", cast[0].Image)
	}
	// profile_path 为 null 时不能拼出一个 ".../w185" 这种必然 404 的地址。
	if cast[1].Image != "" {
		t.Errorf("没有 profile_path 时头像应为空，实际 %q", cast[1].Image)
	}

	// 只留导演（摄影不算）、去重、最多 3 个。
	if len(cr.Directors) != 3 || cr.Directors[0] != "韩延" {
		t.Errorf("导演应去重并截到 3 个，实际 %v", cr.Directors)
	}

	// IMDb 里查不到：要给一句人话，而不是空结果。
	if _, _, err := c.Find(ctx, "tt999"); err == nil {
		t.Error("查不到时应该报错")
	}

	// 剧集走 aggregate_credits。
	movieID, tvID, err = c.Find(ctx, "tt555")
	if err != nil || movieID != 0 || tvID != 4242 {
		t.Fatalf("应该反查到剧集：movie=%d tv=%d err=%v", movieID, tvID, err)
	}
	kind, id = KindTV, tvID
	cr, err = c.Credits(ctx, kind, id, 15)
	if err != nil || len(cr.Cast) != 1 {
		t.Fatalf("剧集演员表异常：%+v err=%v", cr, err)
	}
	if cr.Cast[0].Role != "老张" {
		t.Errorf("剧集角色名要从 roles[0].character 取，实际 %q", cr.Cast[0].Role)
	}
	// 剧集有 Creator 时只用 Creator：aggregate_credits 的 Director 是按集
	// 合并的，一部剧能有八九个「导演」，全写进 nfo 明显不对。
	if len(cr.Directors) != 1 || cr.Directors[0] != "王某" {
		t.Errorf("剧集应优先取 Creator，实际 %v", cr.Directors)
	}
}

// 没配 Key 时所有调用都应该返回 ErrNoKey，调用方据此静默降级。
func TestCreditsNoKey(t *testing.T) {
	c := NewClient("")
	if _, _, err := c.Find(context.Background(), "tt1"); err != ErrNoKey {
		t.Errorf("期望 ErrNoKey，实际 %v", err)
	}
	if _, err := c.Credits(context.Background(), KindMovie, 1, 5); err != ErrNoKey {
		t.Errorf("期望 ErrNoKey，实际 %v", err)
	}
}

// limit 要生效：Emby 的演员横排只显示前十几个，多取是白白拉长 nfo。
func TestCreditsLimit(t *testing.T) {
	body := `{"cast":[`
	for i := 0; i < 30; i++ {
		if i > 0 {
			body += ","
		}
		body += `{"id":` + itoa(i+1) + `,"name":"演员` + itoa(i+1) + `","character":"c","profile_path":"/p.jpg"}`
	}
	body += `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient("k")
	c.Base = srv.URL
	cr, err := c.Credits(context.Background(), KindMovie, 1, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Cast) != 5 {
		t.Fatalf("limit=5 应该只返回 5 条，实际 %d", len(cr.Cast))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
