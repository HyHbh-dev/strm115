package subscribe

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"strm115/internal/resource"
	"strm115/internal/store"
	"strm115/internal/tmdb"
)

const (
	// tickEvery 是轮询器自己的心跳。它不等于「每部剧多久查一次」——
	// 那是每条订阅的 next_check_at，由检查结果决定（新集在等就 10 分钟，
	// 追平了就按用户设的周期）。心跳只负责「有没有到期的」。
	tickEvery = time.Minute

	// initialDelay 让进程刚起来时先别抢 115 的风控额度：同步和刮削
	// 往往也在这一段启动，堆在一起容易被限流。
	initialDelay = 45 * time.Second

	// maxCatchUp 是一轮最多补几集。首次订阅一部已经播了 24 集的剧，
	// 一次全提会被 115 当成刷单；一轮 3 集既快又不像机器人。
	maxCatchUp = 3

	// retryNoSource 是「查到有新集但还没资源」时的重试间隔。比正常周期短得多，
	// 因为资源通常在上线后几小时内陆续出现。
	retryNoSource = 10 * time.Minute
)

// ErrBusy 表示这条订阅正在被另一个调用检查（手动点 + 轮询撞车）。
var ErrBusy = errors.New("这条订阅正在检查中")

// Deps 是 Service 需要的外部能力。全部由 httpapi 注入，避免 subscribe 反向
// 依赖 Server —— 那样就没法单独测这个包。
type Deps struct {
	TMDb   func(context.Context) *tmdb.Client
	Search func(ctx context.Context, q, kind, en string, limit int) (*resource.Result, error)
	Submit func(ctx context.Context, accountID int64, uris []string, saveCID, label string) ([]string, error)
	// Interval 读用户设的检查周期。返回 0 或负数时用 6 小时兜底。
	// 只作用于电影；剧集订阅改由专有日历的播出日驱动。
	Interval func(context.Context) time.Duration
	// CalendarRefresh 重新从 TMDB 拉一部剧的集数集合并写回专有日历，返回集数。
	// 由 httpapi 注入，避免 subscribe 反向依赖 Server。为 nil 时退回旧逻辑。
	CalendarRefresh func(ctx context.Context, sub store.Subscription) (int, error)
}

// Service 是订阅轮询器。
type Service struct {
	st  *store.Store
	log *slog.Logger
	dep Deps

	mu      sync.Mutex
	busy    map[int64]bool
	started bool
}

// New 构造轮询器。构造不会启动任何 goroutine，要显式 Start。
func New(st *store.Store, log *slog.Logger, dep Deps) *Service {
	if log == nil {
		log = slog.Default()
	}
	return &Service{st: st, log: log, dep: dep, busy: map[int64]bool{}}
}

// Start 起后台循环。重复调用是安全的（第二次直接返回）。
func (s *Service) Start(ctx context.Context) {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop(ctx)
}

func (s *Service) loop(ctx context.Context) {
	t := time.NewTimer(initialDelay)
	select {
	case <-ctx.Done():
		t.Stop()
		return
	case <-t.C:
	}
	for {
		s.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(tickEvery):
		}
	}
}

// tick 扫一轮到期的订阅。串行处理：115 的离线上传对并发很敏感，
// 而且串行下日志顺序就是时间顺序，排查问题时不用对时间戳。
func (s *Service) tick(ctx context.Context) {
	// 专有日历驱动：按每集 air_date 搜资源。放在前面，因为刚订阅时新集
	// 立刻到期，先跑完这一轮再去刷新元数据，用户能更快看到结果。
	s.tickCalendar(ctx)

	due, err := s.st.DueSubscriptions(ctx, time.Now(), 10)
	if err != nil {
		s.log.Warn("读取到期订阅失败", "err", err)
		return
	}
	if len(due) == 0 {
		return
	}
	// 每分钟扫一次，绝大多数轮次是空的，所以只在这轮真有事做时记一行。
	// 没有这行日志时，「订阅到点了却没动静」根本没法判断是轮询没跑、
	// 还是检查跑了但结果没落库（后者只会静默地把 next_check_at 留在过去）。
	s.log.Info("订阅：这一轮有到期的", "count", len(due), "ids", subIDs(due))
	for _, sub := range due {
		if ctx.Err() != nil {
			return
		}
		if _, err := s.CheckOne(ctx, sub); err != nil && !errors.Is(err, ErrBusy) {
			s.log.Warn("订阅检查失败", "id", sub.ID, "title", sub.Title, "err", err)
		}
	}
}

