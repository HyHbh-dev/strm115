package watchhub

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

func joinURL(base, p string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(p, "/")
}

// FetchManifest 读取插件声明，后面的接口、类型、ID 前缀都以它为准。
func (c *Client) FetchManifest(ctx context.Context, base string) (*Manifest, error) {
	u := joinURL(base, "manifest.json")
	var m Manifest
	if err := c.GetJSON(ctx, u, &m); err != nil {
		return nil, fmt.Errorf("获取 manifest 失败: %w", err)
	}
	if m.ID == "" {
		return nil, fmt.Errorf("manifest 缺少 id 字段: %s", u)
	}
	return &m, nil
}

// CatalogQuery 是一次「电影墙」目录查询。
type CatalogQuery struct {
	Type  string // movie / series
	Genre string // IMDb 的英文类型（Action / Sci-Fi …），空 = 不限
	Skip  int    // 已经看过多少条
	Limit int    // 想要多少条
}

// FetchCatalogPage 取一页目录（电影墙用）。
//
// 分页和筛选都塞在**同一段**路径里：catalog/movie/top/skip=50&genre=Action.json。
// 用 `/` 分开（skip=50/genre=Action）会 404 —— 这一点在真接口上验证过，
// 所以这里手工拼串，不走 url.Values（它会把 & 转义成 %26，同样 404）。
func (c *Client) FetchCatalogPage(ctx context.Context, metaBase string, q CatalogQuery) ([]Meta, error) {
	typ := strings.TrimSpace(q.Type)
	if typ == "" {
		typ = "movie"
	}
	limit := q.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}

	var extras []string
	if q.Skip > 0 {
		extras = append(extras, "skip="+strconv.Itoa(q.Skip))
	}
	if g := strings.TrimSpace(q.Genre); g != "" {
		extras = append(extras, "genre="+g)
	}
	p := fmt.Sprintf("catalog/%s/top", typ)
	if len(extras) > 0 {
		p += "/" + strings.Join(extras, "&")
	}
	p += ".json"

	var resp struct {
		Metas []Meta `json:"metas"`
	}
	// 空字符串表示「用客户端默认上游」，调用方不必自己拼 base
	if strings.TrimSpace(metaBase) == "" {
		metaBase = c.metaBase
	}
	if err := c.GetJSON(ctx, joinURL(metaBase, p), &resp); err != nil {
		return nil, fmt.Errorf("获取 %s 目录失败: %w", typ, err)
	}
	// 上游一次给 50 条，这里按 limit 截断；不够时调用方靠 has_more 决定要不要再翻。
	if len(resp.Metas) > limit {
		resp.Metas = resp.Metas[:limit]
	}
	return resp.Metas, nil
}

// FetchCatalog 从 Cinemeta 热门目录发现 IMDb ID（manifest.catalogs 为空时用它补位）。
func (c *Client) FetchCatalog(ctx context.Context, metaBase, typ string, limit int) ([]Meta, error) {
	if limit <= 0 {
		limit = 10
	}
	out := make([]Meta, 0, limit)
	seen := make(map[string]bool)
	for skip := 0; len(out) < limit && skip < 1000; skip += 50 {
		p := fmt.Sprintf("catalog/%s/top.json", typ)
		if skip > 0 {
			p = fmt.Sprintf("catalog/%s/top/skip=%d.json", typ, skip)
		}
		var resp struct {
			Metas []Meta `json:"metas"`
		}
		if err := c.GetJSON(ctx, joinURL(metaBase, p), &resp); err != nil {
			if len(out) > 0 {
				break // 已拿到部分数据，分页失败不致命
			}
			return nil, fmt.Errorf("获取 %s 热门目录失败: %w", typ, err)
		}
		if len(resp.Metas) == 0 {
			break
		}
		for _, m := range resp.Metas {
			id := m.IMDbID
			if id == "" {
				id = m.ID
			}
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			out = append(out, m)
			if len(out) >= limit {
				break
			}
		}
		if len(resp.Metas) < 50 {
			break
		}
	}
	return out, nil
}

// FetchMeta 补齐名称、海报、年份、评分、简介等元数据。
func (c *Client) FetchMeta(ctx context.Context, metaBase, typ, id string) (*Meta, error) {
	p := fmt.Sprintf("meta/%s/%s.json", typ, id)
	var resp struct {
		Meta Meta `json:"meta"`
	}
	if err := c.GetJSON(ctx, joinURL(metaBase, p), &resp); err != nil {
		return nil, err
	}
	if resp.Meta.Name == "" && resp.Meta.ID == "" {
		return nil, fmt.Errorf("元数据为空: %s", p)
	}
	return &resp.Meta, nil
}

// FetchStreams 查询该 IMDb ID 在各流媒体平台的可观看信息。
func (c *Client) FetchStreams(ctx context.Context, addonBase, typ, id string) (*StreamResponse, error) {
	p := fmt.Sprintf("stream/%s/%s.json", typ, id)
	var resp StreamResponse
	if err := c.GetJSON(ctx, joinURL(addonBase, p), &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

var imageSizeRE = regexp.MustCompile(`/(poster|background|logo)/(small|medium|large)/`)

// ImageWithSize 把 metahub 图片链接换成指定尺寸。
func ImageWithSize(raw, size string) string {
	if raw == "" || size == "" || size == "original" {
		return raw
	}
	return imageSizeRE.ReplaceAllString(raw, "/${1}/"+size+"/")
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}
