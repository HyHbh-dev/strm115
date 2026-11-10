package mediaserver

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// emby 同时兼容 Emby 与 Jellyfin：两者在「列库 / 建库 / 刷新」这几个接口上一致，
// 差异只在鉴权头的写法。这里两个头都发，省得为了一行差异分叉出两个实现。
type emby struct {
	kind   string
	base   string
	apiKey string
	hc     *http.Client
}

func newEmby(cfg Config) *emby {
	base := strings.TrimRight(cfg.URL, "/")
	base = strings.TrimSuffix(base, "/emby") // 用户常把 /emby 一起粘进来
	kind := cfg.Kind
	if kind == "" {
		kind = "emby"
	}
	return &emby{
		kind:   kind,
		base:   base,
		apiKey: cfg.APIKey,
		hc:     &http.Client{Timeout: 30 * time.Second},
	}
}

func (e *emby) Name() string { return e.kind }

func (e *emby) do(ctx context.Context, method, path string, query url.Values, out any) error {
	u := e.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body io.Reader
	if method == http.MethodPost && out == nil {
		body = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("X-Emby-Token", e.apiKey)
	req.Header.Set("Authorization", `MediaBrowser Token="`+e.apiKey+`"`)
	req.Header.Set("Accept", "application/json")

	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", e.kind, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s 返回 %d: %s", e.kind, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("解析 %s 响应失败: %w", e.kind, err)
		}
	}
	return nil
}

func (e *emby) Ping(ctx context.Context) error {
	var info struct {
		ServerName string `json:"ServerName"`
		Version    string `json:"Version"`
	}
	if err := e.do(ctx, http.MethodGet, "/System/Info", nil, &info); err != nil {
		return err
	}
	if info.ServerName == "" && info.Version == "" {
		return fmt.Errorf("%s 返回空信息，检查 URL 与 API Key", e.kind)
	}
	return nil
}

// Libraries 列全部虚拟库。
func (e *emby) Libraries(ctx context.Context) ([]Library, error) {
	var raw []struct {
		Name           string   `json:"Name"`
		ItemID         string   `json:"ItemId"`
		CollectionType string   `json:"CollectionType"`
		Locations      []string `json:"Locations"`
	}
	if err := e.do(ctx, http.MethodGet, "/Library/VirtualFolders", nil, &raw); err != nil {
		return nil, err
	}
	out := make([]Library, 0, len(raw))
	for _, r := range raw {
		out = append(out, Library{ID: r.ItemID, Name: r.Name, CollectionType: r.CollectionType, Paths: r.Locations})
	}
	return out, nil
}

// EmbyCollectionType 把我们的库类型翻成 Emby 的 collectionType。
//
// mixed 必须降级成 movies —— 这不是保守，是实测结论：
// Emby 4.10.2 的「混合内容」库建出来之后，扫描会把目录全部建好，
// 但一个 .strm 都不会变成影片，日志里连 FFProbeProvider 都不会出现，
// 也不报错。同一个目录换成 movies 类型，25 个文件立刻全部入库。
// 所以别让用户踩这个坑：库里既使有剧集，也用 movies 建，
// Emby 照样能按 Season 目录认出集数。
func EmbyCollectionType(collectionType string) string {
	switch collectionType {
	case "tvshows":
		return "tvshows"
	case "mixed", "":
		return "movies"
	default:
		return "movies"
	}
}

// EnsureLibrary 幂等建库。
//
// 注意：同名但路径不同的库不重建——用户可能在 Emby 里手动加过路径，
// 直接报错让用户自己决定，比悄悄改掉他的配置安全。
func (e *emby) EnsureLibrary(ctx context.Context, name, collectionType string, paths []string) (Library, error) {
	collectionType = EmbyCollectionType(collectionType)

	libs, err := e.Libraries(ctx)
	if err == nil {
		for _, l := range libs {
			if strings.EqualFold(l.Name, name) {
				return l, nil
			}
		}
	}

	q := url.Values{}
	q.Set("name", name)
	q.Set("collectionType", collectionType)
	// refreshLibrary 必须是 true：建库那一下是 Emby 唯一一次真正递归读目录、
	// 跑 ffprobe 把 .strm 变成影片的时机。用 false 建库，之后无论怎么点
	// 「扫描媒体库」都只会补目录、不会补影片（目录项已存在就会被跳过）。
	q.Set("refreshLibrary", "true")
	for _, p := range paths {
		q.Add("paths", p)
	}
	if err := e.do(ctx, http.MethodPost, "/Library/VirtualFolders", q, nil); err != nil {
		return Library{}, err
	}

	// 回读一次拿 ItemId（Emby 的创建接口不返回它）
	if libs, err := e.Libraries(ctx); err == nil {
		for _, l := range libs {
			if strings.EqualFold(l.Name, name) {
				return l, nil
			}
		}
	}
	return Library{Name: name, CollectionType: collectionType, Paths: paths}, nil
}

