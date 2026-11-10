package httpapi

// 媒体目录（海报墙）的「查」。
//
// 和 /api/wall 的分工：/api/wall 是 TMDB 榜单，用来「发现想看什么」；
// 这里只列「网盘里已经有、并且进了媒体库」的影视，用来管理。
//
// 数据源优先级：
//  1. TMDB（默认）——卡片骨架来自本地台账（只有它知道网盘里有什么），
//     标题 / 评分 / 简介 / 类型 / 海报按台账里的 tmdb_id 现查 TMDB；
//  2. Emby ——它读的是我们写在 .strm 旁边的 nfo，是媒体服务器里最终
//     生效的那一份，想核对服务器实际看到的元数据时切过去；
//  3. 本地刮削台账 ——不联网、纯本地，用来看「网盘里有但 Emby 里没有」的片，
//     以及 TMDB 反代挂掉时的兜底。
//
// 卡片通过路径把两边对起来：Emby 的 Path → 反推库与 rel_path → 台账。
// 所有对 Emby 的图片请求都走服务端代理，浏览器不接触 API Key 与内网地址。
import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"strm115/internal/mediaserver"
	"strm115/internal/store"
)

// catalogCard 是海报墙上一张卡片（也是详情抽屉的数据源）。
type catalogCard struct {
	Key       string   `json:"key"` // emby:<id> / local:<media id>
	ID        string   `json:"id,omitempty"`
	Name      string   `json:"name"`
	Original  string   `json:"original,omitempty"`
	Type      string   `json:"type"` // movie / series
	Year      int      `json:"year,omitempty"`
	Rating    float64  `json:"rating,omitempty"`
	Overview  string   `json:"overview,omitempty"`
	Genres    []string `json:"genres,omitempty"`
	Poster    string   `json:"poster,omitempty"`
	Backdrop  string   `json:"backdrop,omitempty"`
	IMDbID    string   `json:"imdb_id,omitempty"`
	TMDBID    string   `json:"tmdb_id,omitempty"`
	LibraryID int64    `json:"library_id,omitempty"`
	Library   string   `json:"library,omitempty"`
	RelPath   string   `json:"rel_path,omitempty"`
	Path      string   `json:"path,omitempty"`
	MediaID   int64    `json:"media_id,omitempty"`
	NFO       bool     `json:"nfo"`
	SubState  string   `json:"sub_state,omitempty"`
	Episodes  int      `json:"episodes,omitempty"`
	Size      int64    `json:"size,omitempty"`
	Added     string   `json:"added,omitempty"`
	InEmby    bool     `json:"in_emby"`
	InLocal   bool     `json:"in_local"`
	People    []string `json:"people,omitempty"`
	// Cast 是带头像的演员表，只在详情里返回（列表不拉）。
	Cast []castCard `json:"cast,omitempty"`
}

// castCard 是详情抽屉里的一张演员头像卡。
type castCard struct {
	Name  string `json:"name"`
	Role  string `json:"role,omitempty"`
	Image string `json:"image,omitempty"` // 本地代理地址；空表示这个人没有图
}

// castImageURL 走媒体服务器的图片代理取演员头像。
//
// 160 是「一排头像」的尺寸：Emby 的 list 视图和我们的详情抽屉都在这个量级，
// 再大就是白花流量。没有图的演员返回空串，前端据此显示姓名首字，
// 而不是一个 404 的 <img>（那会是一片破图图标，比没图更难看）。
func castImageURL(p mediaserver.CastMember) string {
	if !p.HasImage || p.PersonID == "" {
		return ""
	}
	return "/api/catalog/image?key=" + url.QueryEscape(embyKey(p.PersonID)) + "&kind=poster&w=160"
}

func splitCatalogKey(key string) (source, id string) {
	if i := strings.IndexByte(key, ':'); i > 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}

func embyKey(id string) string { return "emby:" + id }
func localKey(id int64) string { return "local:" + strconv.FormatInt(id, 10) }

func catalogTypeOf(t string) string {
	if strings.EqualFold(t, "Series") || strings.EqualFold(t, "series") || strings.EqualFold(t, "Episode") {
		return "series"
	}
	return "movie"
}