func subIDs(subs []store.Subscription) []int64 {
	out := make([]int64, 0, len(subs))
	for _, s := range subs {
		out = append(out, s.ID)
	}
	return out
}

// RunNow 立即检查一条订阅，供「立即检查」按钮用。
//
// 和轮询走同一个 CheckOne，所以手动触发也会推进水位线、写 last_result ——
// 不存在「手动点出来的结果不进历史」这种两套账。
func (s *Service) RunNow(ctx context.Context, id int64) (string, error) {
	sub, err := s.st.GetSubscription(ctx, id)
	if err != nil {
		return "", err
	}
	if sub == nil {
		return "", store.ErrNotFound
	}
	return s.CheckOne(ctx, *sub)
}

// CheckOne 检查一条订阅并（可能）提交离线。
//
// 返回的一句话结论会写进 last_result，页面上直接显示，所以措辞是给用户看的：
// 「已追平（TMDB 最新 S01E10）」比「no pending episodes」有用得多。
func (s *Service) CheckOne(parent context.Context, sub store.Subscription) (string, error) {
	if !s.acquire(sub.ID) {
		return "", ErrBusy
	}
	defer s.release(sub.ID)

	// 一次检查最多三分钟：TMDB 两次 + 搜索 30 秒 + 离线提交 90 秒，
	// 再加余量。超过这个时间说明上游有问题，让下一轮再来。
	ctx, cancel := context.WithTimeout(parent, 3*time.Minute)
	defer cancel()

	result, err := s.check(ctx, sub)
	if err != nil {
		// 失败也要落库：不写 last_result 的话，页面上永远显示上一次的成功结果，
		// 用户会以为一切正常。
		n := sub.FailCount + 1
		s.log.Warn("订阅检查出错", "id", sub.ID, "title", sub.Title, "err", err, "fail", n)
		s.mark(ctx, sub, time.Now().Add(backoffDuration(n)), sub.State,
			"检查失败："+err.Error(), n)
		return "", err
	}
	s.log.Info("订阅检查完成", "id", sub.ID, "title", sub.Title, "result", result)
	return result, nil
}

func (s *Service) check(ctx context.Context, sub store.Subscription) (string, error) {
	if s.dep.TMDb == nil || s.dep.Search == nil || s.dep.Submit == nil {
		return "", errors.New("订阅所需的上游能力没接上")
	}
	client := s.dep.TMDb(ctx)
	if client == nil {
		return "", errors.New("没有可用的 TMDB Key")
	}
	interval := s.interval(ctx)

	if sub.MediaType == "movie" {
		return s.checkMovie(ctx, sub, interval)
	}
	// 剧集一律走专有日历：这一轮只刷新 TMDB 集数集合（新季 / 新增集），
	// 资源搜索由 tickCalendar 按每集播出日驱动。两条路径不能同时跑，
	// 否则同一集会被提交两次。
	//
	// 老订阅（升级前建的，订阅表里没有日历行）也走这里：materializeCalendar
	// 会顺手把它的集数集合补齐，等于自动迁移到播出日驱动，不需要人工干预。
	if s.dep.CalendarRefresh != nil {
		return s.refreshCalendar(ctx, sub)
	}
	// 没注入刷新能力时（单测 / 精简部署）才回退到旧的水位线轮询。
	return s.checkTV(ctx, sub, client, interval)
}

// checkMovie 是「有了就下一次」：电影没有集数概念，提交成功就结束。
//
// 已经 done 的不会再进来（DueSubscriptions 排除了 done），所以不会重复下载。
// 用户删了片想重下，把状态改回 watching 即可。
func (s *Service) checkMovie(ctx context.Context, sub store.Subscription, interval time.Duration) (string, error) {
	res, err := s.search(ctx, sub)
	if err != nil {
		return "", err
	}
	opt := PickOptions{Quality: sub.Quality, MinSize: sub.MinSize, MaxSize: sub.MaxSize}
	it := Pick(res.Items, opt)
	if it == nil {
		msg := Describe(len(res.Items), opt)
		// 没资源是常态（新片还没人压），按周期继续等，不算失败。
		s.mark(ctx, sub, time.Now().Add(interval), store.SubWaiting, msg, 0)
		return msg, nil
	}

	acctID, cid := s.target(ctx, sub)
	label := fmt.Sprintf("%s (%d)", sub.Title, sub.Year)
	if _, err := s.dep.Submit(ctx, acctID, []string{URI(*it)}, cid, label); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("已提交 %s", shortTitle(it.Title))
	s.mark(ctx, sub, time.Now().Add(interval), store.SubDone, msg, 0)
	return msg, nil
}

