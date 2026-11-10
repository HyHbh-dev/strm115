package mediaserver

// Emby / Jellyfin 的条目读取实现。
//
// 两个实测坑，改这里之前请看：
//  1. GET /Items/{id} 在这台 Emby 4.10 上返回 404「找不到文件」，
//     取单条必须用 GET /Items?Ids=<id>。
//  2. Fields 不写全的话，返回的条目会缺 ProductionYear / CommunityRating
//     等字段（不是零值，是压根没这个 key），排序和卡片都会空白。
import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const catalogFields = "Path,ProductionYear,Overview,CommunityRating,ImageTags,ProviderIds," +
	"RunTimeTicks,Genres,DateCreated,ParentIndexNumber,IndexNumber,OriginalTitle," +
	"SeriesName,SortName,RecursiveItemCount"

type embyRawItem struct {
	ID                 string            `json:"Id"`
	Name               string            `json:"Name"`
	OriginalTitle      string            `json:"OriginalTitle"`
	Type               string            `json:"Type"`
	Overview           string            `json:"Overview"`
	Path               string            `json:"Path"`
	SeriesName         string            `json:"SeriesName"`
	DateCreated        string            `json:"DateCreated"`
	ProductionYear     int               `json:"ProductionYear"`
	CommunityRating    float64           `json:"CommunityRating"`
	RunTimeTicks       int64             `json:"RunTimeTicks"`
	RecursiveItemCount int               `json:"RecursiveItemCount"`
	ParentIndexNumber  int               `json:"ParentIndexNumber"`
	IndexNumber        int               `json:"IndexNumber"`
	Genres             []string          `json:"Genres"`
	ProviderIds        map[string]string `json:"ProviderIds"`
	ImageTags          map[string]string `json:"ImageTags"`
	BackdropImageTags  []string          `json:"BackdropImageTags"`
	People             []struct {
		Name string `json:"Name"`
		Role string `json:"Role"`
		// Id 与 PrimaryImageTag 是「演员头像」的关键：有 Id 才能走
		// /Items/{id}/Images/Primary 取图，PrimaryImageTag 为空说明
		// Emby 那边根本没有这个人的图（老 nfo 没有 <thumb> 就是这样）。
		ID              string `json:"Id"`
		Type            string `json:"Type"`
		PrimaryImageTag string `json:"PrimaryImageTag"`
	} `json:"People"`
}

func (r embyRawItem) toCatalog() CatalogItem {
	out := CatalogItem{
		ID:              r.ID,
		Name:            r.Name,
		OriginalTitle:   r.OriginalTitle,
		Type:            r.Type,
		Overview:        r.Overview,
		Path:            r.Path,
		SeriesName:      r.SeriesName,
		DateCreated:     r.DateCreated,
		ProductionYear:  r.ProductionYear,
		CommunityRating: r.CommunityRating,
		Genres:          r.Genres,
		Season:          r.ParentIndexNumber,
		Episode:         r.IndexNumber,
		EpisodeCount:    r.RecursiveItemCount,
		HasPoster:       r.ImageTags["Primary"] != "",
		HasBackdrop:     len(r.BackdropImageTags) > 0,
		IMDbID:          r.ProviderIds["Imdb"],
		TMDBID:          r.ProviderIds["Tmdb"],
	}
	if r.RunTimeTicks > 0 {
		out.RuntimeMinutes = int(r.RunTimeTicks / int64(60*10000000))
	}
	for _, p := range r.People {
		if p.Name == "" {
			continue
		}
		// People 保持「所有人的名字」，既有行为不变（导演也在里面）。
		out.People = append(out.People, p.Name)
		// Cast 只要演员：导演混进演员横排会让「主演」一栏里冒出导演的名字。
		if !strings.EqualFold(p.Type, "Actor") && p.Type != "" {
			continue
		}
		out.Cast = append(out.Cast, CastMember{
			Name:     p.Name,
			Role:     strings.TrimSpace(p.Role),
			PersonID: p.ID,
			HasImage: p.PrimaryImageTag != "",
		})
	}
	return out
}