func (s *Server) catalogServer(ctx context.Context) (mediaserver.Server, mediaserver.Catalog, error) {
	srv, err := s.mediaServer(ctx)
	if err != nil {
		return nil, nil, err
	}
	cat, err := mediaserver.AsCatalog(srv)
	if err != nil {
		return nil, nil, err
	}
	return srv, cat, nil
}

// catalogLocate 把媒体服务器看到的路径翻译回「哪个库 + 相对路径」。
//
// 比对的是 mapServerPath(库根) 而不是本机路径：两边挂不同卷时，
// server_path_from/to 正好描述了这层映射，直接用它算出服务器侧的根。
func (s *Server) catalogLocate(ctx context.Context, libs []store.Library, serverPath string) (store.Library, string, bool) {
	p := strings.TrimRight(strings.ReplaceAll(serverPath, "\\", "/"), "/")
	if p == "" {
		return store.Library{}, "", false
	}
	var best store.Library
	bestLen := -1
	bestRel := ""
	for _, lib := range libs {
		root := strings.TrimRight(strings.ReplaceAll(s.mapServerPath(ctx, s.libRoot(lib)), "\\", "/"), "/")
		if root == "" {
			continue
		}
		if p == root || strings.HasPrefix(p, root+"/") {
			if len(root) > bestLen {
				best, bestLen = lib, len(root)
				bestRel = strings.TrimPrefix(p, root)
				bestRel = strings.TrimPrefix(bestRel, "/")
			}
		}
	}
	if bestLen < 0 {
		return store.Library{}, "", false
	}
	return best, bestRel, true
}

func catalogSort(sortKey string) (by, order string) {
	switch sortKey {
	case "title":
		return "SortName", "Ascending"
	case "rating":
		return "CommunityRating", "Descending"
	case "year":
		return "ProductionYear", "Descending"
	default:
		return "DateCreated", "Descending"
	}
}

