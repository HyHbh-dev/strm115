package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// 播出日历用的接口。
//
// 和榜单 Discover 的区别：Discover 回答「有哪些好片」，按首播/上映日期排序；
// 日历要回答「这七天里，哪部剧的第几集在哪天播」，必须走 /tv/on_the_air、
// /tv/airing_today 拿在播剧单，再逐部查分季。TMDB 没有「按日期反查分集」的
// 接口，所以这一步只能并发逐剧问，结果按整周缓存（见 internal/calendar）。
//
// 时刻 / 电视网：TMDB 的 /tv/{id} 只有 networks[] 和 episode_run_time[]，
// 没有「每周几几点播」。那部分由 TVDB 的 airsTime 补，本文件只负责不缺的字段。

// AiringShow 是在播剧单里的一行。列表接口本身就带 poster_path，不用再查详情。
type AiringShow struct {
	ID           int     `json:"id"`
	Name         string  `json:"name"`
	Original     string  `json:"original,omitempty"`
	FirstAirDate string  `json:"first_air_date,omitempty"`
	Poster       string  `json:"poster,omitempty"`
	Backdrop     string  `json:"backdrop,omitempty"`
	Rating       float64 `json:"rating,omitempty"`
	Popularity   float64 `json:"popularity,omitempty"`
	Overview     string  `json:"overview,omitempty"`
	Country      string  `json:"country,omitempty"`
}

