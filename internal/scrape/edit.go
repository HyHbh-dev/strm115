package scrape

// EditMeta 让用户在海报墙上直接改元数据：改完重写本地 nfo、更新台账，
// 并同步修正元数据缓存，保证之后重新刮削不会把人工修改覆盖回去。
//
// 全程不联网：标题、年份、简介、类型这些字段本来就在台账和缓存里，
// 重新去查 IMDb 既浪费请求，又会把用户改过的值冲掉。只有「换 IMDb ID」
// 才需要走 RescrapeOne（由 HTTP 层负责分流）。
import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"strm115/internal/store"
)

// Edit 是允许人工修改的字段。零值表示「不改」。
type Edit struct {
	Title    string   `json:"title,omitempty"`
	Original string   `json:"original,omitempty"`
	Year     string   `json:"year,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Genres   []string `json:"genres,omitempty"`
	Rating   string   `json:"rating,omitempty"`
}

func (e Edit) empty() bool {
	return strings.TrimSpace(e.Title) == "" && strings.TrimSpace(e.Original) == "" &&
		strings.TrimSpace(e.Year) == "" && strings.TrimSpace(e.Overview) == "" &&
		len(e.Genres) == 0 && strings.TrimSpace(e.Rating) == ""
}

func (s *Service) EditMeta(ctx context.Context, lib store.Library, m store.Media, e Edit) (store.Media, error) {
	if strings.TrimSpace(m.RelPath) == "" {
		return m, errors.New("这条记录没有本地路径，无法改写元数据")
	}
	if e.empty() {
		return m, nil
	}

	root := ResolveRoot(lib.LocalPath, s.mediaRoot())
	p := Parse(m.RelPath)
	if p.Kind == "" {
		p.Kind = m.Kind
	}
	if p.Kind == "" {
		p.Kind = KindMovie
	}
	if p.Season == 0 {
		p.Season = m.Season
	}
	if p.Episode == 0 {
		p.Episode = m.Episode
	}
	item := workItem{
		file:       store.File{LibraryID: lib.ID, RelPath: m.RelPath},
		parsed:     p,
		folderSolo: s.soloDir(ctx, lib.ID, m.RelPath, p),
	}

	out := m
	if v := strings.TrimSpace(e.Title); v != "" {
		out.MetaTitle = v
	}
	if v := strings.TrimSpace(e.Year); v != "" {
		out.MetaYear = v
	}
	if v := strings.TrimSpace(e.Overview); v != "" {
		out.Overview = v
	}
	if v := strings.TrimSpace(e.Rating); v != "" {
		out.Rating = v
	}
	if len(e.Genres) > 0 {
		out.Genres = strings.Join(cleanList(e.Genres), ", ")
	}

	// 缓存里存着原名与整季单集清单 —— nfo 的 <originaltitle> 和剧集
	// 单集标题都靠它。这里只当本地数据源用，不发任何请求。
	cached, _ := s.cachedMeta(ctx, out.IMDbID)
	src := Meta{
		IMDbID:          out.IMDbID,
		Title:           firstNonEmpty(strings.TrimSpace(e.Original), cached.Title, out.MetaTitle),
		Year:            out.MetaYear,
		Rating:          out.Rating,
		Runtime:         out.Runtime,
		Genres:          splitList(out.Genres),
		Cast:            splitList(out.Cast),
		Director:        splitList(out.Director),
		Overview:        out.Overview,
		Episodes:        cached.Episodes,
		EpisodesFetched: cached.EpisodesFetched,
	}
	if v := strings.TrimSpace(e.Title); v != "" {
		// 必须显式给 TitleCN：缓存里那份是刮削时的旧译名，会盖掉用户改的标题。
		src.TitleCN = v
	}

	opt := Options{
		WriteNFO:    true,
		FetchMeta:   false,
		FetchImages: false, // 图片已经落盘，不要为了改个简介又去请求 IMDb
		Overwrite:   true,  // 用户就是要改，不能让「已有 nfo 就跳过」挡住
		Language:    "zh",
	}
	if err := s.writeArtifacts(ctx, root, src, &out, item, opt, map[string]bool{}); err != nil {
		return out, fmt.Errorf("重写 nfo: %w", err)
	}
	if err := s.st.UpsertMedia(ctx, out); err != nil {
		return out, err
	}
	s.patchMetaCache(ctx, m, p, e)
	return out, nil
}

// cachedMeta 读「按 ID」那份缓存。找不到只是少个原名，不算错误。
func (s *Service) cachedMeta(ctx context.Context, imdbID string) (Meta, bool) {
	if imdbID == "" {
		return Meta{}, false
	}
	_, payload, ok, err := s.st.GetMetaCache(ctx, "imdb|"+imdbID)
	if err != nil || !ok || payload == "" {
		return Meta{}, false
	}
	var m Meta
	if json.Unmarshal([]byte(payload), &m) != nil {
		return Meta{}, false
	}
	return m, true
}

// patchMetaCache 把人工修改同步进两种缓存键，避免下次刮削还原成旧值。
func (s *Service) patchMetaCache(ctx context.Context, m store.Media, p Parsed, e Edit) {
	if m.IMDbID == "" {
		return
	}
	keys := []string{"imdb|" + m.IMDbID}
	if p.Title != "" {
		keys = append(keys, store.MetaCacheKey(p.Kind, normalizeTitle(p.Title), p.Year))
	}
	for _, key := range keys {
		imdbID, payload, ok, err := s.st.GetMetaCache(ctx, key)
		if err != nil || !ok || payload == "" {
			continue
		}
		var c Meta
		if json.Unmarshal([]byte(payload), &c) != nil {
			continue
		}
		if v := strings.TrimSpace(e.Title); v != "" {
			c.TitleCN = v
		}
		if v := strings.TrimSpace(e.Original); v != "" {
			c.Title = v
		}
		if v := strings.TrimSpace(e.Year); v != "" {
			c.Year = v
		}
		if v := strings.TrimSpace(e.Overview); v != "" {
			c.Overview = v
		}
		if v := strings.TrimSpace(e.Rating); v != "" {
			c.Rating = v
		}
		if len(e.Genres) > 0 {
			c.Genres = cleanList(e.Genres)
		}
		data, err := json.Marshal(c)
		if err != nil {
			continue
		}
		if imdbID == "" {
			imdbID = c.IMDbID
		}
		if err := s.st.PutMetaCache(ctx, key, imdbID, string(data)); err != nil {
			s.log.Warn("回写元数据缓存失败", "key", key, "err", err)
		}
	}
}
