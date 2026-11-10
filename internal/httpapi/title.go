package httpapi

// 影视详情的通用入口：GET /api/title?type=tv|movie&tmdb_id=... [&media_id=...]
//
// 为什么不复用 /api/catalog/item：那个接口的 key 只认 `emby:` 和 `local:`，
// 因为它本来是为「海报墙里已经有的片」服务的。而订阅列表、播出日历里的片
// 大多**还没入库**，手里只有一个 tmdb_id —— 于是那两处的片名只能是死文本。
//
// 这里按 (type, tmdb_id) 查，本地台账和订阅状态都是「有就带上」：
//   - 入库了：给库名、相对路径、已有集数、字幕状态，海报用磁盘上那张；
//   - 没入库：只给 TMDB 详情 + 一个订阅入口；
//   - TMDB 没拉到时：如果台账有这一条就用台账的字段，并说明来源。

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
	"strm115/internal/tmdb"
)

type titleLocalView struct {
	Exists    bool   `json:"exists"`
	MediaID   int64  `json:"media_id,omitempty"`
	LibraryID int64  `json:"library_id,omitempty"`
	Library   string `json:"library,omitempty"`
	RelPath   string `json:"rel_path,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Season    int    `json:"season,omitempty"`
	Episode   int    `json:"episode,omitempty"`
	// Episodes 是台账里已有的集数：剧集是行数，电影恒为 1（有就是有）。
	Episodes  int    `json:"episodes"`
	SubState  string `json:"sub_state,omitempty"`
	SubOrigin string `json:"sub_origin,omitempty"`
	SubLang   string `json:"sub_lang,omitempty"`
	Poster    string `json:"poster,omitempty"`
	Backdrop  string `json:"backdrop,omitempty"`

	// —— 目录跳转 ——
	// 这几个字段属于「媒体库」，通过 media.library_id 关联出来；store.Media
	// 本身只有 RelPath，没有网盘目录信息。
	RemoteCID   string `json:"remote_cid,omitempty"`    // 库根在 115 上的 cid
	RemotePath  string `json:"remote_path,omitempty"`   // 网盘显示路径（给人看）
	LocalRoot   string `json:"local_root,omitempty"`    // 磁盘上真实的库根
	LocalDir    string `json:"local_dir,omitempty"`     // 这条 .strm 所在目录的绝对路径（服务端视角）
	HostDir     string `json:"host_dir,omitempty"`      // Docker 部署时换算出的宿主机绝对路径
	LocalRelDir string `json:"local_rel_dir,omitempty"` // 相对库根，给「打开目录」接口用
	// RemoteDirCID 是这部片所在的 115 目录（解析不到就退回库根 cid）。
	// 解析要打 115，有超时和降级，详情接口不会被它拖垮。
	RemoteDirCID   string `json:"remote_dir_cid,omitempty"`
	RemoteDirExact bool   `json:"remote_dir_exact,omitempty"`
	// CatalogKey 是这条在海报墙里的 key（local:<media_id>）。详情抽屉的
	// 删除按钮直接拿它调 DELETE /api/catalog/item，不用前端再拼一遍。
	CatalogKey string `json:"catalog_key,omitempty"`
}

type titleSubView struct {
	ID               int64  `json:"id"`
	State            string `json:"state"`
	Quality          string `json:"quality"`
	WatermarkSeason  int    `json:"watermark_season"`
	WatermarkEpisode int    `json:"watermark_episode"`
	TotalEpisodes    int    `json:"total_episodes"`
	LastResult       string `json:"last_result,omitempty"`
	NextCheckAt      string `json:"next_check_at,omitempty"`
}

type titleDetail struct {
	Type     string   `json:"type"`
	TMDBID   int      `json:"tmdb_id"`
	IMDbID   string   `json:"imdb_id,omitempty"`
	Title    string   `json:"title"`
	Original string   `json:"original,omitempty"`
	Year     int      `json:"year,omitempty"`
	Rating   float64  `json:"rating,omitempty"`
	Runtime  string   `json:"runtime,omitempty"`
	Overview string   `json:"overview,omitempty"`
	Genres   []string `json:"genres,omitempty"`
	Cast     []string `json:"cast,omitempty"`
	Director []string `json:"director,omitempty"`
	Poster   string   `json:"poster,omitempty"`
	Backdrop string   `json:"backdrop,omitempty"`
	Seasons  []int    `json:"seasons,omitempty"`
	// SeasonEpisodes 给前端的「起播线」滑杆算长度用：每季各几集。
	// 和 Seasons 一起来自同一次 TMDB 请求，没有就留空，前端按单季兜底。
	SeasonEpisodes []tmdb.SeasonCount `json:"season_episodes,omitempty"`

	Local *titleLocalView `json:"local,omitempty"`
	Sub   *titleSubView   `json:"sub,omitempty"`
	// Note 只用来解释「这份数据是从哪来的」，不是错误提示。
	Note string `json:"note,omitempty"`
}

func (s *Server) handleTitleDetail(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	q := r.URL.Query()
	tmdbID, _ := strconv.Atoi(strings.TrimSpace(q.Get("tmdb_id")))
	mediaID, _ := strconv.ParseInt(strings.TrimSpace(q.Get("media_id")), 10, 64)
	kind := titleKind(q.Get("type"))
	name := strings.TrimSpace(q.Get("q"))
	year := strings.TrimSpace(q.Get("year"))

	// ① 有 media_id 就直接读台账：这是最可靠的起点，kind 和 tmdb_id 都能从行里拿。
	var media store.Media
	hasMedia := false
	if mediaID > 0 {
		m, ok, err := s.st.GetMediaByID(ctx, mediaID)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if ok {
			media, hasMedia = m, true
			if tmdbID <= 0 {
				tmdbID = m.TMDbID
			}
			if kind == "" {
				kind = titleKind(m.Kind)
			}
		}
	}
	if kind == "" {
		kind = "tv"
	}

	// ② 只有片名：整理记录、首页最近入库这类页面手里没有 tmdb_id，只有
	//    「叫什么、哪年的」。先按片名+年份问 TMDB 换一个 id，后面反查台账和
	//    订阅就能走和别人完全相同的那条路。查不到就当用户没点开（404）。
	var preloaded *tmdb.MetaDetail
	if tmdbID <= 0 && name != "" {
		md, err := s.catalogTMDBMeta(ctx, s.catalogTMDBClient(ctx),
			tmdbKindOf(kind), 0, "", name, year)
		if err == nil && md != nil && md.ID > 0 {
			tmdbID = md.ID
			preloaded = md
		}
	}
	if tmdbID <= 0 && !hasMedia {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 tmdb_id 或 media_id，片名也没查到"))
		return
	}
	// ③ 只有 tmdb_id：反查台账，看这部片到底下没下、下到哪一集。
	if !hasMedia && tmdbID > 0 {
		if m, ok, err := s.st.FindMediaByTMDBID(ctx, ledgerKind(kind), tmdbID); err == nil && ok {
			media, hasMedia = m, true
		}
	}

	d := titleDetail{Type: kind, TMDBID: tmdbID}
	if hasMedia {
		if d.TMDBID <= 0 {
			d.TMDBID = media.TMDbID
		}
		d.IMDbID = media.IMDbID
		d.Title = firstNonEmptyLocal(media.MetaTitle, media.Title)
		d.Year = yearOfLocal(media.MetaYear, media.Year)
		d.Rating = parseFloatLocal(media.Rating)
		d.Overview = media.Overview
		d.Genres = splitComma(media.Genres)
		d.Cast = splitComma(media.Cast)
		d.Director = splitComma(media.Director)
		d.Runtime = media.Runtime
	}

	// ④ TMDB 只覆盖「展示字段」，本地状态一个不动。
	//    第 ② 步按片名查过的那份详情直接复用，别再发一次请求。
	var md *tmdb.MetaDetail
	var metaErr error
	if preloaded != nil {
		md = preloaded
	} else if d.TMDBID > 0 {
		md, metaErr = s.catalogTMDBMeta(ctx, s.catalogTMDBClient(ctx),
			tmdbKindOf(kind), d.TMDBID, d.IMDbID, d.Title, strconv.Itoa(d.Year))
	}
	switch {
	case metaErr != nil || md == nil:
		// 没入库又查不到 TMDB：这条详情就没有任何可显示的东西了。
		if !hasMedia {
			writeErr(w, http.StatusNotFound, fmt.Errorf(
				"TMDB 里没查到这部片，本地台账也没有"))
			return
		}
		d.Note = "TMDB 详情没拉到，下面用的是本地台账里的信息"
	default:
		if md.ID > 0 && d.TMDBID <= 0 {
			d.TMDBID = md.ID
		}
		if v := strings.TrimSpace(md.Title); v != "" {
			d.Title = v
		}
		if v := strings.TrimSpace(md.Original); v != "" {
			d.Original = v
		}
		if y := yearOfLocal(md.Year); y > 0 {
			d.Year = y
		}
		if md.Rating != "" {
			d.Rating = parseFloatLocal(md.Rating)
		}
		if v := strings.TrimSpace(md.Runtime); v != "" {
			d.Runtime = v
		}
		if v := strings.TrimSpace(md.Overview); v != "" {
			d.Overview = v
		}
		if len(md.Genres) > 0 {
			d.Genres = md.Genres
		}
		if len(md.Seasons) > 0 {
			d.Seasons = md.Seasons
		}
		if kind == "tv" && len(md.SeasonEpisodes) > 0 {
			d.SeasonEpisodes = md.SeasonEpisodes
		}
		if v := strings.TrimSpace(md.IMDbID); v != "" {
			d.IMDbID = v
		}
		d.Poster = strings.TrimSpace(md.Poster)
		d.Backdrop = strings.TrimSpace(md.Backdrop)
	}

	// ⑤ 本地台账视图。磁盘上的海报比远端 URL 快，所以最后再盖一次。
	if hasMedia {
		lv := &titleLocalView{
			Exists: true, MediaID: media.ID, LibraryID: media.LibraryID,
			RelPath: media.RelPath, Kind: media.Kind,
			Season: media.Season, Episode: media.Episode,
			SubState: media.SubState, SubOrigin: media.SubOrigin, SubLang: media.SubLang,
		}
		if lib, err := s.st.GetLibrary(ctx, media.LibraryID); err == nil {
			lv.Library = lib.Name
			lv.RemoteCID = lib.RemoteCID
			lv.RemotePath = lib.RemotePath
			root := s.libRoot(lib)
			lv.LocalRoot = root
			relDir := path.Dir(filepath.ToSlash(media.RelPath))
			if relDir == "." || relDir == "/" {
				relDir = ""
			}
			lv.LocalRelDir = relDir
			lv.LocalDir = filepath.Clean(filepath.Join(root, filepath.FromSlash(relDir)))
			lv.HostDir = s.hostLocalPath(lv.LocalDir)
			lv.RemoteDirCID, lv.RemoteDirExact = s.resolveRemoteDir(ctx, lib, media)
		}
		if strings.TrimSpace(media.PosterFile) != "" {
			lv.Poster = fmt.Sprintf("/api/media/%d/image", media.ID)
		}
		if strings.TrimSpace(media.BackdropFile) != "" {
			lv.Backdrop = fmt.Sprintf("/api/media/%d/image?kind=backdrop", media.ID)
		}
		n, err := s.st.CountMediaByTMDBID(ctx, ledgerKind(kind), d.TMDBID)
		if err == nil {
			lv.Episodes = n
		}
		if kind == "movie" && lv.Episodes == 0 {
			// tmdb_id 没刮上的老行也要显示「有」，不能因为数不出来就当成没有。
			lv.Episodes = 1
		}
		lv.CatalogKey = localKey(media.ID)
		d.Local = lv
		if lv.Poster != "" {
			d.Poster = lv.Poster
		}
		if lv.Backdrop != "" {
			d.Backdrop = lv.Backdrop
		}
	}

	// ⑥ 订了没有 —— 这是详情抽屉里唯一一个会改状态的地方。
	if d.TMDBID > 0 {
		sub, err := s.st.FindSubscriptionByTMDB(ctx, kind, d.TMDBID)
		if err == nil && sub != nil {
			d.Sub = &titleSubView{
				ID: sub.ID, State: sub.State, Quality: sub.Quality,
				WatermarkSeason: sub.WatermarkSeason, WatermarkEpisode: sub.WatermarkEpisode,
				TotalEpisodes: sub.TotalEpisodes, LastResult: sub.LastResult,
				NextCheckAt: sub.NextCheckAt.Format(time.RFC3339),
			}
		}
	}

	writeJSON(w, http.StatusOK, d)
}

// resolveRemoteDir 把「这一集所在的 115 目录」解析成 cid，用于详情抽屉的
// 「打开网盘目录」。
//
// 为什么不能拿 remote_path + rel_path 拼：网盘里同名目录可以有好几层，
// remote_path 还可能只存了末级（见 handleLibraryRelDir 的注释）。这里走
// 115 自己记的 ParentID 往上爬，爬到「父目录 == 库根」的那层就是片目录，
// 剧集会连带跳过 Season 目录，落到整部剧的文件夹。
//
// 这是纯装饰信息：没台账、115 风控、超时、目录已删，一律返回空，前端
// 退回「打开库根」。绝不能让一次打不开的目录把整个详情接口拖成 500。
func (s *Server) resolveRemoteDir(ctx context.Context, lib store.Library,
	media store.Media) (string, bool) {

	rootCID := strings.TrimSpace(lib.RemoteCID)
	if rootCID == "" || s.pool == nil || ctx.Err() != nil {
		return "", false
	}
	f, ok, err := s.st.GetFileByRelPath(ctx, lib.ID, media.RelPath)
	if err != nil || !ok || strings.TrimSpace(f.FileID) == "" {
		return "", false
	}

	cctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	mgr, err := s.pool.Get(cctx, lib.AccountID)
	if err != nil {
		return "", false
	}
	e, err := mgr.Stat(cctx, f.FileID)
	if err != nil || e.ParentID == "" || e.ParentID == rootCID {
		return "", false
	}
	cur := e.ParentID
	for depth := 0; depth < 4; depth++ {
		p, err := mgr.Stat(cctx, cur)
		if err != nil {
			return cur, false
		}
		if p.ParentID == rootCID {
			return cur, true
		}
		if p.ParentID == "" || p.ParentID == cur {
			return cur, false
		}
		cur = p.ParentID
	}
	return cur, false
}

// titleKind 把各种写法归一到前端用的 tv / movie。认不出来返回空，
// 由调用方决定默认值 —— 这里不要偷偷替调用方猜。
func titleKind(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "tv", "series", "show", "episode", "tvshow":
		return "tv"
	case "movie", "film":
		return "movie"
	}
	return ""
}

// ledgerKind 是同一件事在 media_meta 里的写法（series / movie）。
func ledgerKind(kind string) string {
	if kind == "movie" {
		return "movie"
	}
	return "series"
}

// tmdbKindOf 是 tmdb 包里的写法（tv / movie），正好和前端一致。
func tmdbKindOf(kind string) string {
	if kind == "movie" {
		return "movie"
	}
	return "tv"
}