func (s *Server) handleCatalog(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()

	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	q := r.URL.Query()
	libID, _ := strconv.ParseInt(q.Get("library_id"), 10, 64)
	if libID < 0 {
		libID = 0
	}
	source := strings.ToLower(strings.TrimSpace(q.Get("source")))
	// 默认数据源：接了 Emby 就用 Emby —— 海报墙要回答的是「媒体服务器里
	// 现在有什么」，而 Emby 读的是我们写在 .strm 旁边的 nfo，是最终生效的那份。
	// 没接 Emby 时退回 TMDB 富集（而不是裸台账：那样海报会整页空）。
	switch source {
	case "tmdb", "emby", "local":
	default:
		if _, err := s.mediaServer(ctx); err == nil {
			source = "emby"
		} else {
			source = "tmdb"
		}
	}
	typ := q.Get("type")
	if typ != "movie" && typ != "series" {
		typ = "all"
	}
	genre := strings.TrimSpace(q.Get("genre"))
	search := strings.TrimSpace(q.Get("q"))
	sortKey := q.Get("sort")
	// refresh=1：强制这一页重新问一遍 TMDB（设置页改了反代 / 想让元数据跟上时用）。
	// 平时一律不联网，见下面 tmdb 分支的注释。
	forceTMDB := q.Get("refresh") == "1"
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 60 {
		limit = 30
	}

	var cards []catalogCard
	total := 0
	embyOK := false
	embyTotal := 0
	tmdbMissing := 0
	hint := ""

	switch source {
	case "emby":
		srv, cat, err := s.catalogServer(ctx)
		if err == nil {
			cards, total, embyTotal, err = s.catalogFromEmby(ctx, srv, cat, libs, libID, typ, search, genre, sortKey, page, limit)
			if err == nil {
				embyOK = true
			} else {
				hint = "读取 Emby 失败，已切到本地台账：" + err.Error()
			}
		} else {
			hint = "没连上 Emby，已切到本地台账：" + err.Error()
		}
		if !embyOK {
			source = "local"
			cards, total = s.catalogFromLocal(ctx, libs, libID, typ, search, genre, sortKey, page, limit)
		}
	case "tmdb":
		cards, total = s.catalogFromLocal(ctx, libs, libID, typ, search, genre, sortKey, page, limit)
		// 只给「连一张海报都没有」的卡片补 TMDB。
		//
		// 这一页原先无条件把整页卡片拿去现查 TMDB，而 TMDB 走的是用户自己的
		// 反代，单次 1.6–2.6 秒（4 并发，六张卡两轮）—— 这就是「打开影视库要等
		// 两三秒」的全部来源，而刮削早就把标题/评分/简介/海报 URL 落库了，
		// 这些请求等于每次都把已知的东西再问一遍。
		tmdbMissing = s.enrichCatalogTMDB(ctx, cards, forceTMDB)
		if tmdbMissing > 0 {
			hint = fmt.Sprintf("有 %d 张卡片没查到 TMDB 元数据，已保留本地台账的信息", tmdbMissing)
		}
	default: // local
		cards, total = s.catalogFromLocal(ctx, libs, libID, typ, search, genre, sortKey, page, limit)
	}

	genres, _ := s.st.MediaGenres(ctx, libID)
	localTotal := 0
	for _, lib := range libs {
		if libID > 0 && lib.ID != libID {
			continue
		}
		if n, err := s.st.CountMedia(ctx, lib.ID); err == nil {
			localTotal += n
		}
	}

	note := "按台账的 TMDB ID 现查元数据；想看媒体服务器里最终生效的 nfo 就切 Emby。"
	if source == "emby" || source == "local" {
		note = "Emby 里没有扫到的片子，可以切到「本地台账」查看，或在 Strm 管理里跑一次同步。"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":         cards,
		"total":         total,
		"page":          page,
		"limit":         limit,
		"has_more":      page*limit < total,
		"source":        source,
		"type":          typ,
		"genre":         genre,
		"q":             search,
		"sort":          defaultString(sortKey, "recent"),
		"library_id":    libID,
		"libraries":     catalogLibOptions(libs),
		"genres":        genres,
		"emby_ok":       embyOK,
		"emby_total":    embyTotal,
		"emby_checked":  source == "emby",
		"local_total":   localTotal,
		"tmdb_missing":  tmdbMissing,
		"hint":          hint,
		"provider_note": note,
	})
}

func catalogLibOptions(libs []store.Library) []map[string]any {
	out := make([]map[string]any, 0, len(libs))
	for _, l := range libs {
		out = append(out, map[string]any{"id": l.ID, "name": l.Name, "server_lib_id": l.ServerLibID})
	}
	return out
}

