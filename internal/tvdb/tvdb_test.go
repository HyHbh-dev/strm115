package tvdb

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const seriesFixture = `{"status":"success","data":{
	"id":371028,"name":"Arcane","airsTime":"20:00","airsDays":{"friday":true,"monday":false},
	"originalNetwork":{"name":"Netflix"},"latestNetwork":{"name":""},
	"image":"https://artworks.thetvdb.com/banners/posters/1.jpg",
	"firstAired":"2021-11-06","lastAired":"2024-11-23","nextAired":"",
	"episodes":[
		{"id":1,"name":"Ep1","aired":"2026-11-05","seasonNumber":2,"episodeNumber":5,"runtime":45},
		{"id":2,"name":"Ep2","aired":"2026-11-12T20:00:00Z","seasonNumber":2,"episodeNumber":6},
		{"id":3,"name":"脏数据","aired":"","seasonNumber":2,"episodeNumber":7}
	]}}`

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c := New("test-key", "1234")
	c.Base = srv.URL
	return c
}

func TestLoginAndSeriesExtended(t *testing.T) {
	var logins atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			logins.Add(1)
			if r.Method != http.MethodPost {
				t.Errorf("login 应该是 POST，实际 %s", r.Method)
			}
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("请求体不是 JSON: %v", err)
			}
			if body["apikey"] != "test-key" || body["pin"] != "1234" {
				t.Errorf("凭据没传对: %+v", body)
			}
			_, _ = w.Write([]byte(`{"status":"success","data":{"token":"JWT-TOKEN"}}`))
		case "/series/371028/extended":
			if r.Header.Get("Authorization") != "Bearer JWT-TOKEN" {
				t.Errorf("Bearer Token 没带: %q", r.Header.Get("Authorization"))
			}
			if r.URL.Query().Get("meta") != "episodes" || r.URL.Query().Get("short") != "true" {
				t.Errorf("查询参数不对: %s", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(seriesFixture))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	})
	s, err := c.SeriesExtended(context.Background(), 371028)
	if err != nil {
		t.Fatal(err)
	}
	if s.Network() != "Netflix" || s.AirTime() != "20:00" {
		t.Errorf("电视网/时刻错: %s / %s", s.Network(), s.AirTime())
	}
	if len(s.AirsDays.Days()) != 1 || s.AirsDays.Days()[0] != "Friday" {
		t.Errorf("播出星期错: %v", s.AirsDays.Days())
	}
	if len(s.Episodes) != 3 || s.Episodes[0].Date() != "2026-11-05" || s.Episodes[1].Date() != "2026-11-12" {
		t.Errorf("分集日期错: %+v", s.Episodes)
	}
	if s.Episodes[2].Date() != "" {
		t.Error("空 aired 应该返回空日期")
	}
}

func TestTokenIsCachedAcrossCalls(t *testing.T) {
	var logins atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			_, _ = w.Write([]byte(`{"data":{"token":"T"}}`))
			return
		}
		_, _ = w.Write([]byte(seriesFixture))
	})
	for i := 0; i < 3; i++ {
		if _, err := c.SeriesExtended(context.Background(), 371028); err != nil {
			t.Fatal(err)
		}
	}
	if got := logins.Load(); got != 1 {
		t.Errorf("Token 没缓存，登录了 %d 次", got)
	}
}

func TestUnauthorizedTriggersReLogin(t *testing.T) {
	var logins, hits atomic.Int32
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			n := logins.Add(1)
			_, _ = w.Write([]byte(`{"data":{"token":"T` + string(rune('0'+n)) + `"}}`))
			return
		}
		if hits.Add(1) == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"token expired"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer T2" {
			t.Errorf("重登后没用新 Token: %q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(seriesFixture))
	})
	if _, err := c.SeriesExtended(context.Background(), 371028); err != nil {
		t.Fatalf("401 应该自动重登再试一次: %v", err)
	}
	if logins.Load() != 2 {
		t.Errorf("登录次数 = %d，应为 2", logins.Load())
	}
}

func TestNoKey(t *testing.T) {
	c := New("", "")
	if c.Configured() {
		t.Fatal("空 Key 不该算已配置")
	}
	if _, err := c.Token(context.Background()); err != ErrNoKey {
		t.Errorf("期望 ErrNoKey，实际 %v", err)
	}
}

func TestPing(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login":
			_, _ = w.Write([]byte(`{"data":{"token":"T"}}`))
		case "/languages":
			if r.Header.Get("Authorization") != "Bearer T" {
				t.Errorf("Ping 没带 Token")
			}
			_, _ = w.Write([]byte(`{"data":[{"id":1,"name":"Chinese"}]}`))
		default:
			t.Errorf("意外路径 %s", r.URL.Path)
		}
	})
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestLoginErrorIsReadable(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"status":"failure","message":"Invalid API key"}`))
	})
	_, err := c.SeriesExtended(context.Background(), 371028)
	if err == nil || !strings.Contains(err.Error(), "Invalid API key") {
		t.Errorf("错误信息该带上 TVDB 原文，实际 %v", err)
	}
}