// checkTV 是主逻辑：问 TMDB 该出到第几集 → 比水位线 → 搜 → 挑 → 提交。
func (s *Service) checkTV(ctx context.Context, sub store.Subscription,
	client *tmdb.Client, interval time.Duration) (string, error) {

	show, err := client.TV(ctx, sub.TMDbID)
	if err != nil {
		return "", err
	}
	// 总集数变了就顺手更新，前端「3 / 10 集」才准。
	if show.TotalEpisodes > 0 && show.TotalEpisodes != sub.TotalEpisodes {
		if err := s.st.SetSubscriptionTotal(ctx, sub.ID, show.TotalEpisodes); err != nil {
			s.log.Warn("更新总集数失败", "id", sub.ID, "err", err)
		}
	}

	latest := show.Latest()
	if latest.Episode <= 0 {
		// 未开播 / TMDB 没填集数。这不是错误，是常态，按周期再看。
		msg := "TMDB 还没有集数信息（未开播或数据缺失）"
		s.mark(ctx, sub, time.Now().Add(interval), store.SubWaiting, msg, 0)
		return msg, nil
	}

	// 多要一集用来判断「后面还有没有」：pendingEpisodes 会按 maxCatchUp 截断，
	// 截断之后 len(picked)==len(pending) 看着像追平了，其实后面还有十几集。
	all := pendingEpisodes(sub.WatermarkSeason, sub.WatermarkEpisode, show, maxCatchUp+1)
	hasMore := len(all) > maxCatchUp
	pending := all
	if hasMore {
		pending = all[:maxCatchUp]
	}

	if len(pending) == 0 {
		state, msg := store.SubWaiting,
			fmt.Sprintf("已追平（TMDB 最新 S%02dE%02d）", latest.Season, latest.Episode)
		if show.Ended {
			state, msg = store.SubDone, "已完结并追平"
		}
		// 起播线在最新一集之后时，别只说追平：用户会以为订阅坏了。
		if sub.WatermarkEpisode > 0 && atOrBefore(latest.Season, latest.Episode,
			sub.WatermarkSeason, sub.WatermarkEpisode) {
			msg += fmt.Sprintf("；起播线已设到 S%02dE%02d，之后的集还没播",
				sub.WatermarkSeason, sub.WatermarkEpisode)
		}
		next := time.Now().Add(interval)
		// 已完结的剧不再有下一集，next 用不上了，随便给个远一点的；
		// 状态是 done，轮询器本来也不会再取它。
		if !show.Ended && show.NextAiring != nil {
			next = nextCheckAfterAir(show.NextAiring.AirDate, interval, time.Now())
		}
		s.mark(ctx, sub, next, state, msg, 0)
		return msg, nil
	}

	res, err := s.search(ctx, sub)
	if err != nil {
		return "", err
	}

	// 按集顺序挑，遇到没资源的就停：跳过中间某一集去下后面的，
	// 水位线就没法单调推进了（这也是 maxCatchUp 存在的另一半理由）。
	opt := PickOptions{Quality: sub.Quality, MinSize: sub.MinSize, MaxSize: sub.MaxSize}
	var uris []string
	var picked []tmdb.TVEpisode
	for _, ep := range pending {
		o := opt
		o.Season, o.Episode = ep.Season, ep.Episode
		it := Pick(res.Items, o)
		if it == nil {
			break
		}
		uris = append(uris, URI(*it))
		picked = append(picked, ep)
	}

	if len(uris) == 0 {
		// 查到有新集但没搜到资源：这是最常见的情况（刚播完还没人压）。
		// 用短间隔重试，不算失败 —— 算失败会把 fail_count 抬高，
		// 之后真出错时的退避就从更高的档位开始。
		first := pending[0]
		o := opt
		o.Season, o.Episode = first.Season, first.Episode
		msg := Describe(len(res.Items), o)
		s.mark(ctx, sub, time.Now().Add(retryNoSource), store.SubWaiting, msg, 0)
		return msg, nil
	}

	acctID, cid := s.target(ctx, sub)
	label := sub.Title + " " + episodeSpan(picked)
	if _, err := s.dep.Submit(ctx, acctID, uris, cid, label); err != nil {
		return "", err
	}

	last := picked[len(picked)-1]
	if err := s.st.SetSubscriptionWatermark(ctx, sub.ID, last.Season, last.Episode); err != nil {
		// 提交已经成功，水位线没存上是严重问题：下一轮会重复提交同一集。
		// 明确报出来，别让用户看到「已提交」却发现库里有两份。
		return "", fmt.Errorf("已提交但水位线没存上（下一轮可能重复下载）：%w", err)
	}

	msg := "已提交 " + episodeSpan(picked)
	switch {
	case len(picked) < len(pending) || hasMore:
		// 还有没排上的集（这一集没资源，或者这一轮已经补满 3 集）：
		// 10 分钟后再来一轮，别等满周期。
		s.mark(ctx, sub, time.Now().Add(retryNoSource), store.SubWatching, msg, 0)
	default:
		// 所有已播集都提交完了 —— 这才是真的追平。
		state := store.SubWaiting
		if show.Ended {
			state = store.SubDone
		}
		s.mark(ctx, sub, time.Now().Add(interval), state, msg, 0)
	}
	return msg, nil
}

