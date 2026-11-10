package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/store"
	"strm115/internal/subscribe"
	"strm115/internal/tmdb"
)

// 订阅的全局设置只有一项：多久检查一次。
//
// 单部剧的周期不放在这里：追平之后由 TMDB 的下一集日期决定，有新集没资源
// 时是 10 分钟重试，这些都不该让用户配 —— 配了也只会配错。
const settingSubInterval = "sub_interval_minutes"

const defaultSubInterval = 360 // 6 小时

func (s *Server) subInterval(ctx context.Context) time.Duration {
	raw := strings.TrimSpace(s.setting(ctx, settingSubInterval))
	if raw == "" {
		return defaultSubInterval * time.Minute
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 15 {
		// 15 分钟是下限：比这更密对 TMDB 和 115 都不友好，而且没有意义。
		return defaultSubInterval * time.Minute
	}
	if n > 7*24*60 {
		n = 7 * 24 * 60
	}
	return time.Duration(n) * time.Minute
}

// handleListSubs 列出订阅 + 分类计数 + 全局配置。
//
// 一次返回三样：前端顶部要显示「追更中 3 · 等待 1」，一进页面就打三个请求
// 会让空库时的首屏闪一下。
func (s *Server) handleListSubs(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	items, err := s.st.ListSubscriptions(ctx, r.URL.Query().Get("state"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	counts, err := s.st.CountSubscriptions(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items":  items,
		"counts": counts,
		// 没填自己的 Key 时用的是内置那把（公共配额，会被限流）。
		// 前端据此提示「建议填自己的 Key」，而不是等用户发现订阅半天不动。
		"builtin_tmdb":     strings.TrimSpace(s.setting(ctx, "tmdb_api_key")) == "",
		"interval_minutes": int(s.subInterval(ctx) / time.Minute),
	})
}

// handleCreateSub 建一条订阅。
//
// 剧集一律以 TMDB 的元数据为准，前端传的 title 只作兜底：订阅靠 tmdb_id
// 判断集数，标题和它不一致时页面会显示错的名字，比空着更误导。
func (s *Server) handleCreateSub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TMDbID        int    `json:"tmdb_id"`
		MediaType     string `json:"media_type"`
		Title         string `json:"title"`
		OriginalTitle string `json:"original_title"`
		Year          int    `json:"year"`
		IMDbID        string `json:"imdb_id"`
		LibraryID     int64  `json:"library_id"`
		SaveCID       string `json:"save_cid"`
		Quality       string `json:"quality"`
		MinSize       int64  `json:"min_size"`
		MaxSize       int64  `json:"max_size"`
		// 起播线：从这一集开始追（含这一集），之前的集不补。0/0 = 从头追。
		// 只给季不给集是合法的（S02E00 表示「S01 整季跳过，从 S02E01 开始」）。
		WatermarkSeason  int `json:"watermark_season"`
		WatermarkEpisode int `json:"watermark_episode"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	if body.TMDbID <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 tmdb_id"))
		return
	}
	if body.MediaType != "movie" {
		body.MediaType = "tv"
	}
	if !validEpisodePair(body.WatermarkSeason, body.WatermarkEpisode) {
		writeErr(w, http.StatusBadRequest, errors.New("起播线数值不合理"))
		return
	}
	// 电影没有集数，起播线对它没有意义，直接忽略。
	if body.MediaType == "movie" {
		body.WatermarkSeason, body.WatermarkEpisode = 0, 0
	}

	ctx := r.Context()
	sub := store.Subscription{
		TMDbID:           body.TMDbID,
		MediaType:        body.MediaType,
		Title:            strings.TrimSpace(body.Title),
		OriginalTitle:    strings.TrimSpace(body.OriginalTitle),
		Year:             body.Year,
		IMDbID:           strings.TrimSpace(body.IMDbID),
		LibraryID:        body.LibraryID,
		SaveCID:          strings.TrimSpace(body.SaveCID),
		Quality:          strings.TrimSpace(body.Quality),
		MinSize:          body.MinSize,
		MaxSize:          body.MaxSize,
		WatermarkSeason:  body.WatermarkSeason,
		WatermarkEpisode: body.WatermarkEpisode,
	}

	id, calendarTotal, err := s.createSubscription(ctx, &sub)
	if err != nil {
		var ce *subCreateError
		switch {
		case errors.As(err, &ce):
			writeErr(w, ce.status, ce.err)
		case errors.Is(err, store.ErrSubExists):
			writeErr(w, http.StatusConflict, err)
		default:
			writeErr(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "calendar_total": calendarTotal})
}

// subCreateError 给「创建订阅」的失败附一个 HTTP 状态。
//
// 网页和 Telegram 共用 createSubscription：网页要把它翻成 400/502，机器人
// 只要一句人话。状态留在错误里，由 HTTP 那层自己取，机器人直接看 Error()。
type subCreateError struct {
	status int
	err    error
}

func (e *subCreateError) Error() string { return e.err.Error() }
func (e *subCreateError) Unwrap() error { return e.err }

// createSubscription 是「建一条订阅」的唯一入口：网页的 POST 和 Telegram
// 的订阅按钮都走这里，保证两边都拿到同一种校正结果和同一份专有日历。
//
// sub 传指针：剧集订阅会用 TMDB 就地校正标题、年份、IMDb ID 与总集数，
// 调用方拿到的就是最终写进库的那份（回执要显示校正后的中文名）。
// 返回 (订阅 id, 生成的日历集数, error)。日历生成失败不回滚订阅。
func (s *Server) createSubscription(ctx context.Context, sub *store.Subscription) (int64, int, error) {
	// 剧集：用 TMDB 校正标题、年份、IMDb ID 和总集数。
	var show *tmdb.TVShow
	if sub.MediaType == "tv" {
		client := s.tmdbClient(ctx)
		if client != nil {
			got, err := client.TV(ctx, sub.TMDbID)
			if err != nil {
				// 查不到就不让建：建一条判断不了集数的订阅，只会永远停在 waiting。
				return 0, 0, &subCreateError{
					status: http.StatusBadGateway,
					err:    errors.New("查不到这部剧的集数信息：" + err.Error()),
				}
			}
			show = got
			sub.Title = firstNonEmptyLocal(show.Name, sub.Title)
			sub.OriginalTitle = firstNonEmptyLocal(show.OriginalName, sub.OriginalTitle)
			if sub.Year == 0 {
				sub.Year = show.Year
			}
			sub.IMDbID = firstNonEmptyLocal(show.IMDb, sub.IMDbID)
			sub.TotalEpisodes = show.TotalEpisodes
		}
	}
	if strings.TrimSpace(sub.Title) == "" {
		return 0, 0, &subCreateError{
			status: http.StatusBadRequest, err: errors.New("缺少片名"),
		}
	}

	id, err := s.st.CreateSubscription(ctx, *sub)
	if err != nil {
		return 0, 0, err
	}

	// 复用刚才那次 TV() 的结果，不再打一遍 TMDB。
	calendarTotal := 0
	if sub.MediaType == "tv" && show != nil {
		if client := s.tmdbClient(ctx); client != nil {
			withID := *sub
			withID.ID = id
			n, calErr := s.materializeCalendar(ctx, withID, show, client)
			if calErr != nil {
				s.log.Warn("订阅建好了，但专有日历没生成", "id", id, "err", calErr)
			} else {
				calendarTotal = n
			}
		}
	}
	return id, calendarTotal, nil
}

// handleUpdateSub 改画质、体积、库、目录、状态。
//
// 水位线不在这里（见 store.UpdateSubscription 的注释）：让表单能改
// 「已经下到第几集」，改错一次就是重复下载几十集。
func (s *Server) handleUpdateSub(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("订阅 id 不合法"))
		return
	}
	// 先确认这条还在：UPDATE 影响 0 行不会报错，以前会给不存在的 id 回
	// {"ok":true}，用户以为起播线改上了，其实什么都没发生。
	exists, err := s.st.GetSubscription(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if exists == nil {
		writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		return
	}
	var body struct {
		Quality   string `json:"quality"`
		MinSize   *int64 `json:"min_size"`
		MaxSize   *int64 `json:"max_size"`
		LibraryID *int64 `json:"library_id"`
		SaveCID   string `json:"save_cid"`
		State     string `json:"state"`
		// 起播线（原水位线）：两个都给了才改，只给一个视为非法 —— 单改季不单改集，
		// 否则 S02 配 E09 这种组合在跨季时会指到一个不存在的集。
		WatermarkSeason  *int `json:"watermark_season"`
		WatermarkEpisode *int `json:"watermark_episode"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	if body.State != "" && !validSubState(body.State) {
		writeErr(w, http.StatusBadRequest, errors.New("状态只能是 watching / waiting / done / paused"))
		return
	}
	moveWatermark := body.WatermarkSeason != nil || body.WatermarkEpisode != nil
	if moveWatermark {
		if body.WatermarkSeason == nil || body.WatermarkEpisode == nil {
			writeErr(w, http.StatusBadRequest, errors.New("起播线要季和集一起给"))
			return
		}
		if !validEpisodePair(*body.WatermarkSeason, *body.WatermarkEpisode) {
			writeErr(w, http.StatusBadRequest, errors.New("起播线数值不合理"))
			return
		}
	}
	// -1 表示「这一项不改」：指针为空时用 -1 传下去，store 那边据此跳过。
	minSize, maxSize, libID := int64(-1), int64(-1), int64(-1)
	if body.MinSize != nil {
		minSize = *body.MinSize
	}
	if body.MaxSize != nil {
		maxSize = *body.MaxSize
	}
	if body.LibraryID != nil {
		libID = *body.LibraryID
	}

	if err := s.st.UpdateSubscription(r.Context(), id, body.Quality,
		minSize, maxSize, libID, strings.TrimSpace(body.SaveCID), body.State); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if moveWatermark {
		if err := s.st.ForceSubscriptionWatermark(r.Context(), id,
			*body.WatermarkSeason, *body.WatermarkEpisode); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		// 专有日历要跟着起播线走：往后挪 = 起始集之前的集不再搜；往前挪 =
		// 之前跳过的集（含新的起始集）恢复待搜。只改订阅表不改日历的话，
		// 界面和轮询器会各说各话。
		if err := s.st.SkipEpisodesBefore(r.Context(), id,
			*body.WatermarkSeason, *body.WatermarkEpisode); err != nil {
			s.log.Warn("同步起播线到专有日历失败", "id", id, "err", err)
		}
		if err := s.st.UnskipEpisodesAfter(r.Context(), id,
			*body.WatermarkSeason, *body.WatermarkEpisode,
			time.Now().Format("2006-01-02")); err != nil {
			s.log.Warn("恢复起播线之后的集失败", "id", id, "err", err)
		}
		// 手改起播线是高影响操作（往前挪会直接跳过若干集），留一行日志备查。
		s.log.Info("订阅起播线被手改",
			"id", id, "season", *body.WatermarkSeason, "episode", *body.WatermarkEpisode)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSub(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("订阅 id 不合法"))
		return
	}
	ok, err := s.st.DeleteSubscription(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if !ok {
		writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		return
	}
	// 订阅没了，它那份专有日历也不能留：否则周历里会冒出没有归属的集。
	if err := s.st.DeleteSubscriptionEpisodes(r.Context(), id); err != nil {
		s.log.Warn("删除订阅专有日历失败", "id", id, "err", err)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleRunSub 立即检查一条订阅。
//
// 同步执行并返回结论：用户点「立即检查」就是想马上知道「有没有新集、交没交上去」，
// 回一句「已提交检查」然后让他自己去日志里找，等于没做。
// 最坏情况（TMDB 两次 + 搜 30 秒 + 提交 90 秒）在 CheckOne 里有 3 分钟上限。
func (s *Server) handleRunSub(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("订阅 id 不合法"))
		return
	}
	if s.subs == nil {
		writeErr(w, http.StatusServiceUnavailable, errors.New("订阅服务没启动"))
		return
	}
	result, err := s.subs.RunNow(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, subscribe.ErrBusy):
			writeErr(w, http.StatusConflict, errors.New("这条订阅正在检查，稍等几秒"))
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, http.StatusNotFound, errors.New("订阅不存在"))
		default:
			writeErr(w, http.StatusBadGateway, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": result})
}

// handleSubSeasons 返回一部剧的季清单，给「建订阅时选从哪一季开始」用。
func (s *Server) handleSubSeasons(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.Atoi(strings.TrimSpace(r.URL.Query().Get("tmdb_id")))
	if id <= 0 {
		writeErr(w, http.StatusBadRequest, errors.New("缺少 tmdb_id"))
		return
	}
	client := s.tmdbClient(r.Context())
	if client == nil {
		writeErr(w, http.StatusBadRequest, errors.New("先到设置里填 TMDB API Key"))
		return
	}
	show, err := client.TV(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"show":    show,
		"seasons": show.Seasons,
	})
}

// handleGetSubConfig / handlePutSubConfig 只管「多久检查一次」。
func (s *Server) handleGetSubConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"interval_minutes": int(s.subInterval(r.Context()) / time.Minute),
		"builtin_tmdb":     strings.TrimSpace(s.setting(r.Context(), "tmdb_api_key")) == "",
	})
}

