package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"strm115/internal/calendar"
	"strm115/internal/douban"
	"strm115/internal/tvdb"
	"strm115/internal/tvmaze"
)

// handleCalendar 返回一周的播出日历。
//
// 冷启动要并发打几十次 TMDB（剧单 → 详情 → 分季），给 60 秒；
// 之后整周走缓存，基本是毫秒级。refresh=1 才强制重抓。
func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	start, err := calendar.ParseStart(r.URL.Query().Get("start"), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	refresh := r.URL.Query().Get("refresh")
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	week, err := s.cal.Week(ctx, start, refresh == "1" || refresh == "true")
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, week)
}

// calendarDoubanResp 是 /api/calendar/douban 的返回形状。
// movies 是带 date 的日历 Entry，前端据此并进对应那天的周历格子；
// tv 是国产剧进度列表，单独成栏（豆瓣不给逐集播出日）。
type calendarDoubanResp struct {
	Enabled bool             `json:"enabled"`
	OK      bool             `json:"ok"`
	Error   string           `json:"error,omitempty"`
	TV      []douban.Item    `json:"tv"`
	Movies  []calendar.Entry `json:"movies"`
}

// handleCalendarDouban 单独返回这一周的豆瓣数据（国产剧 + 国内档期电影）。
//
// 为什么拆成独立接口：豆瓣 4 个榜单串行 + minInterval 节流 + 最长 12s 超时，
// 同步塞进 /api/calendar 会把整张周历拖到十几秒。拆开后前端先渲染 TMDB
// 主体，豆瓣慢/挂/超时只影响「国产剧」那一栏，页面不再转圈等它。
//
// 整段给 5 秒上限：宁可这一栏空着（返回 ok=false + error），也不让用户干等
// 豆瓣。calendar.DoubanWeek 内部还有一道更短的共享超时，两道闸门保证不会被
// 单个挂住的请求拖死。
func (s *Server) handleCalendarDouban(w http.ResponseWriter, r *http.Request) {
	start, err := calendar.ParseStart(r.URL.Query().Get("start"), time.Now())
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	block, movies := s.cal.DoubanWeek(ctx, start)
	// 统一给空数组而不是 null：前端可以直接 .length / for-of，不用到处判空。
	if block.TV == nil {
		block.TV = []douban.Item{}
	}
	if movies == nil {
		movies = []calendar.Entry{}
	}
	writeJSON(w, http.StatusOK, calendarDoubanResp{
		Enabled: block.Enabled,
		OK:      block.OK,
		Error:   block.Error,
		TV:      block.TV,
		Movies:  movies,
	})
}

// handleCalendarTVMazeTest 真打一次当日 /schedule，给设置页「测试连接」用。
//
// 输入框里还没保存的接口根优先（和 TVDB 测试按钮同一套语义）；
// 代理跟全局 net_proxy 走，直连不通的环境不用为 TVMaze 单独配一份。
func (s *Server) handleCalendarTVMazeTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Base *string `json:"base"`
	}
	_ = readJSON(r, &body) // 请求体可空
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	base := ""
	if body.Base != nil {
		base = strings.TrimSpace(*body.Base)
	} else {
		base = strings.TrimSpace(s.setting(ctx, "tvmaze_base"))
	}
	if base != "" {
		norm, err := validTVMazeBase(base)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "TVMaze " + err.Error()})
			return
		}
		base = norm
	}

	// 走 netProxy 而不是直接读设置：环境变量部署时也要跟着代理。
	c := tvmaze.New(base, s.netProxy(ctx))
	started := time.Now()
	n, err := c.Ping(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"message":  fmt.Sprintf("TVMaze 连通：当日 %d 条排期，%d ms", n, time.Since(started).Milliseconds()),
		"count":    n,
		"endpoint": c.Endpoint(),
	})
}

// handleCalendarTVDBTest 用 Key / PIN 真连一次 TVDB。
//
// 输入框里刚填、还没保存的凭据优先：否则用户填完点「测试」测的还是库里旧的
// 那份，会误以为新 Key 不对。没传就用库里已存的。
// 用 200 + ok 字段而不是把失败变成 4xx：设置页要显示「失败原因」这一行，
// 而不是被 http 客户端统一弹成一个错误 toast。
func (s *Server) handleCalendarTVDBTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		APIKey string `json:"api_key"`
		PIN    string `json:"pin"`
		// 指针是为了区分「没传」和「传了空串」：
		// 传空串 = 用户把接口根清空了，要按官方地址测；没传才读库里那份。
		Base *string `json:"base"`
	}
	_ = readJSON(r, &body) // 请求体可空
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	// 和 Key / PIN 一样，接口根也优先用输入框里还没保存的那份：
	// 用户改完地址点「测试连接」，测的得是刚填的地址而不是库里旧的。
	base := ""
	if body.Base != nil {
		base = strings.TrimSpace(*body.Base)
	} else {
		base = strings.TrimSpace(s.setting(ctx, "tvdb_base"))
	}
	if base != "" {
		norm, e := validTVDBBase(base)
		if e != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "TVDB " + e.Error()})
			return
		}
		base = norm
	}

	var err error
	if strings.TrimSpace(body.APIKey) != "" || body.Base != nil {
		// 只改了接口根也要用「新地址 + 库里已存的凭据」测一遍，
		// 否则 s.cal.Ping 会拿旧地址的缓存 Token，测不出反代通不通。
		key, pin := strings.TrimSpace(body.APIKey), strings.TrimSpace(body.PIN)
		if key == "" {
			key = strings.TrimSpace(s.setting(ctx, "tvdb_api_key"))
			pin = strings.TrimSpace(s.setting(ctx, "tvdb_pin"))
		}
		c := tvdb.New(key, pin)
		if base != "" {
			c.Base = base
		}
		err = c.Ping(ctx)
	} else {
		err = s.cal.Ping(ctx)
	}
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "TVDB API Key / PIN 可用，播出时刻与电视网增强已生效",
	})
}