// catalogFromEmby 分页取 Emby 条目，并按路径补上本地台账信息。
//
// 指定库时直接用 ParentId 让 Emby 分页；跨库时没有单一 ParentId，
// 改成一次多取几条、按路径归属过滤后在内存里分页（库数量通常个位数）。
func (s *Server) catalogFromEmby(ctx context.Context, srv mediaserver.Server, cat mediaserver.Catalog, libs []store.Library,
	libID int64, typ, search, genre, sortKey string, page, limit int) ([]catalogCard, int, int, error) {

	by, order := catalogSort(sortKey)
	types := []string{"Movie", "Series"}
	if typ == "movie" {
		types = []string{"Movie"}
	} else if typ == "series" {
		types = []string{"Series"}
	}

	var lib store.Library
	if libID > 0 {
		found := false
		for _, l := range libs {
			if l.ID == libID {
				lib, found = l, true
				break
			}
		}
		if !found {
			return nil, 0, 0, errors.New("媒体库不存在")
		}
	}

	query := mediaserver.CatalogQuery{
		Types: types, Search: search, Genre: genre,
		SortBy: by, SortOrder: order, Limit: limit,
	}
	if libID > 0 {
		// 库没绑 Emby 库时先按路径认一次：不认的话 ParentID 为空，Emby 会
		// 把整台服务器的条目都返回，再按 StartIndex 切页 —— 分页与总数全错。
		if strings.TrimSpace(lib.ServerLibID) == "" {
			if id, ok := s.matchServerLib(ctx, srv, lib); ok {
				lib.ServerLibID = id
				// 记住绑定关系，下次直接按 ParentId 查。
				_ = s.st.SetLibraryServer(ctx, lib.ID, srv.Name(), id)
			}
		}
		query.ParentID = lib.ServerLibID
		query.StartIndex = (page - 1) * limit
	} else {
		// 1000 是安全阀：正常库远小于这个数；真超了也只是少显示后面的条目，
		// 不会把整面墙卡死。
		query.Limit = 1000
	}

	pg, err := cat.Items(ctx, query)
	if err != nil {
		return nil, 0, 0, err
	}

	// 富集前先算 rel_path，一次 IN 查询把台账取回来。
	type located struct {
		item mediaserver.CatalogItem
		lib  store.Library
		rel  string
	}
	var rows []located
	for _, it := range pg.Items {
		l, rel, ok := s.catalogLocate(ctx, libs, it.Path)
		if !ok {
			continue // 不是我们这几个库里的东西，别混进墙面
		}
		if libID > 0 && l.ID != libID {
			continue
		}
		rows = append(rows, located{it, l, rel})
	}

	total := pg.Total
	if libID == 0 {
		total = len(rows)
		start := (page - 1) * limit
		if start >= len(rows) {
			rows = nil
		} else {
			end := start + limit
			if end > len(rows) {
				end = len(rows)
			}
			rows = rows[start:end]
		}
	}

	mediaByLib := map[int64]map[string]store.Media{}
	relsByLib := map[int64][]string{}
	for _, row := range rows {
		relsByLib[row.lib.ID] = append(relsByLib[row.lib.ID], row.rel)
	}
	for id, rels := range relsByLib {
		got, err := s.st.ListMediaByRelPaths(ctx, id, rels)
		if err == nil {
			mediaByLib[id] = got
		}
	}

	// Emby 的剧集条目 Path 指向「剧目录」，台账里却是一集一行，精确查必然落空。
	// 对这类条目再按目录前缀查一次，卡片上的 nfo / 字幕 / 本地链接才不会丢。
	showEps := map[string][]store.Media{}
	for _, row := range rows {
		if row.item.Type != "Series" || mediaByLib[row.lib.ID][row.rel].ID > 0 {
			continue
		}
		eps, err := s.st.ListMediaByPrefix(ctx, row.lib.ID, row.rel)
		if err == nil && len(eps) > 0 {
			showEps[row.item.ID] = eps
		}
	}

	cards := make([]catalogCard, 0, len(rows))
	for _, row := range rows {
		m := mediaByLib[row.lib.ID][row.rel]
		eps := showEps[row.item.ID]
		if m.ID == 0 && len(eps) > 0 {
			m = catalogRepresentative(eps)
		}
		c := s.cardFromEmby(row.item, row.lib, row.rel, m)
		if len(eps) > c.Episodes {
			c.Episodes = len(eps)
		}
		cards = append(cards, c)
	}
	return cards, total, pg.Total, nil
}

// catalogRepresentative 从整部剧的分集记录里挑一条代表：优先「刮削成功 + 有海报」，
// 否则退到任意一条成功记录。卡片与详情抽屉都靠它显示本地状态。
func catalogRepresentative(eps []store.Media) store.Media {
	best := eps[0]
	for _, m := range eps {
		if m.Status == "ok" && strings.TrimSpace(m.PosterFile) != "" {
			return m
		}
		if m.Status == "ok" && best.Status != "ok" {
			best = m
		}
	}
	return best
}

