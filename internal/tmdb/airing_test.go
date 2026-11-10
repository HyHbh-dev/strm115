package tmdb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func airingTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := NewClient("test-key")
	c.Base = srv.URL
	return c
}

func TestAiringOnTheAir(t *testing.T) {
	c := airingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/on_the_air" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		if r.URL.Query().Get("language") != "zh-CN" || r.URL.Query().Get("page") != "2" {
			t.Errorf("查询参数 = %s", r.URL.RawQuery)
		}
		if r.URL.Query().Get("api_key") != "test-key" {
			t.Errorf("丢了 api_key: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"page":2,"results":[
			{"id":2734,"name":"法律与秩序","original_name":"Law & Order","first_air_date":"1999-09-20",
			 "poster_path":"/a.jpg","backdrop_path":"/b.jpg","vote_average":8.0,"popularity":1448.0,
			 "overview":"剧情","origin_country":["US"]},
			{"id":0,"name":"脏数据"}]}`))
	})
	got, err := c.Airing(context.Background(), "on_the_air", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("应剔掉 id=0 的脏数据，剩 1 条，实际 %d", len(got))
	}
	_, image := CurrentUpstream()
	if !strings.HasPrefix(got[0].Poster, image+"/w342/") {
		t.Errorf("海报没走进程级图片根: %s", got[0].Poster)
	}
	if got[0].Original != "Law & Order" || got[0].Country != "US" || got[0].Rating != 8 {
		t.Errorf("字段解析错: %+v", got[0])
	}
}

func TestAiringRejectsUnknownList(t *testing.T) {
	c := NewClient("test-key")
	if _, err := c.Airing(context.Background(), "/movie/550", 1); err == nil {
		t.Fatal("任意路径应该被挡住")
	}
}

func TestShowDetail(t *testing.T) {
	c := airingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tv/100":
			_, _ = w.Write([]byte(`{"id":100,"name":"测试剧","original_name":"Test Show",
				"status":"Returning Series","first_air_date":"2020-01-01","last_air_date":"2026-12-31",
				"poster_path":"/p.jpg","backdrop_path":"/bg.jpg","overview":"简介",
				"vote_average":7.5,"popularity":99.1,"episode_run_time":[42,45],
				"networks":[{"name":"NBC"},{"name":"Peacock"}],
				"seasons":[{"season_number":0,"episode_count":3,"air_date":"2020-02-01"},
					{"season_number":2,"episode_count":8,"name":"第 2 季","air_date":"2026-10-08"}],
				"last_episode_to_air":{"season_number":2,"episode_number":5,"name":"第五集","air_date":"2026-11-05"},
				"next_episode_to_air":{"season_number":2,"episode_number":6,"name":"第六集","air_date":"2026-11-12"}}`))
		case "/tv/100/external_ids":
			_, _ = w.Write([]byte(`{"imdb_id":"tt123","tvdb_id":371028}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	})
	d, err := c.ShowDetail(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if d.TVDBID != 371028 || d.IMDb != "tt123" {
		t.Errorf("external_ids 没解析: %+v", d)
	}
	if d.Network() != "NBC" || len(d.Networks) != 2 || d.Runtime != 42 {
		t.Errorf("电视网/时长错: %s %v %d", d.Network(), d.Networks, d.Runtime)
	}
	if len(d.Seasons) != 1 || d.Seasons[0].Season != 2 {
		t.Errorf("特别篇没剔掉: %+v", d.Seasons)
	}
	if d.LastAired == nil || d.LastAired.Episode != 5 || d.NextAiring == nil || d.NextAiring.Episode != 6 {
		t.Errorf("最新/下一集错: %+v %+v", d.LastAired, d.NextAiring)
	}
}

func TestShowDetailExternalIDsMayFail(t *testing.T) {
	c := airingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/tv/100/external_ids" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"status_message":"oops"}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":100,"name":"测试剧"}`))
	})
	d, err := c.ShowDetail(context.Background(), 100)
	if err != nil {
		t.Fatalf("external_ids 失败不该拖垮详情: %v", err)
	}
	if d.TVDBID != 0 || d.Name != "测试剧" {
		t.Errorf("结果不对: %+v", d)
	}
}

func TestSeasonEpisodes(t *testing.T) {
	c := airingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/tv/100/season/2" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"episodes":[
			{"episode_number":0,"name":"特别篇"},
			{"episode_number":5,"name":"第五集","air_date":"2026-11-05","still_path":"/s.jpg",
			 "overview":"这集讲什么","runtime":42,"vote_average":8.2}]}`))
	})
	got, err := c.SeasonEpisodes(context.Background(), 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Episode != 5 || got[0].Season != 2 {
		t.Fatalf("解析错: %+v", got)
	}
	_, image := CurrentUpstream()
	if !strings.HasPrefix(got[0].Still, image+"/w1280/") {
		t.Errorf("剧照没拼图片根: %s", got[0].Still)
	}
	if got[0].Runtime != 42 || got[0].Rating != 8.2 {
		t.Errorf("元数据丢失: %+v", got[0])
	}
}

func TestMovieReleases(t *testing.T) {
	c := airingTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/discover/movie" {
			t.Errorf("路径 = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("primary_release_date.gte") != "2026-11-02" || q.Get("primary_release_date.lte") != "2026-11-08" {
			t.Errorf("日期区间丢了: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"results":[
			{"id":1,"title":"哥斯拉","release_date":"2026-11-03","poster_path":"/g.jpg"},
			{"id":2,"title":"没日期","release_date":""}]}`))
	})
	got, err := c.MovieReleases(context.Background(), "2026-11-02", "2026-11-08", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ReleaseDate != "2026-11-03" {
		t.Errorf("应剔掉无上映日的那条: %+v", got)
	}
}