func (s *Server) handlePutSubConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IntervalMinutes int `json:"interval_minutes"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	if body.IntervalMinutes < 15 || body.IntervalMinutes > 7*24*60 {
		writeErr(w, http.StatusBadRequest,
			errors.New("检查周期只能在 15 分钟到 7 天之间"))
		return
	}
	if err := s.st.SetSetting(r.Context(), settingSubInterval,
		strconv.Itoa(body.IntervalMinutes)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"interval_minutes": body.IntervalMinutes,
	})
}

// validEpisodePair 是季集号的统一边界：0 是合法的（0/0 = 不限 / 还没开始）。
func validEpisodePair(season, episode int) bool {
	return season >= 0 && episode >= 0 && season <= 99 && episode <= 999
}

func validSubState(st string) bool {
	switch st {
	case store.SubWatching, store.SubWaiting, store.SubDone, store.SubPaused:
		return true
	}
	return false
}

// tmdbClient 按当前设置建客户端。
//
// 每次新建 Client，但 http.Client 的 Transport 是 Go 的默认共享实例，
// 连接池不会因此重建 —— 这里不需要再做一个进程内单例。
//
// 上游地址由 tmdb.SetUpstream 在进程级统一切换（电影墙、演员刮削、订阅
// 共用），这里**不要**再单独设 c.Base：只在订阅生效的反代，等于用户配了
// 地址却只有订阅快、榜单和头像还是直连超时。
func (s *Server) tmdbClient(ctx context.Context) *tmdb.Client {
	key := strings.TrimSpace(s.tmdbKey(ctx))
	if key == "" {
		return nil
	}
	return tmdb.NewClient(key)
}