// Airing 拉一页在播剧单。list 只能是 on_the_air / airing_today，
// 避免调用方拼出任意 /tv/ 路径（get 会带上 api_key）。
func (c *Client) Airing(ctx context.Context, list string, page int) ([]AiringShow, error) {
	if list != "on_the_air" && list != "airing_today" {
		return nil, fmt.Errorf("不支持的剧单类型: %s", list)
	}
	if page <= 0 {
		page = 1
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	v.Set("page", strconv.Itoa(page))
	raw, err := c.get(ctx, "/tv/"+list, v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			ID            int      `json:"id"`
			Name          string   `json:"name"`
			OriginalName  string   `json:"original_name"`
			FirstAirDate  string   `json:"first_air_date"`
			PosterPath    string   `json:"poster_path"`
			BackdropPath  string   `json:"backdrop_path"`
			VoteAverage   float64  `json:"vote_average"`
			Popularity    float64  `json:"popularity"`
			Overview      string   `json:"overview"`
			OriginCountry []string `json:"origin_country"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 在播剧单失败: %w", err)
	}
	out := make([]AiringShow, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.ID <= 0 {
			continue
		}
		country := ""
		if len(r.OriginCountry) > 0 {
			country = r.OriginCountry[0]
		}
		out = append(out, AiringShow{
			ID:           r.ID,
			Name:         r.Name,
			Original:     r.OriginalName,
			FirstAirDate: r.FirstAirDate,
			Poster:       PosterURL(r.PosterPath),
			Backdrop:     BackdropURL(r.BackdropPath),
			Rating:       r.VoteAverage,
			Popularity:   r.Popularity,
			Overview:     r.Overview,
			Country:      country,
		})
	}
	return out, nil
}

// ShowDetail 是一部剧在日历里需要的全部信息。
//
// 比 tv.go 的 TVShow 多三样：海报（列表页的海报可能破图，详情能兜底）、
// networks（卡片上的「20:00 on NBC」）、tvdb_id（拿去查 TVDB）。
type ShowDetail struct {
	ID           int        `json:"id"`
	Name         string     `json:"name"`
	Original     string     `json:"original,omitempty"`
	Status       string     `json:"status,omitempty"`
	FirstAirDate string     `json:"first_air_date,omitempty"`
	LastAirDate  string     `json:"last_air_date,omitempty"`
	Poster       string     `json:"poster,omitempty"`
	Backdrop     string     `json:"backdrop,omitempty"`
	Overview     string     `json:"overview,omitempty"`
	Networks     []string   `json:"networks,omitempty"`
	Runtime      int        `json:"runtime,omitempty"`
	Rating       float64    `json:"rating,omitempty"`
	Popularity   float64    `json:"popularity,omitempty"`
	IMDb         string     `json:"imdb,omitempty"`
	TVDBID       int        `json:"tvdb_id,omitempty"`
	Seasons      []TVSeason `json:"seasons,omitempty"`
	LastAired    *TVEpisode `json:"last_aired,omitempty"`
	NextAiring   *TVEpisode `json:"next_airing,omitempty"`
}

// Network 返回卡片上要显示的电视网，取第一个（TMDB 多网联播时第一个是主网）。
func (d *ShowDetail) Network() string {
	if d == nil || len(d.Networks) == 0 {
		return ""
	}
	return d.Networks[0]
}

// ShowDetail 查 /tv/{id} + /tv/{id}/external_ids。
// external_ids 失败不算致命：只是这部剧没有 TVDB 增强，日历照常显示。
func (c *Client) ShowDetail(ctx context.Context, id int) (*ShowDetail, error) {
	if id <= 0 {
		return nil, fmt.Errorf("剧集 id 不合法: %d", id)
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	raw, err := c.get(ctx, "/tv/"+strconv.Itoa(id), v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		ID             int     `json:"id"`
		Name           string  `json:"name"`
		OriginalName   string  `json:"original_name"`
		Status         string  `json:"status"`
		FirstAirDate   string  `json:"first_air_date"`
		LastAirDate    string  `json:"last_air_date"`
		PosterPath     string  `json:"poster_path"`
		BackdropPath   string  `json:"backdrop_path"`
		Overview       string  `json:"overview"`
		VoteAverage    float64 `json:"vote_average"`
		Popularity     float64 `json:"popularity"`
		EpisodeRunTime []int   `json:"episode_run_time"`
		Networks       []struct {
			Name string `json:"name"`
		} `json:"networks"`
		Seasons []struct {
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
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析 TMDB 剧集详情失败: %w", err)
	}
	out := &ShowDetail{
		ID:           resp.ID,
		Name:         resp.Name,
		Original:     resp.OriginalName,
		Status:       resp.Status,
		FirstAirDate: resp.FirstAirDate,
		LastAirDate:  resp.LastAirDate,
		Poster:       PosterURL(resp.PosterPath),
		Backdrop:     BackdropURL(resp.BackdropPath),
		Overview:     resp.Overview,
		Rating:       resp.VoteAverage,
		Popularity:   resp.Popularity,
	}
	if len(resp.EpisodeRunTime) > 0 {
		out.Runtime = resp.EpisodeRunTime[0]
	}
	for _, n := range resp.Networks {
		if name := strings.TrimSpace(n.Name); name != "" {
			out.Networks = append(out.Networks, name)
		}
	}
	for _, s := range resp.Seasons {
		// 特别篇不进日历：它的 air_date 经常是空或乱填，混进来会多一堆假条目。
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
		out.LastAired = &TVEpisode{Season: e.SeasonNumber, Episode: e.EpisodeNumber, Name: e.Name, AirDate: e.AirDate}
	}
	if e := resp.NextEpisodeToAir; e != nil && e.EpisodeNumber > 0 {
		out.NextAiring = &TVEpisode{Season: e.SeasonNumber, Episode: e.EpisodeNumber, Name: e.Name, AirDate: e.AirDate}
	}

	if raw, err := c.get(ctx, "/tv/"+strconv.Itoa(id)+"/external_ids", nil); err == nil {
		var ext struct {
			IMDb   string `json:"imdb_id"`
			TVDBID int    `json:"tvdb_id"`
		}
		if json.Unmarshal(raw, &ext) == nil {
			out.IMDb = strings.TrimSpace(ext.IMDb)
			out.TVDBID = ext.TVDBID
		}
	}
	return out, nil
}

// SeasonEpisode 是分季接口里的一集，比订阅用的 TVEpisode 多了剧照和时长。
type SeasonEpisode struct {
	Season   int     `json:"season"`
	Episode  int     `json:"episode"`
	Name     string  `json:"name,omitempty"`
	AirDate  string  `json:"air_date,omitempty"`
	Still    string  `json:"still,omitempty"`
	Overview string  `json:"overview,omitempty"`
	Runtime  int     `json:"runtime,omitempty"`
	Rating   float64 `json:"rating,omitempty"`
}

// SeasonEpisodes 拉某一季的分集清单。日历靠它把「本周多集」的剧一次补全，
// 也靠它拿到集标题（last/next_episode_to_air 只给两集，不够铺一周）。
func (c *Client) SeasonEpisodes(ctx context.Context, id, season int) ([]SeasonEpisode, error) {
	if id <= 0 || season <= 0 {
		return nil, fmt.Errorf("剧集/季号不合法: %d/%d", id, season)
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	raw, err := c.get(ctx, fmt.Sprintf("/tv/%d/season/%d", id, season), v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Episodes []struct {
			EpisodeNumber int     `json:"episode_number"`
			Name          string  `json:"name"`
			AirDate       string  `json:"air_date"`
			StillPath     string  `json:"still_path"`
			Overview      string  `json:"overview"`
			Runtime       int     `json:"runtime"`
			VoteAverage   float64 `json:"vote_average"`
		} `json:"episodes"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析分集响应失败: %w", err)
	}
	out := make([]SeasonEpisode, 0, len(resp.Episodes))
	for _, e := range resp.Episodes {
		if e.EpisodeNumber <= 0 {
			continue
		}
		out = append(out, SeasonEpisode{
			Season:   season,
			Episode:  e.EpisodeNumber,
			Name:     e.Name,
			AirDate:  e.AirDate,
			Still:    imageURL(e.StillPath, BackdropSize),
			Overview: e.Overview,
			Runtime:  e.Runtime,
			Rating:   e.VoteAverage,
		})
	}
	return out, nil
}

// MovieRelease 是本周上映/发行的一部电影（对应截图左侧的 Movies 筛选）。
type MovieRelease struct {
	ID          int     `json:"id"`
	Title       string  `json:"title"`
	Original    string  `json:"original,omitempty"`
	ReleaseDate string  `json:"release_date,omitempty"`
	Poster      string  `json:"poster,omitempty"`
	Backdrop    string  `json:"backdrop,omitempty"`
	Overview    string  `json:"overview,omitempty"`
	Rating      float64 `json:"rating,omitempty"`
	Popularity  float64 `json:"popularity,omitempty"`
}

// MovieReleases 按发行日期区间查电影。TMDB 这里有 primary_release_date.gte/lte，
// 而剧集那边没有等价的「分集播出日期」过滤，所以剧集才要逐部查。
func (c *Client) MovieReleases(ctx context.Context, from, to string, page int) ([]MovieRelease, error) {
	if page <= 0 {
		page = 1
	}
	v := url.Values{}
	v.Set("language", DefaultLanguage)
	v.Set("page", strconv.Itoa(page))
	v.Set("sort_by", "popularity.desc")
	v.Set("include_adult", "false")
	v.Set("primary_release_date.gte", from)
	v.Set("primary_release_date.lte", to)
	raw, err := c.get(ctx, "/discover/movie", v)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Results []struct {
			ID            int     `json:"id"`
			Title         string  `json:"title"`
			OriginalTitle string  `json:"original_title"`
			ReleaseDate   string  `json:"release_date"`
			PosterPath    string  `json:"poster_path"`
			BackdropPath  string  `json:"backdrop_path"`
			Overview      string  `json:"overview"`
			VoteAverage   float64 `json:"vote_average"`
			Popularity    float64 `json:"popularity"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("解析电影上映列表失败: %w", err)
	}
	out := make([]MovieRelease, 0, len(resp.Results))
	for _, r := range resp.Results {
		if r.ID <= 0 || r.ReleaseDate == "" {
			continue
		}
		out = append(out, MovieRelease{
			ID:          r.ID,
			Title:       r.Title,
			Original:    r.OriginalTitle,
			ReleaseDate: r.ReleaseDate,
			Poster:      PosterURL(r.PosterPath),
			Backdrop:    BackdropURL(r.BackdropPath),
			Overview:    r.Overview,
			Rating:      r.VoteAverage,
			Popularity:  r.Popularity,
		})
	}
	return out, nil
}