func (s *Server) cardFromEmby(it mediaserver.CatalogItem, lib store.Library, rel string, m store.Media) catalogCard {
	key := embyKey(it.ID)
	c := catalogCard{
		Key:       key,
		ID:        it.ID,
		Name:      it.Name,
		Original:  it.OriginalTitle,
		Type:      catalogTypeOf(it.Type),
		Year:      it.ProductionYear,
		Rating:    it.CommunityRating,
		Overview:  it.Overview,
		Genres:    it.Genres,
		IMDbID:    it.IMDbID,
		TMDBID:    it.TMDBID,
		LibraryID: lib.ID,
		Library:   lib.Name,
		RelPath:   rel,
		Path:      it.Path,
		Episodes:  it.EpisodeCount,
		Added:     it.DateCreated,
		InEmby:    true,
		InLocal:   m.ID > 0,
		People:    it.People,
	}
	for _, p := range it.Cast {
		c.Cast = append(c.Cast, castCard{Name: p.Name, Role: p.Role, Image: castImageURL(p)})
	}
	if it.HasPoster {
		c.Poster = "/api/catalog/image?key=" + url.QueryEscape(key) + "&kind=poster&w=342"
	}
	if it.HasBackdrop {
		c.Backdrop = "/api/catalog/image?key=" + url.QueryEscape(key) + "&kind=backdrop&w=1000"
	}
	if m.ID > 0 {
		c.MediaID = m.ID
		if c.IMDbID == "" {
			c.IMDbID = m.IMDbID
		}
		c.NFO = strings.TrimSpace(m.NFOFile) != ""
		c.SubState = m.SubState
		if m.ID > 0 && !it.HasPoster && strings.TrimSpace(m.PosterFile) != "" {
			c.Poster = fmt.Sprintf("/api/media/%d/image", m.ID)
		}
		if !it.HasBackdrop && strings.TrimSpace(m.BackdropFile) != "" {
			c.Backdrop = fmt.Sprintf("/api/media/%d/image?kind=backdrop", m.ID)
		}
	}
	return c
}

// catalogFromLocal 用刮削台账拼一面墙。剧集按「剧根目录」合并成一张卡片，
// 否则一季 24 集就是 24 张一样的海报。
func (s *Server) catalogFromLocal(ctx context.Context, libs []store.Library, libID int64,
	typ, search, genre, sortKey string, page, limit int) ([]catalogCard, int) {

	type group struct {
		lib   store.Library
		items []store.Media
	}
	groups := map[string]*group{}
	order := []string{}
	for _, lib := range libs {
		if libID > 0 && lib.ID != libID {
			continue
		}
		ms, err := s.st.ListMediaAll(ctx, lib.ID)
		if err != nil {
			continue
		}
		for _, m := range ms {
			k := m.RelPath
			if m.Kind == "series" {
				k = catalogShowRoot(m.RelPath)
			}
			gk := fmt.Sprintf("%d|%s", lib.ID, k)
			g := groups[gk]
			if g == nil {
				g = &group{lib: lib}
				groups[gk] = g
				order = append(order, gk)
			}
			g.items = append(g.items, m)
		}
	}

	var cards []catalogCard
	needle := strings.ToLower(search)
	for _, gk := range order {
		g := groups[gk]
		best := g.items[0]
		for _, m := range g.items[1:] {
			if best.Status != "ok" && m.Status == "ok" {
				best = m
			}
		}
		ct := "movie"
		if best.Kind == "series" {
			ct = "series"
		}
		if typ != "all" && typ != ct {
			continue
		}
		name := firstNonEmptyLocal(best.MetaTitle, best.Title)
		if needle != "" && !strings.Contains(strings.ToLower(name), needle) &&
			!strings.Contains(strings.ToLower(best.RelPath), needle) {
			continue
		}
		genres := splitComma(best.Genres)
		if genre != "" && !containsFold(genres, genre) {
			continue
		}
		c := catalogCard{
			Key:       localKey(best.ID),
			Name:      name,
			Type:      ct,
			Rating:    parseFloatLocal(best.Rating),
			Year:      yearOfLocal(best.MetaYear, best.Year),
			Overview:  best.Overview,
			Genres:    genres,
			IMDbID:    best.IMDbID,
			TMDBID:    catalogTMDBID(best.TMDbID),
			LibraryID: g.lib.ID,
			Library:   g.lib.Name,
			RelPath:   best.RelPath,
			MediaID:   best.ID,
			NFO:       strings.TrimSpace(best.NFOFile) != "",
			SubState:  best.SubState,
			Episodes:  len(g.items),
			Added:     best.UpdatedAt.Format(time.RFC3339),
			InLocal:   true,
		}
		// 海报 / 背景图三级兜底，全都**不联网**：
		//   1. 磁盘上那张（/api/media/{id}/image，秒开）；
		//   2. 刮削时存下来的 TMDB 图片 URL（让浏览器自己去取）；
		//   3. 都没有 —— 这才是唯一值得为它等一次 TMDB 的情况。
		//
		// 剧集的每一集都有 media_meta 行，但 poster.jpg 通常只落在剧根那一行，
		// 所以要在整组里找图，而不是只看被选中的那一行（否则剧集卡片永远没海报，
		// 就只能靠 TMDB 现查，一页六张卡就是两三秒）。
		posterID, backdropID := catalogImageOwner(g.items)
		switch {
		case posterID > 0:
			c.Poster = fmt.Sprintf("/api/media/%d/image", posterID)
		case strings.TrimSpace(best.PosterURL) != "":
			c.Poster = strings.TrimSpace(best.PosterURL)
		}
		switch {
		case backdropID > 0:
			c.Backdrop = fmt.Sprintf("/api/media/%d/image?kind=backdrop", backdropID)
		case strings.TrimSpace(best.BackdropURL) != "":
			c.Backdrop = strings.TrimSpace(best.BackdropURL)
		}
		cards = append(cards, c)
	}

	sort.SliceStable(cards, func(i, j int) bool { return catalogLess(cards[i], cards[j], sortKey) })
	total := len(cards)
	start := (page - 1) * limit
	if start >= total {
		return []catalogCard{}, total
	}
	end := start + limit
	if end > total {
		end = total
	}
	return cards[start:end], total
}

