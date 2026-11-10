package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 订阅判断「有没有新集」全靠 TV()，这条链路的四个判断都要单独立住：
// 季/集解析、特别篇剔除、last_episode_to_air 缺失时的兜底、IMDb 补全失败不影响主结果。
func TestTV(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("api_key") == "" {
			t.Errorf("请求里没有 api_key: %s", r.URL.String())
		}
		switch r.URL.Path {
		case "/tv/100":
			if lang := r.URL.Query().Get("language"); lang != DefaultLanguage {
				t.Errorf("剧集详情要中文，实际 language=%q", lang)
			}
			// season 0 是特别篇，必须被剔掉。集数口径混进特别篇会让水位线跳错。
			_, _ = w.Write([]byte(`{
				"id":100,"name":"赛博朋克：边缘行者","original_name":"Cyberpunk: Edgerunners",
				"first_air_date":"2022-09-13","status":"Ended","number_of_episodes":10,
				"seasons":[
					{"season_number":0,"episode_count":2,"name":"特别篇","air_date":"2022-09-01"},
					{"season_number":1,"episode_count":10,"name":"第 1 季","air_date":"2022-09-13"}
				],
				"last_episode_to_air":{"season_number":1,"episode_number":10,
					"name":"我本凡人","air_date":"2022-09-13"},
				"next_episode_to_air":null
			}`))
		case "/tv/100/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt12590266","tvdb_id":4242}`))
		case "/tv/200":
			// 老剧没填 last_episode_to_air，只能从 seasons 推。
			_, _ = w.Write([]byte(`{
				"id":200,"name":"某老剧","first_air_date":"1998-03-01","status":"Returning Series",
				"number_of_episodes":40,
				"seasons":[{"season_number":1,"episode_count":10},
				           {"season_number":2,"episode_count":18},
				           {"season_number":3,"episode_count":6}],
				"last_episode_to_air":null
			}`))
		case "/tv/200/external_ids":
			http.NotFound(w, r) // IMDb 查不到不算致命
		case "/tv/300":
			// 未开播：集数一个都没有，Latest 必须返回零值而不是 E00。
			_, _ = w.Write([]byte(`{"id":300,"name":"还没播","first_air_date":"2027-01-01",
				"status":"Planned","number_of_episodes":0,"seasons":[]}`))
		case "/tv/300/external_ids":
			_, _ = w.Write([]byte(`{}`))
		case "/tv/100/season/1":
			_, _ = w.Write([]byte(`{"episodes":[
				{"episode_number":1,"name":"第一集","air_date":"2022-09-13"},
				{"episode_number":2,"name":"第二集","air_date":"2022-09-13"},
				{"episode_number":0,"name":"花絮","air_date":"2022-09-13"}
			]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	c := NewClient("k")
	c.Base = srv.URL
	ctx := context.Background()

	// 1. 正常剧集
	show, err := c.TV(ctx, 100)
	if err != nil {
		t.Fatalf("TV(100) 报错: %v", err)
	}
	if show.Name != "赛博朋克：边缘行者" || show.OriginalName != "Cyberpunk: Edgerunners" {
		t.Fatalf("剧名解析错了: %+v", show)
	}
	if show.Year != 2022 {
		t.Fatalf("年份应为 2022，实际 %d", show.Year)
	}
	if show.IMDb != "tt12590266" {
		t.Fatalf("IMDb 应为 tt12590266，实际 %q", show.IMDb)
	}
	if len(show.Seasons) != 1 || show.Seasons[0].Season != 1 {
		t.Fatalf("特别篇没被剔掉: %+v", show.Seasons)
	}
	if !show.Ended {
		t.Fatalf("status=Ended 时 Ended 应为 true")
	}
	if show.LastAired == nil || show.LastAired.Season != 1 || show.LastAired.Episode != 10 {
		t.Fatalf("last_episode_to_air 解析错了: %+v", show.LastAired)
	}
	if !show.HasAired(1, 10) || !show.HasAired(1, 3) {
		t.Fatalf("已播的集应该 HasAired=true")
	}
	if show.HasAired(2, 1) {
		t.Fatalf("第 2 季还没播，HasAired 不该为 true")
	}
	if show.HasAired(1, 11) {
		t.Fatalf("E11 不存在，HasAired 不该为 true")
	}

	// 2. 没有 last_episode_to_air 的老剧：按最大季推
	older, err := c.TV(ctx, 200)
	if err != nil {
		t.Fatalf("TV(200) 报错: %v", err)
	}
	latest := older.Latest()
	if latest.Season != 3 || latest.Episode != 6 {
		t.Fatalf("兜底应取第 3 季的 6 集，实际 %+v", latest)
	}
	if older.IMDb != "" {
		t.Fatalf("external_ids 404 时 IMDb 应为空，实际 %q", older.IMDb)
	}
	if !older.HasAired(3, 6) || older.HasAired(3, 7) {
		t.Fatalf("兜底水位线比较错了")
	}

	// 3. 未开播：Latest 零值，不能当成 E00 去搜
	planned, err := c.TV(ctx, 300)
	if err != nil {
		t.Fatalf("TV(300) 报错: %v", err)
	}
	if l := planned.Latest(); l.Episode != 0 || l.Season != 0 {
		t.Fatalf("未开播的剧 Latest 应为零值，实际 %+v", l)
	}
	if planned.HasAired(1, 1) {
		t.Fatalf("未开播的剧不该有任何已播集")
	}

	// 4. 分集清单：episode_number 为 0 的特别内容要剔掉
	eps, err := c.Episodes(ctx, 100, 1)
	if err != nil {
		t.Fatalf("Episodes 报错: %v", err)
	}
	if len(eps) != 2 || eps[0].Episode != 1 || eps[1].Episode != 2 {
		t.Fatalf("分集解析错了: %+v", eps)
	}

	// 5. 非法 id 应该在本地就挡掉，不发请求
	if _, err := c.TV(ctx, 0); err == nil {
		t.Fatalf("id=0 应该报错")
	}
}