// search 按订阅搜资源。中文名给 TG 频道，英文原名给种子站。
//
// 刻意**不**把季集号拼进查询串：帖子标题写法千奇百怪（S02E05 / S02.E05 /
// 第5集），把集号塞进搜索词会让很多本来能命中的帖子搜不到。反正结果回来
// 之后 Pick 会按季集精确过滤，搜宽一点没有副作用。
func (s *Service) search(ctx context.Context, sub store.Subscription) (*resource.Result, error) {
	q := strings.TrimSpace(sub.Title)
	if q == "" {
		q = strings.TrimSpace(sub.OriginalTitle)
	}
	en := strings.TrimSpace(sub.OriginalTitle)
	res, err := s.dep.Search(ctx, q, "", en, 30)
	if res == nil {
		return nil, err
	}
	// 部分源失败不算失败：能挑出一条就够了。
	return res, nil
}

// target 决定这条订阅提交到哪个账号、哪个目录。
//
// 优先用订阅指定的媒体库（库里有 account_id 和 remote_cid，交上去之后
// 自动整理 / 同步 / 刮削那条链就能对上）；没指定就让 submitOffline 自己挑
// 第一个支持离线的账号。
// target 决定「下到哪个账号的哪个目录」。
//
// 没指定媒体库时必须兜到全局 res_save_cid：自动整理盯的就是这个目录，
// 订阅下到 115 根目录等于把闭环断在最后一环 —— 东西下来了但没人整理，
// 用户只能自己去网盘里翻。网页「搜到就存」和 TG 机器人也是这么兜底的。
func (s *Service) target(ctx context.Context, sub store.Subscription) (int64, string) {
	cid := strings.TrimSpace(sub.SaveCID)
	if sub.LibraryID > 0 {
		lib, err := s.st.GetLibrary(ctx, sub.LibraryID)
		if err == nil {
			if cid == "" {
				cid = strings.TrimSpace(lib.RemoteCID)
			}
			return lib.AccountID, cid
		}
		// 库被删了就退回全局设置，别把下载丢到根目录去。
	}
	if cid == "" {
		if v, ok := s.st.GetSetting(ctx, "res_save_cid"); ok {
			cid = strings.TrimSpace(v)
		}
	}
	if cid == "" {
		cid = "0"
	}
	return 0, cid
}

func (s *Service) interval(ctx context.Context) time.Duration {
	if s.dep.Interval != nil {
		if d := s.dep.Interval(ctx); d > 0 {
			return d
		}
	}
	return 6 * time.Hour
}

func (s *Service) mark(ctx context.Context, sub store.Subscription,
	next time.Time, state, result string, failCount int) {

	if err := s.st.MarkSubscriptionChecked(ctx, sub.ID, next, state, result, failCount); err != nil {
		s.log.Warn("写回订阅状态失败", "id", sub.ID, "err", err)
	}
}

func (s *Service) acquire(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *Service) release(id int64) {
	s.mu.Lock()
	delete(s.busy, id)
	s.mu.Unlock()
}

// Busy 报告某条订阅是不是正在检查，供接口回 409。
func (s *Service) Busy(id int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[id]
}

// ==================== 纯函数区（都有单测） ====================