// catalogImageOwner 在同一个作品的所有台账行里找「谁真的存了图」。
//
// 剧集的 poster.jpg/fanart.jpg 只落在剧根那一行，其余分集行的这两列是空的，
// 所以不能只看被选中的那条。
func catalogImageOwner(items []store.Media) (posterID, backdropID int64) {
	for _, m := range items {
		if posterID == 0 && strings.TrimSpace(m.PosterFile) != "" {
			posterID = m.ID
		}
		if backdropID == 0 && strings.TrimSpace(m.BackdropFile) != "" {
			backdropID = m.ID
		}
		if posterID != 0 && backdropID != 0 {
			break
		}
	}
	return posterID, backdropID
}

func catalogLess(a, b catalogCard, key string) bool {
	switch key {
	case "title":
		return strings.ToLower(a.Name) < strings.ToLower(b.Name)
	case "rating":
		if a.Rating != b.Rating {
			return a.Rating > b.Rating
		}
	case "year":
		if a.Year != b.Year {
			return a.Year > b.Year
		}
	default:
		if a.Added != b.Added {
			return a.Added > b.Added
		}
	}
	return a.Name < b.Name
}

// catalogShowRoot 找剧集的剧根目录：Season 1 / S01 / 第 1 季 这类目录要再上浮一层。
var reSeasonDir = regexp.MustCompile(`(?i)^(season\s*\d+|s\d{1,2}|第\s*\d+\s*季|Specials)$`)

func catalogShowRoot(rel string) string {
	dir := path.Dir(filepath.ToSlash(rel))
	if dir == "." || dir == "/" || dir == "" {
		return filepath.ToSlash(rel)
	}
	if reSeasonDir.MatchString(path.Base(dir)) {
		if parent := path.Dir(dir); parent != "." && parent != "/" && parent != "" {
			return parent
		}
	}
	return dir
}

func firstNonEmptyLocal(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func splitComma(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(strings.TrimSpace(v), strings.TrimSpace(want)) {
			return true
		}
	}
	return false
}

func parseFloatLocal(s string) float64 {
	f, _ := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f
}

func yearOfLocal(vals ...string) int {
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if len(v) >= 4 {
			if n, err := strconv.Atoi(v[:4]); err == nil && n > 1900 && n < 2200 {
				return n
			}
		}
	}
	return 0
}

// ==================== 详情与图片 ====================

