package mediaserver

// Catalog 是媒体服务器上「已入库条目」的读取与移除能力。
//
// 为什么不直接塞进 Server 接口：Server 的最小能力集只服务「建库 / 刷新」，
// 很多测试里的假实现只需要那几个方法。海报墙要的读条目、取图片、删条目
// 属于另一层能力，做成可选接口后用类型断言取用，新增实现不会被迫补齐。
//
// 数据方向是单向的：这里的「删」只把条目从媒体服务器的库里移走，
// 不动网盘文件；真正删 .strm / nfo / 网盘源文件由 httpapi 的编排负责。
import (
	"context"
	"errors"
)

// CatalogItem 是媒体服务器里的一条影视（电影 / 剧集 / 单集）。
// 字段刻意用中立命名，不绑死 Emby 的 BaseItemDto。
type CatalogItem struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	OriginalTitle   string   `json:"original_title,omitempty"`
	Type            string   `json:"type"` // Movie / Series / Episode / Season
	Overview        string   `json:"overview,omitempty"`
	ProductionYear  int      `json:"production_year,omitempty"`
	CommunityRating float64  `json:"community_rating,omitempty"`
	RuntimeMinutes  int      `json:"runtime_minutes,omitempty"`
	Genres          []string `json:"genres,omitempty"`
	Path            string   `json:"path,omitempty"`
	IMDbID          string   `json:"imdb_id,omitempty"`
	TMDBID          string   `json:"tmdb_id,omitempty"`
	HasPoster       bool     `json:"has_poster"`
	HasBackdrop     bool     `json:"has_backdrop"`
	SeriesName      string   `json:"series_name,omitempty"`
	Season          int      `json:"season,omitempty"`
	Episode         int      `json:"episode,omitempty"`
	DateCreated     string   `json:"date_created,omitempty"`
	EpisodeCount    int      `json:"episode_count,omitempty"`
	// People 只用于详情抽屉展示，列表里不拉（多一个大字段，整页响应会变大）。
	People []string `json:"people,omitempty"`
	// Cast 是带角色与头像的演员表。和 People 并存是因为 People 还被
	// 列表/搜索的「演员」筛选用到，换成结构体会牵动一片前端。
	Cast []CastMember `json:"cast,omitempty"`
}

// CastMember 是详情抽屉里的一位演员。
//
// 这里不拼图片地址：媒体服务器只知道「这个人有没有图」，怎么代理出去是
// HTTP 层的事。把 /api/catalog/image 写进这个包里，换一种前端就得改这里。
type CastMember struct {
	Name     string `json:"name"`
	Role     string `json:"role,omitempty"`
	PersonID string `json:"person_id,omitempty"`
	HasImage bool   `json:"has_image"`
}

// CatalogQuery 是列表查询。SortBy/SortOrder 直接对应媒体服务器的字段名。
type CatalogQuery struct {
	ParentID   string
	Types      []string
	Search     string
	Genre      string
	SortBy     string
	SortOrder  string
	StartIndex int
	Limit      int
}

type CatalogPage struct {
	Items []CatalogItem `json:"items"`
	Total int           `json:"total"`
}

// Catalog 由支持读条目的媒体服务器实现（目前是 Emby / Jellyfin）。
type Catalog interface {
	Items(ctx context.Context, q CatalogQuery) (CatalogPage, error)
	Item(ctx context.Context, id string) (CatalogItem, error)
	Children(ctx context.Context, parentID string, types []string) ([]CatalogItem, error)
	DeleteItem(ctx context.Context, id string) error
	// Image 返回图片字节与 Content-Type，由 HTTP 层代理给浏览器。
	Image(ctx context.Context, id, kind string, maxWidth int) ([]byte, string, error)
}

var ErrNoCatalog = errors.New("媒体服务器不支持读取条目")

// AsCatalog 把 Server 转成 Catalog；不支持时返回 ErrNoCatalog。
func AsCatalog(s Server) (Catalog, error) {
	if c, ok := s.(Catalog); ok {
		return c, nil
	}
	return nil, ErrNoCatalog
}