func (e *emby) catalogQuery(q CatalogQuery) url.Values {
	v := url.Values{}
	v.Set("Recursive", "true")
	v.Set("Fields", catalogFields)
	if q.ParentID != "" {
		v.Set("ParentId", q.ParentID)
	}
	if len(q.Types) > 0 {
		v.Set("IncludeItemTypes", strings.Join(q.Types, ","))
	}
	if q.Search != "" {
		v.Set("SearchTerm", q.Search)
	}
	if q.Genre != "" {
		v.Set("Genres", q.Genre)
	}
	if q.SortBy != "" {
		v.Set("SortBy", q.SortBy)
	}
	if q.SortOrder != "" {
		v.Set("SortOrder", q.SortOrder)
	}
	if q.StartIndex > 0 {
		v.Set("StartIndex", strconv.Itoa(q.StartIndex))
	}
	if q.Limit > 0 {
		v.Set("Limit", strconv.Itoa(q.Limit))
	}
	return v
}

func (e *emby) Items(ctx context.Context, q CatalogQuery) (CatalogPage, error) {
	var raw struct {
		Items            []embyRawItem `json:"Items"`
		TotalRecordCount int           `json:"TotalRecordCount"`
	}
	if err := e.do(ctx, http.MethodGet, "/Items", e.catalogQuery(q), &raw); err != nil {
		return CatalogPage{}, err
	}
	out := CatalogPage{Total: raw.TotalRecordCount, Items: make([]CatalogItem, 0, len(raw.Items))}
	for _, r := range raw.Items {
		out.Items = append(out.Items, r.toCatalog())
	}
	return out, nil
}

func (e *emby) Item(ctx context.Context, id string) (CatalogItem, error) {
	if id == "" {
		return CatalogItem{}, fmt.Errorf("缺少条目 id")
	}
	v := url.Values{}
	v.Set("Ids", id)
	v.Set("Fields", catalogFields+",People")
	v.Set("Limit", "1")
	var raw struct {
		Items []embyRawItem `json:"Items"`
	}
	if err := e.do(ctx, http.MethodGet, "/Items", v, &raw); err != nil {
		return CatalogItem{}, err
	}
	if len(raw.Items) == 0 {
		return CatalogItem{}, fmt.Errorf("媒体服务器里没有这个条目（%s）", id)
	}
	return raw.Items[0].toCatalog(), nil
}

func (e *emby) Children(ctx context.Context, parentID string, types []string) ([]CatalogItem, error) {
	if parentID == "" {
		return nil, nil
	}
	q := CatalogQuery{ParentID: parentID, Types: types, Limit: 1000, SortBy: "SortName"}
	v := e.catalogQuery(q)
	v.Set("Fields", "Path,ProductionYear,ImageTags,ProviderIds,ParentIndexNumber,IndexNumber,Name,Type")
	var raw struct {
		Items []embyRawItem `json:"Items"`
	}
	if err := e.do(ctx, http.MethodGet, "/Items", v, &raw); err != nil {
		return nil, err
	}
	out := make([]CatalogItem, 0, len(raw.Items))
	for _, r := range raw.Items {
		out = append(out, r.toCatalog())
	}
	return out, nil
}

// DeleteItem 只从媒体服务器库里移除条目，不删任何文件。
// Emby 对不存在的 id 也返回 204，所以调用方不必先查存在性。
func (e *emby) DeleteItem(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("缺少条目 id")
	}
	return e.do(ctx, http.MethodDelete, "/Items/"+url.PathEscape(id), nil, nil)
}

// Image 把媒体服务器的图片透传给浏览器。
//
// 走服务端代理有两个原因：浏览器直连会暴露 API Key；Emby 常在容器里用
// 内网地址（http://emby:8096），用户浏览器根本连不上。
func (e *emby) Image(ctx context.Context, id, kind string, maxWidth int) ([]byte, string, error) {
	if id == "" {
		return nil, "", fmt.Errorf("缺少条目 id")
	}
	imgPath := "/Items/" + url.PathEscape(id) + "/Images/Primary"
	switch kind {
	case "backdrop":
		imgPath = "/Items/" + url.PathEscape(id) + "/Images/Backdrop/0"
	case "poster", "":
	default:
		return nil, "", fmt.Errorf("不支持的图片类型 %q", kind)
	}
	if maxWidth <= 0 || maxWidth > 2000 {
		maxWidth = 400
	}
	q := url.Values{}
	q.Set("maxWidth", strconv.Itoa(maxWidth))
	q.Set("api_key", e.apiKey)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.base+imgPath+"?"+q.Encode(), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("X-Emby-Token", e.apiKey)
	req.Header.Set("Authorization", `MediaBrowser Token="`+e.apiKey+`"`)
	resp, err := e.hc.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("取 %s 图片失败: %w", e.kind, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, "", fmt.Errorf("%s 图片返回 %d", e.kind, resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 12<<20))
	if err != nil {
		return nil, "", err
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/jpeg"
	}
	return data, ct, nil
}