func (s *Server) handleCatalogItem(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	src, id := splitCatalogKey(r.URL.Query().Get("key"))
	if id == "" {
		writeErr(w, http.StatusBadRequest, errors.New("缺少条目 key"))
		return
	}
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}

	var card catalogCard
	var lib store.Library
	var media store.Media
	episodes := []map[string]any{}

	switch src {
	case "emby":
		_, cat, err := s.catalogServer(ctx)
		if err != nil {
			writeErr(w, http.StatusBadGateway, fmt.Errorf("没连上 Emby: %w", err))
			return
		}
		it, err := cat.Item(ctx, id)
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		l, rel, ok := s.catalogLocate(ctx, libs, it.Path)
		if ok {
			lib = l
			if m, ok, _ := s.st.GetMedia(ctx, l.ID, rel); ok {
				media = m
			} else if it.Type == "Series" {
				// 剧集台账按分集存，目录本身没有记录，用前缀捞一条代表出来。
				if eps, err := s.st.ListMediaByPrefix(ctx, l.ID, rel); err == nil && len(eps) > 0 {
					media = catalogRepresentative(eps)
				}
			}
			if it.Type == "Series" {
				kids, err := cat.Children(ctx, it.ID, []string{"Episode"})
				if err == nil {
					for _, k := range kids {
						_, krel, found := s.catalogLocate(ctx, libs, k.Path)
						row := map[string]any{
							"name": k.Name, "season": k.Season, "episode": k.Episode,
							"path": k.Path, "rel_path": krel,
						}
						if found {
							if em, ok, _ := s.st.GetMedia(ctx, l.ID, krel); ok {
								row["sub_state"] = em.SubState
								row["nfo"] = strings.TrimSpace(em.NFOFile) != ""
								row["media_id"] = em.ID
							}
						}
						episodes = append(episodes, row)
					}
				}
			}
		}
		card = s.cardFromEmby(it, lib, rel, media)
	case "local":
		mid, _ := strconv.ParseInt(id, 10, 64)
		m, ok, err := s.st.GetMediaByID(ctx, mid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("本地台账里没有这个条目"))
			return
		}
		media = m
		for _, l := range libs {
			if l.ID == m.LibraryID {
				lib = l
				break
			}
		}
		card = s.cardFromMedia(m, lib)
		if m.Kind == "series" {
			all, _ := s.st.ListMediaAll(ctx, m.LibraryID)
			root := catalogShowRoot(m.RelPath)
			for _, em := range all {
				if em.Kind != "series" || catalogShowRoot(em.RelPath) != root {
					continue
				}
				episodes = append(episodes, map[string]any{
					"name":   firstNonEmptyLocal(em.MetaTitle, em.Title),
					"season": em.Season, "episode": em.Episode,
					"rel_path": em.RelPath, "sub_state": em.SubState,
					"nfo": strings.TrimSpace(em.NFOFile) != "", "media_id": em.ID,
				})
			}
		}
	default:
		writeErr(w, http.StatusBadRequest, errors.New("条目 key 不合法"))
		return
	}

	// 海报墙默认数据源是 TMDB：详情抽屉也用同一份详情覆盖展示字段，
	// 否则列表是 TMDB 的中文名 / 海报，点开却跳回台账里的旧名字。
	// 查不到就保留本地那份，不报错。
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("source")), "tmdb") {
		id, _ := strconv.Atoi(strings.TrimSpace(card.TMDBID))
		d, err := s.catalogTMDBMeta(ctx, s.catalogTMDBClient(ctx),
			catalogTMDBKindOf(card.Type), id, card.IMDbID,
			card.Name, strconv.Itoa(card.Year))
		if err == nil {
			applyCatalogTMDB(&card, d)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"item":     card,
		"episodes": episodes,
		"local":    catalogLocalView(media),
	})
}

