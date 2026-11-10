package watchhub

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// normalizeID 把剧集形式的 tt1234567:1:1 归一成 tt1234567。
func normalizeID(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ':'); i > 0 {
		s = s[:i]
	}
	return s
}

// ValidID 判断 id 是否符合 manifest 声明的 idPrefixes（空声明表示不限）。
func ValidID(id string, prefixes []string) bool {
	if len(prefixes) == 0 {
		return true
	}
	for _, p := range prefixes {
		if strings.HasPrefix(id, p) {
			return true
		}
	}
	return false
}

// Manifest 返回插件声明（首次调用拉取，之后走缓存）。
func (c *Client) Manifest(ctx context.Context) (*Manifest, error) {
	c.mu.Lock()
	cached := c.manifest
	c.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	m, err := c.FetchManifest(ctx, c.addonBase)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.manifest = m
	c.mu.Unlock()
	return m, nil
}

// Detail 取一部片子「元数据 + 各平台观看入口」，这就是前端「在哪看」那块的数据。
//
// typ 只会是 manifest 里声明的类型（默认取第一个）。Cinemeta 对类型写错的 ID
// 会返回残缺 meta（没海报、没年份），此时按 manifest 的其他类型重试一次——
// 用户只给了一个 IMDb ID，没理由让他自己猜是电影还是剧集。
func (c *Client) Detail(ctx context.Context, typ, id string) (*Item, error) {
	id = normalizeID(id)
	if id == "" {
		return nil, fmt.Errorf("IMDb ID 不能为空")
	}

	m, err := c.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidID(id, m.IDPrefixes) {
		return nil, fmt.Errorf("ID %q 不符合该插件的 ID 前缀 %v", id, m.IDPrefixes)
	}
	if typ == "" && len(m.Types) > 0 {
		typ = m.Types[0]
	}

	item, err := c.detail(ctx, typ, id)
	if err != nil {
		return nil, err
	}

	// 类型纠偏：只有 meta 残缺时才换类型重试，避免每次都多打两次上游。
	if (item.Poster == "" || item.Year == "") && len(m.Types) > 1 {
		for _, alt := range m.Types {
			if alt == typ {
				continue
			}
			other, altErr := c.detail(ctx, alt, id)
			if altErr != nil || other.Poster == "" {
				continue
			}
			return other, nil
		}
	}
	return item, nil
}

// Meta 只取元数据（标题 / 年份 / 评分 / 简介 / 演员 / 海报 / 单集清单），
// 不查「在哪看」。
//
// 刮削用这条路：Detail 会并发打 meta 与 stream 两个上游，而 nfo 里根本不写
// 播放平台。少一次请求，就少一次被上游限流的机会。
func (c *Client) Meta(ctx context.Context, typ, id string) (*Item, error) {
	id = normalizeID(id)
	if id == "" {
		return nil, fmt.Errorf("IMDb ID 不能为空")
	}

	m, err := c.Manifest(ctx)
	if err != nil {
		return nil, err
	}
	if !ValidID(id, m.IDPrefixes) {
		return nil, fmt.Errorf("ID %q 不符合该插件的 ID 前缀 %v", id, m.IDPrefixes)
	}
	if typ == "" && len(m.Types) > 0 {
		typ = m.Types[0]
	}

	meta, err := c.FetchMeta(ctx, c.metaBase, typ, id)
	if err != nil {
		return nil, fmt.Errorf("查询元数据失败: %w", err)
	}
	if meta == nil {
		return nil, fmt.Errorf("上游没有返回元数据: %s", id)
	}
	item := itemFromMeta(meta, typ, id)

	// 类型纠偏：Cinemeta 对「类型写错的 ID」会返回残缺 meta（没海报、没年份）。
	// 只有残缺时才换类型重试，正常条目不会多打请求。
	if (item.Poster == "" || item.Year == "") && len(m.Types) > 1 {
		for _, alt := range m.Types {
			if alt == typ {
				continue
			}
			other, altErr := c.FetchMeta(ctx, c.metaBase, alt, id)
			if altErr != nil || other == nil || other.Poster == "" {
				continue
			}
			return itemFromMeta(other, alt, id), nil
		}
	}
	return item, nil
}

// detail 并发拉 meta 与 stream，然后合并成一条 Item。
func (c *Client) detail(ctx context.Context, typ, id string) (*Item, error) {
	var (
		meta       *Meta
		streamResp *StreamResponse
		metaErr    error
		streamErr  error
	)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		meta, metaErr = c.FetchMeta(ctx, c.metaBase, typ, id)
	}()
	go func() {
		defer wg.Done()
		streamResp, streamErr = c.FetchStreams(ctx, c.addonBase, typ, id)
	}()
	wg.Wait()

	// meta 是「有没有这部片」的判据；stream 失败只是「查不到在哪看」，
	// 不该让整个查询失败。
	if metaErr != nil {
		return nil, fmt.Errorf("查询元数据失败: %w", metaErr)
	}
	if meta == nil {
		return nil, fmt.Errorf("上游没有返回元数据: %s", id)
	}

	item := itemFromMeta(meta, typ, id)

	if streamResp != nil {
		for _, s := range streamResp.Streams {
			item.Streams = append(item.Streams, Source{
				Platform:     s.Name,
				Availability: s.Title,
				URL:          s.ExternalURL,
				DeepLinks:    s.DeepLinks(),
			})
			if s.Name != "" && !contains(item.Platforms, s.Name) {
				item.Platforms = append(item.Platforms, s.Name)
			}
			if item.WatchURL == "" {
				item.WatchURL = s.ExternalURL
			}
		}
	}
	item.PlatformCount = len(item.Streams)
	if streamErr != nil {
		item.Error = "stream: " + streamErr.Error()
	}
	return item, nil
}

// itemFromMeta 把上游 meta 映射成 Item。Detail 与 Meta 共用这一份，
// 保证「刮削写进 nfo 的字段」和「前端详情页看到的字段」永远一致。
func itemFromMeta(meta *Meta, typ, id string) *Item {
	return &Item{
		IMDbID:      pickString(meta.IMDbID, id),
		Name:        meta.Name,
		Type:        pickString(meta.Type, typ),
		Year:        meta.Year.String(),
		ReleaseInfo: meta.ReleaseInfo.String(),
		IMDbRating:  meta.IMDbRating.String(),
		Runtime:     meta.Runtime.String(),
		Genres:      pickList(meta.Genres, meta.Genre),
		Cast:        []string(meta.Cast),
		Director:    []string(meta.Director),
		Description: meta.Description,
		Slug:        meta.Slug,
		Poster:      meta.Poster,
		Background:  meta.Background,
		Logo:        meta.Logo,
		Videos:      meta.Videos,
	}
}

// Hot 从 Cinemeta 热门榜取一批片子（manifest 没有 catalog 时用它做发现）。
func (c *Client) Hot(ctx context.Context, typ string, limit int) ([]Meta, error) {
	if typ == "" {
		m, err := c.Manifest(ctx)
		if err != nil {
			return nil, err
		}
		if len(m.Types) > 0 {
			typ = m.Types[0]
		}
	}
	return c.FetchCatalog(ctx, c.metaBase, typ, limit)
}

func pickString(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func pickList(lists ...StringList) []string {
	for _, l := range lists {
		if len(l) > 0 {
			return []string(l)
		}
	}
	return nil
}
