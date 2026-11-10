package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// TVSeason 是正片的一季。特别篇（season_number = 0）不进这个列表。
type TVSeason struct {
	Season       int    `json:"season"`
	EpisodeCount int    `json:"episode_count"`
	Name         string `json:"name,omitempty"`
	AirDate      string `json:"air_date,omitempty"`
}

// TVEpisode 是一集的播出信息，订阅只用到季/集两个数字。
type TVEpisode struct {
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Name    string `json:"name,omitempty"`
	AirDate string `json:"air_date,omitempty"`
}

// TVShow 是订阅判断「该出到第几集」所需的全部信息。
type TVShow struct {
	ID           int        `json:"id"`
	Name         string     `json:"name"`
	OriginalName string     `json:"original_name,omitempty"`
	Year         int        `json:"year,omitempty"`
	IMDb         string     `json:"imdb,omitempty"`
	Seasons      []TVSeason `json:"seasons,omitempty"`
	// TotalEpisodes 是 TMDB 口径的总集数，含已定档未播出的。
	TotalEpisodes int `json:"total_episodes,omitempty"`
	// LastAired 是已经播出的最新一集。未开播的剧这里为空。
	LastAired *TVEpisode `json:"last_aired,omitempty"`
	// NextAiring 是下一集的播出时间，用来决定「下次什么时候再查」。
	NextAiring *TVEpisode `json:"next_airing,omitempty"`
	// Ended 为真表示 TMDB 侧已完结，不必再轮询。
	Ended  bool   `json:"ended,omitempty"`
	Status string `json:"status,omitempty"`
}

// Latest 返回「剧集应该追到哪一集」。
//
// 优先用 last_episode_to_air：它是 TMDB 自己维护的「最新已播出」，比
// 「season 里最大的 episode_number」准 —— 后者在季中停播、复播错档的时候
// 会偏高，导致订阅以为有集可下，实际还没播。
//
// 兜底用 seasons 里的集数：有些老剧 / 短剧 TMDB 没填 last_episode_to_air，
// 这时候「第 N 季共 M 集」是唯一能用的信息。两处都没有就返回零值，
// 调用方据此报「查不到集数」而不是当成 E00。
func (t *TVShow) Latest() TVEpisode {
	if t == nil {
		return TVEpisode{}
	}
	if t.LastAired != nil && t.LastAired.Episode > 0 {
		return *t.LastAired
	}
	// 没有 last_episode_to_air 时按「季号最大、集数最多」那一季推。
	// 特别篇已经在解析时剔掉了，所以这里不会挑到 season 0。
	var best TVSeason
	for _, s := range t.Seasons {
		if s.Season > best.Season || (s.Season == best.Season && s.EpisodeCount > best.EpisodeCount) {
			best = s
		}
	}
	if best.EpisodeCount <= 0 {
		return TVEpisode{}
	}
	return TVEpisode{Season: best.Season, Episode: best.EpisodeCount}
}

// HasAired 判断某个季/集是否已经播出。
//
// 只比季集号，不比日期：日期对「北京时间今天播」这种边界很敏感，而季集号
// 是单调的。水位线只在确认「已播出」之后才推进，所以这个比较够用。
func (t *TVShow) HasAired(season, episode int) bool {
	latest := t.Latest()
	if latest.Episode <= 0 {
		return false
	}
	if season != latest.Season {
		return season < latest.Season
	}
	return episode <= latest.Episode
}