func (s *Server) cardFromMedia(m store.Media, lib store.Library) catalogCard {
	c := catalogCard{
		Key:       localKey(m.ID),
		Name:      firstNonEmptyLocal(m.MetaTitle, m.Title),
		Type:      firstNonEmptyLocal(m.Kind, "movie"),
		Year:      yearOfLocal(m.MetaYear, m.Year),
		Rating:    parseFloatLocal(m.Rating),
		Overview:  m.Overview,
		Genres:    splitComma(m.Genres),
		IMDbID:    m.IMDbID,
		TMDBID:    catalogTMDBID(m.TMDbID),
		LibraryID: m.LibraryID,
		Library:   lib.Name,
		RelPath:   m.RelPath,
		MediaID:   m.ID,
		NFO:       strings.TrimSpace(m.NFOFile) != "",
		SubState:  m.SubState,
		Added:     m.UpdatedAt.Format(time.RFC3339),
		InLocal:   true,
		People:    splitComma(m.Cast),
	}
	if strings.TrimSpace(m.PosterFile) != "" {
		c.Poster = fmt.Sprintf("/api/media/%d/image", m.ID)
	}
	if strings.TrimSpace(m.BackdropFile) != "" {
		c.Backdrop = fmt.Sprintf("/api/media/%d/image?kind=backdrop", m.ID)
	}
	return c
}

// catalogLocalView 给编辑弹窗回填用，字段与 store.Media 对齐但不暴露内部状态。
func catalogLocalView(m store.Media) map[string]any {
	if m.ID == 0 {
		return map[string]any{"exists": false}
	}
	return map[string]any{
		"exists":    true,
		"id":        m.ID,
		"title":     firstNonEmptyLocal(m.MetaTitle, m.Title),
		"year":      firstNonEmptyLocal(m.MetaYear, m.Year),
		"overview":  m.Overview,
		"genres":    splitComma(m.Genres),
		"rating":    m.Rating,
		"imdb_id":   m.IMDbID,
		"tmdb_id":   catalogTMDBID(m.TMDbID),
		"sub_state": m.SubState,
		"nfo_file":  m.NFOFile,
		"sub_file":  m.SubFile,
		"rel_path":  m.RelPath,
		"status":    m.Status,
	}
}

func (s *Server) handleCatalogImage(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	src, id := splitCatalogKey(r.URL.Query().Get("key"))
	if id == "" {
		http.NotFound(w, r)
		return
	}
	kind := r.URL.Query().Get("kind")
	if src == "local" {
		// 本地海报已有现成接口：按 media id 反查路径，只可能读到我们自己写的图片。
		http.Redirect(w, r, fmt.Sprintf("/api/media/%s/image?kind=%s",
			url.PathEscape(id), url.QueryEscape(kind)), http.StatusFound)
		return
	}
	if src != "emby" {
		http.NotFound(w, r)
		return
	}
	maxWidth, _ := strconv.Atoi(r.URL.Query().Get("w"))
	_, cat, err := s.catalogServer(ctx)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, ct, err := cat.Image(ctx, id, kind, maxWidth)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=86400")
	_, _ = w.Write(data)
}

// matchServerLib 按「服务器上那个库的扫描路径」反查它是哪个库。
//
// 用户在 Emby 里建库时填的是服务器视角的路径（可能是 /media/电影，也可能是
// 别的挂载点），所以两边都过一次 mapServerPath 再比，而不是硬编码相等。
func (s *Server) matchServerLib(ctx context.Context, srv mediaserver.Server, lib store.Library) (string, bool) {
	libs, err := srv.Libraries(ctx)
	if err != nil {
		return "", false
	}
	root := path.Clean(mapServerPathClean(s.mapServerPath(ctx, s.libRoot(lib))))
	if root == "." || root == "" {
		return "", false
	}
	for _, sl := range libs {
		for _, p := range sl.Paths {
			got := path.Clean(strings.ReplaceAll(p, "\\", "/"))
			if got == root {
				return sl.ID, true
			}
		}
	}
	// 没完全相等时退一步：路径是库根的子目录也算（库指向上一层）。
	for _, sl := range libs {
		for _, p := range sl.Paths {
			got := path.Clean(strings.ReplaceAll(p, "\\", "/"))
			if strings.HasPrefix(root, got+"/") {
				return sl.ID, true
			}
		}
	}
	return "", false
}

func mapServerPathClean(p string) string { return strings.ReplaceAll(p, "\\", "/") }
