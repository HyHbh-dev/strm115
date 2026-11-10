package httpapi

// 订阅弹窗里点某一集，要在下面显示「这一集的详情」。
//
// 分集清单（GET /tv/{id}/season/{n}）一次就把整季的集名、播出日、简介、
// 时长、剧照、评分都带回来了，所以按 (剧, 季) 缓一小会儿：用户在 210 集
// 的网格里连续点几下，不该每点一次就打一次 TMDB。

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"strm115/internal/tmdb"
)

const episodeCacheTTL = 10 * time.Minute

type episodeCacheEntry struct {
	at  time.Time
	eps []tmdb.SeasonEpisode
}

var (
	episodeCacheMu sync.Mutex
	episodeCache   = map[string]episodeCacheEntry{}
)

func (s *Server) seasonEpisodesCached(ctx context.Context, c *tmdb.Client, id, season int) ([]tmdb.SeasonEpisode, error) {
	key := fmt.Sprintf("%d:%d", id, season)
	episodeCacheMu.Lock()
	if e, ok := episodeCache[key]; ok && time.Since(e.at) < episodeCacheTTL {
		eps := e.eps
		episodeCacheMu.Unlock()
		return eps, nil
	}
	episodeCacheMu.Unlock()

	eps, err := c.SeasonEpisodes(ctx, id, season)
	if err != nil {
		return nil, err
	}
	episodeCacheMu.Lock()
	// 只留最近的几十个组合，避免用户翻几十部剧把缓存撑大。
	if len(episodeCache) > 64 {
		episodeCache = map[string]episodeCacheEntry{}
	}
	episodeCache[key] = episodeCacheEntry{at: time.Now(), eps: eps}
	episodeCacheMu.Unlock()
	return eps, nil
}

// handleTMDBEpisode GET /api/tmdb/episode?tmdb_id=&season=&episode=
func (s *Server) handleTMDBEpisode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id, _ := strconv.Atoi(strings.TrimSpace(q.Get("tmdb_id")))
	season, _ := strconv.Atoi(strings.TrimSpace(q.Get("season")))
	episode, _ := strconv.Atoi(strings.TrimSpace(q.Get("episode")))
	if id <= 0 || season <= 0 || episode <= 0 {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("需要 tmdb_id、season、episode 三个正整数"))
		return
	}
	client := s.tmdbClient(r.Context())
	if client == nil {
		writeErr(w, http.StatusBadRequest, fmt.Errorf("先到设置里填 TMDB API Key"))
		return
	}
	eps, err := s.seasonEpisodesCached(r.Context(), client, id, season)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	for _, e := range eps {
		if e.Episode == episode {
			writeJSON(w, http.StatusOK, e)
			return
		}
	}
	writeErr(w, http.StatusNotFound, fmt.Errorf("TMDB 里没有 S%02dE%02d", season, episode))
}