// TV 查一部剧的季集信息。language=zh-CN 拿中文名，external_ids 补 IMDb ID
// （订阅搜索种子站要用英文原名 / IMDb，中文名在那几个站基本搜不到）。
//
// 两次请求：/tv/{id} 与 /tv/{id}/external_ids。external_ids 失败不算致命 ——
// 没有 IMDb ID 只是搜不到种子站，TG 频道还能按中文名搜到。
func (c *Client) TV(ctx context.Context, id int) (*TVShow, error) {
	if id <= 0 {
		return nil, fmt.Errorf("剧集 id 不合法: %d", id)
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	raw, err := c.get(ctx, "/tv/"+strconv.Itoa(id), v)
	if err != nil {
		return nil, err
	}
	var resp tvResp
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 剧集响应失败: %w", err)
	}

	out := &TVShow{
		ID:            resp.ID,
		Name:          resp.Name,
		OriginalName:  resp.OriginalName,
		Year:          yearOf(resp.FirstAirDate),
		TotalEpisodes: resp.NumberOfEpisodes,
		Status:        resp.Status,
		Ended:         resp.Status == "Ended" || resp.Status == "Canceled",
	}
	for _, s := range resp.Seasons {
		// season_number 为 0 的是特别篇：集数口径和正片混在一起会让水位线跳错，
		// 而且追更的人要的是正片。
		if s.SeasonNumber <= 0 || s.EpisodeCount <= 0 {
			continue
		}
		out.Seasons = append(out.Seasons, TVSeason{
			Season:       s.SeasonNumber,
			EpisodeCount: s.EpisodeCount,
			Name:         s.Name,
			AirDate:      s.AirDate,
		})
	}
	if e := resp.LastEpisodeToAir; e != nil && e.EpisodeNumber > 0 {
		out.LastAired = &TVEpisode{
			Season:  e.SeasonNumber,
			Episode: e.EpisodeNumber,
			Name:    e.Name,
			AirDate: e.AirDate,
		}
	}
	if e := resp.NextEpisodeToAir; e != nil && e.EpisodeNumber > 0 {
		out.NextAiring = &TVEpisode{
			Season:  e.SeasonNumber,
			Episode: e.EpisodeNumber,
			Name:    e.Name,
			AirDate: e.AirDate,
		}
	}

	// IMDb ID 单独一次请求。失败就留空，不影响上面这些。
	if raw, err := c.get(ctx, "/tv/"+strconv.Itoa(id)+"/external_ids", nil); err == nil {
		var ext struct {
			IMDb string `json:"imdb_id"`
		}
		if json.Unmarshal(raw, &ext) == nil {
			out.IMDb = strings.TrimSpace(ext.IMDb)
		}
	}
	return out, nil
}

// Episodes 拉某一季的分集清单，给「建订阅时选从第几集开始」用。
func (c *Client) Episodes(ctx context.Context, id, season int) ([]TVEpisode, error) {
	if id <= 0 {
		return nil, fmt.Errorf("剧集 id 不合法: %d", id)
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	raw, err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season), v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
			AirDate       string `json:"air_date"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析分集响应失败: %w", err)
	}
	out := make([]TVEpisode, 0, len(resp.Episodes))
	for _, e := range resp.Episodes {
		if e.EpisodeNumber <= 0 {
			continue
		}
		out = append(out, TVEpisode{
			Season:  season,
			Episode: e.EpisodeNumber,
			Name:    e.Name,
			AirDate: e.AirDate,
		})
	}
	return out, nil
}

type tvResp struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	OriginalName     string `json:"original_name"`
	FirstAirDate     string `json:"first_air_date"`
	Status           string `json:"status"`
	NumberOfEpisodes int    `json:"number_of_episodes"`
	Seasons          []struct {
		SeasonNumber int    `json:"season_number"`
		EpisodeCount int    `json:"episode_count"`
		Name         string `json:"name"`
		AirDate      string `json:"air_date"`
	} `json:"seasons"`
	LastEpisodeToAir *struct {
		SeasonNumber  int    `json:"season_number"`
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		AirDate       string `json:"air_date"`
	} `json:"last_episode_to_air"`
	NextEpisodeToAir *struct {
		SeasonNumber  int    `json:"season_number"`
		EpisodeNumber int    `json:"episode_number"`
		Name          string `json:"name"`
		AirDate       string `json:"air_date"`
	} `json:"next_episode_to_air"`
}

// yearOf 复用 tmdb.go 里那份（从 2006-01-02 取年份，取不到返回 0）。