// Refresh 触发扫描。libID 为空时刷新全部。
func (e *emby) Refresh(ctx context.Context, libID string) error {
	if libID == "" {
		return e.do(ctx, http.MethodPost, "/Library/Refresh", nil, nil)
	}
	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("MetadataRefreshMode", "Default")
	q.Set("ImageRefreshMode", "Default")
	return e.do(ctx, http.MethodPost, "/Items/"+libID+"/Refresh", q, nil)
}

// RefreshPaths 让媒体服务器重读这些路径对应的条目（本地 nfo / 图片）。
//
// 为什么不能只调 /Library/Refresh：整库扫描只处理「新增/删除」，
// 对已经入库、只是 nfo 内容变了的条目会直接跳过。实测改完 nfo 再扫描，
// 条目上的简介仍然是空的；只有条目级 refresh 才会重新解析本地文件。
func (e *emby) RefreshPaths(ctx context.Context, paths []string) (int, error) {
	if len(paths) == 0 {
		return 0, nil
	}
	var resp struct {
		Items []struct {
			ID   string `json:"Id"`
			Path string `json:"Path"`
		} `json:"Items"`
	}
	q := url.Values{}
	q.Set("Recursive", "true")
	q.Set("IncludeItemTypes", "Movie,Episode,Series,Season")
	q.Set("Fields", "Path")
	q.Set("Limit", "50000")
	if err := e.do(ctx, http.MethodGet, "/Items", q, &resp); err != nil {
		return 0, err
	}

	want := make([]string, 0, len(paths))
	for _, p := range paths {
		if p = strings.TrimRight(strings.ReplaceAll(p, "\\", "/"), "/"); p != "" {
			want = append(want, p)
		}
	}

	var ids []string
	seen := make(map[string]bool, len(want))
	for _, it := range resp.Items {
		itemPath := strings.ReplaceAll(it.Path, "\\", "/")
		hit := false
		for _, w := range want {
			// 目录用前缀匹配：传进来的是影片目录，条目路径是目录下的 .strm。
			if itemPath == w || strings.HasPrefix(itemPath, w+"/") {
				hit = true
				break
			}
		}
		if hit && !seen[it.ID] {
			seen[it.ID] = true
			ids = append(ids, it.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}

	// 并发 4：条目 refresh 是 80~200ms 的请求，串行刷 500 条要一分半。
	// 再高意义不大，反而容易把低配 NAS 上的 Emby 拖住。
	const workers = 4
	sem := make(chan struct{}, workers)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		done     int
		firstErr error
	)
	for _, id := range ids {
		select {
		case <-ctx.Done():
			wg.Wait()
			return done, ctx.Err()
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			defer func() { <-sem }()
			err := e.doJSON(ctx, http.MethodPost, "/Items/"+id+"/Refresh", map[string]any{
				"MetadataRefreshMode": "FullRefresh",
				"ImageRefreshMode":    "None",
				"ReplaceAllImages":    false,
				"ReplaceAllMetadata":  false,
			})
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			done++
		}(id)
	}
	wg.Wait()
	return done, firstErr
}

// MediaUpdated 通知 Emby 具体哪几个文件被改过。
//
// 官方文档里这个接口叫「Reports that new movies have been added」，但实际
// 收的是任意文件路径；UpdateType 用 Created / Modified / Deleted，
// 省略时服务器自己判断。改写字幕属于 Modified。
func (e *emby) MediaUpdated(ctx context.Context, paths []string) error {
	updates := make([]map[string]string, 0, len(paths))
	seen := make(map[string]bool, len(paths))
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		updates = append(updates, map[string]string{
			"Path": p, "UpdateType": "Modified",
		})
	}
	if len(updates) == 0 {
		return nil
	}
	return e.doJSON(ctx, http.MethodPost, "/Library/Media/Updated",
		map[string]any{"Updates": updates})
}

// doJSON 发一个带 JSON 体的请求。
//
// Emby 4.10 对 /Items/{id}/Refresh 的参数只认请求体，把 MetadataRefreshMode
// 放 query 里会 400（报错在 RestPath.CreateRequest，看不出所以然）。
func (e *emby) doJSON(ctx context.Context, method, path string, in any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, method, e.base+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Emby-Token", e.apiKey)
	req.Header.Set("Authorization", `MediaBrowser Token="`+e.apiKey+`"`)
	resp, err := e.hc.Do(req)
	if err != nil {
		return fmt.Errorf("连接 %s 失败: %w", e.kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		return fmt.Errorf("%s 返回 %d: %s", e.kind, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}