// pendingEpisodes 列出「已播出、起播线之后」的集，最多 max 集。
//
// 水位线与限制线早就合并成一条 watermark_*：这一集及之前不找资源。
// 程序提交离线成功后自己往前推，用户也能用滑杆往回拨（会重新补那些集）。
//
// 跨季靠 show.Seasons 展开：水位线在 S01E10、最新是 S02E03 时，要接着排
// S02E01-E03；没有 seasons 明细（TMDB 偶尔不填）就退化成只看最新那一季，
// 宁可少补几集，也不要凭猜去补中间的季。
func pendingEpisodes(wmSeason, wmEpisode int, show *tmdb.TVShow, max int) []tmdb.TVEpisode {
	if show == nil || max <= 0 {
		return nil
	}
	latest := show.Latest()
	if latest.Episode <= 0 {
		return nil
	}

	seasons := make([]tmdb.TVSeason, len(show.Seasons))
	copy(seasons, show.Seasons)
	sort.Slice(seasons, func(i, j int) bool { return seasons[i].Season < seasons[j].Season })

	// 没有季明细：只能处理最新这一季里水位线之后的集。
	if len(seasons) == 0 {
		seasons = []tmdb.TVSeason{{Season: latest.Season, EpisodeCount: latest.Episode}}
	}

	var out []tmdb.TVEpisode
	for _, s := range seasons {
		if s.EpisodeCount <= 0 {
			continue
		}
		for e := 1; e <= s.EpisodeCount; e++ {
			if atOrBefore(s.Season, e, wmSeason, wmEpisode) {
				continue
			}
			if !show.HasAired(s.Season, e) {
				continue
			}
			out = append(out, tmdb.TVEpisode{Season: s.Season, Episode: e})
			if len(out) >= max {
				return out
			}
		}
	}
	return out
}

// atOrBefore 判断 (s,e) 是否不晚于水位线 (wms,wme)。
//
// 水位线 0/0 表示「什么都没提交过」，此时任何 (s>=1,e>=1) 都不该被跳过 ——
// 直接写 `s < wms || (s == wms && e <= wme)` 会让首轮变成「全跳过」，这是
// 这类比较最容易踩的坑。
func atOrBefore(s, e, wms, wme int) bool {
	if s != wms {
		return s < wms
	}
	return e <= wme
}

// backoffDuration 是失败后的等待：1h → 2h → 4h → 8h → 12h 封顶。
//
// 不用指数无限涨：订阅出错多半是网络/风控，长时间不重试等于静默停摆，
// 用户只会看到「这条订阅半个月没动过」。
func backoffDuration(failCount int) time.Duration {
	d := time.Hour
	for i := 1; i < failCount; i++ {
		if d >= 12*time.Hour {
			return 12 * time.Hour
		}
		d *= 2
	}
	if d > 12*time.Hour {
		return 12 * time.Hour
	}
	return d
}

// nextCheckAfterAir 在「下一集有播出日期」时决定什么时候再来查。
//
// 播出当天基本拿不到资源：压制、上传、出种都要时间。所以按播出日次日算，
// 同时不早于正常周期 —— 用户把周期设成 24 小时时，不该因为明天有集就今晚查。
func nextCheckAfterAir(airDate string, interval time.Duration, now time.Time) time.Time {
	base := now.Add(interval)
	d, err := time.Parse("2006-01-02", strings.TrimSpace(airDate))
	if err != nil {
		return base
	}
	after := d.Add(24 * time.Hour)
	if after.After(base) {
		return after
	}
	return base
}

// episodeSpan 把一串集写成 S01E05 / S01E05-E07 / S01E05,S01E09 这类。
func episodeSpan(eps []tmdb.TVEpisode) string {
	if len(eps) == 0 {
		return ""
	}
	first := eps[0]
	if len(eps) == 1 {
		return fmt.Sprintf("S%02dE%02d", first.Season, first.Episode)
	}
	last := eps[len(eps)-1]
	// 季不同就不写区间：S01E10-S02E03 读起来像跨了 3 集，其实是两季。
	if first.Season != last.Season {
		parts := make([]string, 0, len(eps))
		for _, e := range eps {
			parts = append(parts, fmt.Sprintf("S%02dE%02d", e.Season, e.Episode))
		}
		return strings.Join(parts, ",")
	}
	return fmt.Sprintf("S%02dE%02d-E%02d", first.Season, first.Episode, last.Episode)
}

// shortTitle 把过长的资源名截断，只用于 last_result 这一行文案。
func shortTitle(s string) string {
	s = strings.TrimSpace(s)
	r := []rune(s)
	if len(r) <= 40 {
		return s
	}
	return string(r[:40]) + "…"
}
