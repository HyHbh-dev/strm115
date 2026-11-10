package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"strm115/internal/notify"
	"strm115/internal/organize"
	"strm115/internal/pan"
	"strm115/internal/scrape"
	"strm115/internal/store"
)

// 自动整理的配置存在 settings 里，和 scrape_config / subtitle_config 一个套路：
// 一行 JSON，读写都走同一个结构体。
const settingOrganizeConfig = "organize_config"

func (s *Server) loadOrganizeConfig(ctx context.Context) organize.Config {
	raw, ok := s.st.GetSetting(ctx, settingOrganizeConfig)
	if !ok || strings.TrimSpace(raw) == "" {
		return organize.Config{Batch: 30, MinSizeMB: 300, SyncAfter: true}.Filled()
	}
	var cfg organize.Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		s.log.Warn("整理配置解析失败，按默认值处理", "err", err)
		return organize.Config{Batch: 30, MinSizeMB: 300, SyncAfter: true}.Filled()
	}
	return cfg.Filled()
}

// handleOrganizeConfig 返回配置 + 可选的账号与媒体库清单。
//
// 一次把「选源目录要的账号」「选目标库要的库清单」「台账统计」都带上：
// 整理页首屏只有这一次请求，省得三个接口互相等。
func (s *Server) handleOrganizeGetConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.loadOrganizeConfig(ctx)

	accts, err := s.st.ListAccounts(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	libs, err := s.st.ListLibraries(ctx)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats, err := s.st.OrganizeStats(ctx)
	if err != nil {
		s.log.Warn("读取整理统计失败", "err", err)
		stats = map[string]int{}
	}

	accounts := make([]map[string]any, 0, len(accts))
	for _, a := range accts {
		accounts = append(accounts, map[string]any{"id": a.ID, "name": a.Name, "kind": a.Kind})
	}
	type libBrief struct {
		ID             int64  `json:"id"`
		Name           string `json:"name"`
		CollectionType string `json:"collection_type"`
		RemoteCID      string `json:"remote_cid"`
	}
	libOut := make([]libBrief, 0, len(libs))
	for _, l := range libs {
		libOut = append(libOut, libBrief{l.ID, l.Name, l.CollectionType, l.RemoteCID})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"config":    cfg,
		"accounts":  accounts,
		"libraries": libOut,
		"stats":     stats,
	})
}

// handleOrganizePutConfig 保存配置。
func (s *Server) handleOrganizePutConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Config organize.Config `json:"config"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	cfg := body.Config.Filled()
	if err := s.validateOrganizeConfig(r.Context(), cfg); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	if err := s.st.SetSetting(r.Context(), settingOrganizeConfig, string(b)); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": cfg})
}

// validateOrganizeConfig 保存前把所有规则过一遍。
//
// 以前这段是「单条配置」的校验，现在按规则循环：一条不合法就整份拒绝，
// 免得用户以为保存成功、跑起来才发现某条永远失败。
func (s *Server) validateOrganizeConfig(ctx context.Context, cfg organize.Config) error {
	for _, rule := range cfg.Rules {
		if !rule.On() {
			continue
		}
		if strings.TrimSpace(rule.SourceCID) == "" && rule.MovieLibID == 0 && rule.TVLibID == 0 {
			continue // 前端加了空行还没填，不算错
		}
		if strings.TrimSpace(rule.SourceCID) == "" {
			return fmt.Errorf("规则「%s」还没选「待整理」目录", rule.Label())
		}
		if rule.MovieLibID == 0 && rule.TVLibID == 0 {
			return fmt.Errorf("规则「%s」还没选目标媒体库（电影 / 剧集至少选一个）", rule.Label())
		}
		// 电影库和剧集库选了同一个：剧集会连同电影一起堆在同一个目录里，
		// 之后既没法按库刮削、也没法在 Emby 里分开显示。
		if rule.MovieLibID != 0 && rule.MovieLibID == rule.TVLibID {
			if lib, err := s.st.GetLibrary(ctx, rule.MovieLibID); err == nil && lib.CollectionType != "mixed" {
				return fmt.Errorf("规则「%s」的电影库和剧集库选的是同一个「%s」：剧集会被搬进电影目录，请给剧集单独建一个库",
					rule.Label(), lib.Name)
			}
		}
		for _, libID := range []int64{rule.MovieLibID, rule.TVLibID} {
			if libID == 0 {
				continue
			}
			lib, err := s.st.GetLibrary(ctx, libID)
			if err != nil {
				return fmt.Errorf("规则「%s」的目标库不存在（可能已被删除）", rule.Label())
			}
			// 源目录的账号必须是目标库的来源之一：移动只能在同一个网盘账号内发生。
			if rule.AccountID != 0 && !libHasAccount(lib, rule.AccountID) {
				return fmt.Errorf("规则「%s」的源目录属于另一个网盘账号，而目标库「%s」没挂这个账号，跨账号无法移动",
					rule.Label(), lib.Name)
			}
		}
		// 夸克靠网页 Cookie 移动文件，TV 凭证里没有 Cookie 时会一路失败到台账里。
		// 提前说清楚，比等用户点了执行再逐条报错好。
		if rule.AccountID != 0 {
			kind, _ := s.st.AccountKind(ctx, rule.AccountID)
			if kind == "quarktv" && !s.accountHasWebCookie(ctx, rule.AccountID) {
				return errors.New("这条夸克规则缺少网页 Cookie：请在「账号与媒体库」里改用 Cookie 方式重新添加夸克账号")
			}
		}
	}
	return nil
}

// accountHasWebCookie 判断夸克账号有没有网页 Cookie（移动/改名/删除要用）。
func (s *Server) accountHasWebCookie(ctx context.Context, accountID int64) bool {
	acct, err := s.st.GetAccount(ctx, accountID)
	if err != nil {
		return false
	}
	var cred struct {
		WebCookie string `json:"web_cookie"`
	}
	if err := json.Unmarshal([]byte(acct.Credential), &cred); err != nil {
		return false
	}
	return strings.TrimSpace(cred.WebCookie) != ""
}

// libHasAccount 判断这个库有没有挂某个网盘账号（主账号或任一来源）。
func libHasAccount(lib store.Library, accountID int64) bool {
	if accountID == 0 || lib.AccountID == accountID {
		return true
	}
	for _, src := range lib.Sources {
		if src.AccountID == accountID {
			return true
		}
	}
	return false
}

// organizeTargetLib 是「这条该进哪个库」，同时给出该账号在库里的根目录。
//
// 一个库挂两个网盘时，库上记的 RemoteCID 只是「主来源」的目录：
// 夸克那条规则要是拿它当目标根，就会把文件往 115 的 cid 上搬。
func (s *Server) organizeTargetLib(ctx context.Context, rule organize.Rule, kind string) (store.Library, string, error) {
	id := rule.MovieLibID
	label := "电影"
	if kind == organize.KindSeries {
		id = rule.TVLibID
		label = "剧集"
	}
	if id == 0 {
		return store.Library{}, "", fmt.Errorf("这条规则没配%s库，先跳过", label)
	}
	lib, err := s.st.GetLibrary(ctx, id)
	if err != nil {
		return store.Library{}, "", fmt.Errorf("%s库不存在（可能已被删除）", label)
	}
	// 规则自己指定了目录就用它：混合库（根 /影视，下面再分 电影 / 剧集）
	// 没有这一层的话，电影会被搬到库根，正好落在 Emby 两个库的中间。
	if cid := strings.TrimSpace(rule.TargetCID); cid != "" {
		return lib, cid, nil
	}
	root := lib.RemoteCID
	if rule.AccountID != 0 && lib.AccountID != rule.AccountID {
		for _, src := range lib.Sources {
			if src.AccountID == rule.AccountID {
				root = src.RemoteCID
				break
			}
		}
	}
	if strings.TrimSpace(root) == "" {
		return store.Library{}, "", fmt.Errorf("目标库「%s」没有这个账号的目录，无法落位", lib.Name)
	}
	return lib, root, nil
}

// organizeScanRule 是一条规则的扫描结果。
type organizeScanRule struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	AccountID   int64           `json:"account_id"`
	AccountName string          `json:"account_name"`
	AccountKind string          `json:"account_kind"`
	SourceCID   string          `json:"source_cid"`
	SourceName  string          `json:"source_name"`
	MovieLibID  int64           `json:"movie_lib_id"`
	TVLibID     int64           `json:"tv_lib_id"`
	TargetCID   string          `json:"target_cid,omitempty"`
	TargetName  string          `json:"target_name,omitempty"`
	Items       []organize.Item `json:"items"`
	Stats       map[string]int  `json:"stats"`
	Error       string          `json:"error,omitempty"`
}

// handleOrganizeScan 扫一遍所有规则的源目录，返回「会怎么整理」，不动任何文件。
//
// 扫描和执行分成两个接口是刻意的：整理会改网盘里的实际位置，
// 没有预览直接动手，用户第一次用就是在赌。
func (s *Server) handleOrganizeScan(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()

	cfg := s.loadOrganizeConfig(ctx)
	if err := cfg.Ready(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rules := cfg.ActiveRules()
	// 只扫一条时（前端选中某条规则）不必把其它源目录也跑一遍 —— 夸克列目录
	// 也是要发请求的，一页一条，扫三个空目录纯属浪费。
	if want := strings.TrimSpace(r.URL.Query().Get("rule_id")); want != "" {
		rule, ok := cfg.RuleByID(want)
		if !ok || !rule.Valid() {
			writeErr(w, http.StatusBadRequest, errors.New("这条规则不存在或不完整"))
			return
		}
		rules = []organize.Rule{rule}
	}

	acctName := map[int64]string{}
	acctKind := map[int64]string{}
	if accts, err := s.st.ListAccounts(ctx); err == nil {
		for _, a := range accts {
			acctName[a.ID] = a.Name
			acctKind[a.ID] = a.Kind
		}
	}
	libName := map[int64]string{}
	if libs, err := s.st.ListLibraries(ctx); err == nil {
		for _, l := range libs {
			libName[l.ID] = l.Name
		}
	}

	out := make([]organizeScanRule, 0, len(rules))
	for _, rule := range rules {
		sr := organizeScanRule{
			ID: rule.ID, Name: rule.Label(), AccountID: rule.AccountID,
			AccountName: acctName[rule.AccountID], AccountKind: acctKind[rule.AccountID],
			SourceCID: rule.SourceCID, SourceName: rule.SourceName,
			MovieLibID: rule.MovieLibID, TVLibID: rule.TVLibID,
			TargetCID: rule.TargetCID, TargetName: rule.TargetName,
			Stats: map[string]int{"total": 0, "movie": 0, "series": 0, "skip": 0, "done": 0},
		}
		accountID := rule.AccountID
		if accountID == 0 {
			accountID = s.st.FirstAccountID(ctx)
			sr.AccountID = accountID
			sr.AccountName = acctName[accountID]
			sr.AccountKind = acctKind[accountID]
		}
		if accountID == 0 {
			sr.Error = "还没有登录网盘账号"
			out = append(out, sr)
			continue
		}
		mgr, err := s.pool.Get(ctx, accountID)
		if err != nil {
			sr.Error = err.Error()
			out = append(out, sr)
			continue
		}
		entries, err := mgr.ListAll(ctx, rule.SourceCID, 200)
		if err != nil {
			sr.Error = err.Error()
			out = append(out, sr)
			continue
		}

		// 已整理 / 已忽略的条目不再出现在扫描结果里。
		done, err := s.st.OrganizeStatuses(ctx, accountID)
		if err != nil {
			s.log.Warn("读取整理台账失败，本轮不做过过滤", "err", err)
			done = map[string]string{}
		}

		actionable := 0
		items := make([]organize.Item, 0, len(entries))
		for _, e := range entries {
			if st, ok := done[e.ID]; ok && store.OrganizeDone(st) {
				sr.Stats["done"]++
				continue
			}
			if actionable >= cfg.Batch {
				break
			}
			sr.Stats["total"]++
			it := organize.Recognize(organize.Entry{
				ID: e.ID, Name: e.Name, IsDir: e.IsDir, Size: e.Size,
			}, cfg.MinSizeMB)
			switch {
			case it.Skip != "":
				sr.Stats["skip"]++
			case it.Kind == organize.KindSeries:
				sr.Stats["series"]++
				actionable++
			default:
				sr.Stats["movie"]++
				actionable++
			}
			// 跳过的条目不查目标库：它们根本不会被移动，标一个库名只会让人
			// 以为「这条也会进那个库」。
			if it.Skip == "" {
				if lib, _, err := s.organizeTargetLib(ctx, rule, it.NeedLib()); err == nil {
					it.Lib = libName[lib.ID]
					if it.Lib == "" {
						it.Lib = lib.Name
					}
				} else {
					it.Skip = err.Error()
				}
			}
			items = append(items, it)
		}
		sr.Items = items
		out = append(out, sr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"rules": out,
		"batch": cfg.Batch,
	})
}

// ==================== 执行 ====================

// dirOps 是整理真正用到的那几个网盘动作。
//
// 抽成接口而不是直接吃 *pan.Manager：建目录 / 移动是整理里最危险的一步，
// 「已存在的目录不会再建一遍」这种逻辑必须能单测，不该只能靠真账号验证。
type dirOps interface {
	ListAll(ctx context.Context, dirID string, pageSize int) ([]pan.Entry, error)
	Mkdir(ctx context.Context, parentID, name string) (string, error)
	Move(ctx context.Context, toDirID string, ids ...string) (int, error)
}

// dirCache 缓存「父目录 → 子目录名 → cid」，避免同一个目标目录反复列。
//
// 一轮整理 30 条，可能有 25 条属于不同的片名目录；不缓存的话每条都要
// 列一次目标库根目录，白白多几十次请求，115 的风控也就来了。
type dirCache struct {
	mgr  dirOps
	kids map[string]map[string]string
}

func newDirCache(mgr dirOps) *dirCache {
	return &dirCache{mgr: mgr, kids: map[string]map[string]string{}}
}

// ensure 逐级确保目录存在，返回最深一级的 cid。
func (d *dirCache) ensure(ctx context.Context, root string, segs []string) (string, error) {
	cid := root
	for _, seg := range segs {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		kids, ok := d.kids[cid]
		if !ok {
			entries, err := d.mgr.ListAll(ctx, cid, 200)
			if err != nil {
				return "", err
			}
			kids = make(map[string]string, len(entries))
			for _, e := range entries {
				if e.IsDir {
					kids[e.Name] = e.ID
				}
			}
			d.kids[cid] = kids
		}
		if id, ok := kids[seg]; ok {
			cid = id
			continue
		}
		id, err := d.mgr.Mkdir(ctx, cid, seg)
		if err != nil {
			return "", err
		}
		kids[seg] = id
		cid = id
	}
	return cid, nil
}

// organizeRunResult 是执行结果，也给记录接口复用。
type organizeRunResult struct {
	Moved    int      `json:"moved"`
	Failed   int      `json:"failed"`
	Skipped  int      `json:"skipped"`
	Synced   []string `json:"synced"`
	Errors   []string `json:"errors"`
	Requests int      `json:"requests"`
	// Names 是本轮成功搬走的片名，只用于通知文案（不进 JSON 响应，
	// 前端台账里已经有每一条的明细）。
	Names []string `json:"-"`
}

// errOrganizeBusy 表示「已经有一轮整理在跑」。
//
// 不做排队：两轮同时移动同一个目录，很容易在 115 侧挪到一半；
// 跳过一轮、让用户（或下一次入库）再触发，比挪乱强。
var errOrganizeBusy = errors.New("上一轮整理还在跑，等它结束再试")

// tmdbRecognized 在搬动之前问一次 TMDB：这部片子刮得到吗？
//
// 为什么要多这一次请求：识别只看文件名，「像个片名」和「真能刮到」是两件事。
// 广告壳、合集目录、名字对不上的枪版都能通过文件名识别，搬进库之后就变成
// 一张没有海报的灰卡片，而想清理得先找到它、删掉 .strm、再清 Emby 条目。
// 让它留在暂存目录（/StrmStation）里，用户一眼能看见、处理也简单。
//
// 两种「不拦」的情况是刻意的：
//   - 没配 TMDB Key：拦了就变成「整理功能一次都跑不动」；
//   - 请求失败（网络 / 限流）：基础设施的锅不该让用户的文件留在原地，
//     刮削阶段还有一次机会。
func (s *Server) tmdbRecognized(ctx context.Context, it organize.Item) (bool, string) {
	c := s.tmdbClient(ctx)
	if c == nil {
		return true, ""
	}
	hits, err := c.SearchMeta(ctx, it.Title, it.Year, it.NeedLib())
	if err != nil {
		s.log.Warn("整理前核对 TMDB 失败，先按识别结果搬", "title", it.Title, "err", err)
		return true, ""
	}
	p := scrape.Parsed{
		Title: it.Title, Year: it.Year, Kind: it.NeedLib(),
		Season: it.Season, Episode: it.Episode, HasEpisode: it.Episode > 0,
	}
	if _, ok := scrape.Pick(p, hits); ok {
		return true, ""
	}
	// 中文库里对不上不等于没有这部片：国创 / 日番的发布名常常只有英文名
	// （`A Record of a Mortal’s Journey to Immortality`），而 TMDB 的 zh-CN
	// 名字是「凡人修仙传」，两边字面完全不同 —— 但用 en-US 搜同一串，返回的
	// 就是那个英文名。只在第一次没认出来时多花这一次请求。
	if en, err := c.SearchMetaLang(ctx, it.Title, it.Year, it.NeedLib(), "en-US"); err == nil {
		if _, ok := scrape.Pick(p, en); ok {
			s.log.Info("整理：zh-CN 名字对不上，按英文名对上了 TMDB", "title", it.Title)
			return true, ""
		}
	}
	return false, "TMDB 里没找到（片名 / 年份对不上），先留在暂存目录"
}

// runOrganize 是整理的实际执行：列源目录 → 逐条识别 → 移动 → 触发同步。
//
// 网页的「执行整理」和「入库后自动整理」共用这一份实现，区别只有入参：
// 规则（源目录 + 目标库 + 账号）和 fileIDs（网页可以只处理勾选的条目）。
func (s *Server) runOrganize(ctx context.Context, cfg organize.Config, rule organize.Rule, fileIDs []string) (organizeRunResult, error) {
	res := organizeRunResult{Synced: []string{}, Errors: []string{}}
	sourceCID := strings.TrimSpace(rule.SourceCID)
	if sourceCID == "" {
		return res, errors.New("这条规则还没选「待整理」目录")
	}
	accountID := rule.AccountID
	if accountID == 0 {
		accountID = s.st.FirstAccountID(ctx)
	}
	mgr, err := s.pool.Get(ctx, accountID)
	if err != nil {
		return res, err
	}
	entries, err := mgr.ListAll(ctx, sourceCID, 200)
	if err != nil {
		return res, err
	}
	want := map[string]bool{}
	for _, id := range fileIDs {
		if id = strings.TrimSpace(id); id != "" {
			want[id] = true
		}
	}

	done, _ := s.st.OrganizeStatuses(ctx, accountID)
	cache := newDirCache(mgr)
	libs := map[int64]store.Library{}
	touched := map[int64]bool{}

	handled := 0
	for _, e := range entries {
		if len(want) > 0 && !want[e.ID] {
			continue
		}
		if len(want) == 0 {
			if st, ok := done[e.ID]; ok && store.OrganizeDone(st) {
				continue
			}
		}
		if handled >= cfg.Batch {
			break
		}

		it := organize.Recognize(organize.Entry{
			ID: e.ID, Name: e.Name, IsDir: e.IsDir, Size: e.Size,
		}, cfg.MinSizeMB)
		rec := store.OrganizeRecord{
			AccountID: accountID, FileID: e.ID, Name: e.Name,
			IsDir: e.IsDir, Size: e.Size,
			Kind: it.Kind, Title: it.Title, Year: it.Year,
			Season: it.Season, Episode: it.Episode,
			Confidence: it.Confidence, Reason: it.Reason,
		}

		if it.Skip != "" {
			rec.Status = store.OrganizeSkipped
			rec.Message = it.Skip
			_ = s.st.UpsertOrganizeRecord(ctx, rec)
			res.Skipped++
			continue
		}
		// TMDB 认不出就留在暂存目录。这一步放在占 batch 名额之前：
		// 一个刮不到的条目不该挤掉同批里真能入库的片子。
		if cfg.VerifyTMDBOn() {
			if ok, note := s.tmdbRecognized(ctx, it); !ok {
				rec.Status = store.OrganizeSkipped
				rec.Message = note
				_ = s.st.UpsertOrganizeRecord(ctx, rec)
				res.Skipped++
				continue
			}
		}
		// 只有「真要搬」的才占 batch 名额。跳过是在本地算出来的（不花网盘的
		// 请求），却以前也算一格 —— 资源目录里攒了 30 个 .nfo/.srt 之后，
		// 同批里真正的电影就永远轮不到。
		handled++

		lib, root, err := s.organizeTargetLib(ctx, rule, it.NeedLib())
		if err != nil {
			rec.Status = store.OrganizeFailed
			rec.Message = err.Error()
			_ = s.st.UpsertOrganizeRecord(ctx, rec)
			res.Failed++
			res.Errors = append(res.Errors, e.Name+"："+err.Error())
			continue
		}
		libs[lib.ID] = lib
		rec.Target = path.Join(lib.Name, it.Target)
		rec.Status = store.OrganizeMoved

		segs := []string{}
		if it.Dir != "" {
			segs = strings.Split(it.Dir, "/")
		}
		// 目录条目：直接搬到库根，再把目录改成「片名 (年份)」。
		//
		// 以前是「先在根下建一个 片名 (年份)，再把整个目录搬进去」，于是
		// 片名本来就规范的目录会变成 `片名 (年份)/片名 (年份)/文件`；
		// 名字带发布组垃圾的目录则是 `片名 (年份)/原始长名/文件`。
		// Emby 虽然认得出，但媒体库里全是这种双层目录，很难整理。
		// 移动+改名是两步，但都是同一个网盘内的元数据操作，比多一层目录划算。
		var toCID string
		if e.IsDir {
			toCID, err = cache.ensure(ctx, root, nil)
		} else {
			toCID, err = cache.ensure(ctx, root, segs)
		}
		if err == nil {
			_, err = mgr.Move(ctx, toCID, e.ID)
		}
		if err == nil && e.IsDir && len(segs) == 1 && segs[0] != e.Name {
			// 改不动不算失败：文件已经在正确的位置，只是名字还是原来的。
			if _, rerr := mgr.Rename(ctx, e.ID, segs[0]); rerr != nil {
				s.log.Warn("整理后改名失败，保留原目录名", "dir", e.Name, "want", segs[0], "err", rerr)
			}
		}
		if err != nil {
			rec.Status = store.OrganizeFailed
			rec.Message = err.Error()
			res.Failed++
			res.Errors = append(res.Errors, e.Name+"："+err.Error())
		} else {
			rec.Message = "已移到 " + rec.Target
			res.Moved++
			name := it.Title
			if name == "" {
				name = e.Name
			}
			res.Names = append(res.Names, name)
			touched[lib.ID] = true
		}
		if err := s.st.UpsertOrganizeRecord(ctx, rec); err != nil {
			s.log.Warn("写整理台账失败", "file", e.Name, "err", err)
		}
	}
	res.Requests = int(mgr.Snapshot().Requests)

	// 整理完让媒体库立刻看到：触发一轮增量同步，等它跑完再发通知。
	// 这里不阻塞 HTTP 响应——同步可能要几分钟，而用户只关心「整理完了没」。
	if cfg.SyncAfter && len(touched) > 0 {
		for id := range touched {
			lib := libs[id]
			if job, err := s.beginSync(lib); err == nil {
				s.watchSyncAndNotify(job, lib)
				res.Synced = append(res.Synced, lib.Name)
			} else {
				res.Errors = append(res.Errors, "同步「"+lib.Name+"」没起来："+err.Error())
			}
		}
	}
	if res.Errors == nil {
		res.Errors = []string{}
	}
	return res, nil
}

// runOrganizeRules 按规则逐条执行，全程只持一次锁。
//
// 锁放在这里而不是 runOrganize 里：一轮里要跑多条规则，如果每条各锁一次，
// 两条之间就会插进别的整理请求，同一批文件可能被挪两次。
func (s *Server) runOrganizeRules(ctx context.Context, cfg organize.Config, rules []organize.Rule, fileIDs []string, notifyAlways bool) (organizeRunResult, error) {
	res := organizeRunResult{Synced: []string{}, Errors: []string{}}
	if !s.organizeMu.TryLock() {
		return res, errOrganizeBusy
	}
	defer s.organizeMu.Unlock()

	for _, rule := range rules {
		one, err := s.runOrganize(ctx, cfg, rule, fileIDs)
		res.Moved += one.Moved
		res.Skipped += one.Skipped
		res.Failed += one.Failed
		res.Requests += one.Requests
		res.Synced = append(res.Synced, one.Synced...)
		res.Errors = append(res.Errors, one.Errors...)
		res.Names = append(res.Names, one.Names...)
		if err != nil {
			res.Failed++
			res.Errors = append(res.Errors, "规则「"+rule.Label()+"」："+err.Error())
		}
	}

	// 整理完推一条。配了 SyncAfter 时后面还会来一条「同步完成」——
	// 那是同一件事的下一个阶段，不合并（合并就得等同步跑完，几十秒起步）。
	//
	// notifyAlways 是「用户在网页上亲手点的那一轮」：哪怕一条都没搬也要回一声。
	// 否则点了「开始整理」、页面显示成功、手机一声不响，用户没法判断到底是
	// 跑过了没东西可搬，还是根本没跑起来 —— 这个疑问只能靠翻日志才能自证。
	if title, line, ok := organizeNotice(res, notifyAlways); ok {
		s.pushEvent("organize_ok", notify.KindOrganize, title, line)
	}
	return res, nil
}

// organizeNotice 生成「整理完成」那条推送的标题与正文。
//
// 第三个返回值表示要不要发：自动整理只在真的搬了或失败时发（没东西可搬不打扰），
// 用户手动点的那一轮永远回一条 —— 否则「点了没反应」和「跑了但没东西可搬」
// 在手机上看起来一模一样，只能来翻日志。
func organizeNotice(res organizeRunResult, always bool) (title, line string, ok bool) {
	if res.Moved == 0 && res.Failed == 0 && !always {
		return "", "", false
	}
	title = "📂 整理完成"
	line = fmt.Sprintf("移入 %d · 跳过 %d · 失败 %d", res.Moved, res.Skipped, res.Failed)
	if res.Moved == 0 && res.Failed == 0 {
		title = "📂 整理完成（没有需要搬的条目）"
		line = fmt.Sprintf("跳过 %d · 失败 0", res.Skipped)
		if res.Skipped == 0 {
			line = "待整理目录里是空的，没有可搬的东西"
		}
	}
	if len(res.Names) > 0 {
		// 只报前三部：手机上一眼看得到的是片名，不是统计数字。
		shown := res.Names
		if len(shown) > 3 {
			shown = shown[:3]
		}
		line += "\n" + strings.Join(shown, "、")
		if len(res.Names) > len(shown) {
			line += fmt.Sprintf(" 等 %d 部", len(res.Names))
		}
	}
	if len(res.Synced) > 0 {
		line += " · 已同步 " + strings.Join(res.Synced, "、")
	}
	return title, line, true
}

// handleOrganizeRun 真正移动文件。
//
// 请求体可以带 file_ids 只处理勾选的条目；不带就按扫描顺序最多处理 batch 条。
// 无论如何都会重新列一次源目录：前端拿着的 file_id 可能是几分钟前的，
// 那期间用户可能自己动过网盘。
func (s *Server) handleOrganizeRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		FileIDs []string `json:"file_ids"`
		// RuleID 只跑某一条规则（扫描页按规则分组，勾选的都是同一条里的）。
		// 留空表示把所有启用的规则都跑一遍。
		RuleID string `json:"rule_id"`
	}
	_ = readJSON(r, &body)

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	cfg := s.loadOrganizeConfig(ctx)
	if !cfg.Enabled {
		writeErr(w, http.StatusBadRequest, errors.New("自动整理没启用（到「基础配置」里打开）"))
		return
	}
	if err := cfg.Ready(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	rules := cfg.ActiveRules()
	if id := strings.TrimSpace(body.RuleID); id != "" {
		rule, ok := cfg.RuleByID(id)
		if !ok || !rule.Valid() {
			writeErr(w, http.StatusBadRequest, errors.New("这条规则不存在或不完整"))
			return
		}
		rules = []organize.Rule{rule}
	}
	// 手动运行：无论结果如何都回一条通知。
	res, err := s.runOrganizeRules(ctx, cfg, rules, body.FileIDs, true)
	if err != nil {
		code := statusOf(err)
		if errors.Is(err, errOrganizeBusy) {
			code = http.StatusConflict
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       res.Failed == 0,
		"moved":    res.Moved,
		"failed":   res.Failed,
		"skipped":  res.Skipped,
		"synced":   res.Synced,
		"errors":   res.Errors,
		"requests": res.Requests,
	})
}

// ==================== 入库后自动整理 ====================

// organizerIngestOn 判断「入库后自动整理」现在能不能跑：
// 开关打开、配置够用、至少有一条规则能跑。
//
// 早先这里还要求必须有「资源保存目录」，因为自动整理曾经只会扫那一个 115 目录。
// 现在源目录跟着规则走（115 和夸克各有自己的转存目录），再拿 115 的 cid 当
// 门槛，会让只配了夸克规则的部署永远进不来。
func (s *Server) organizerIngestOn(ctx context.Context) bool {
	cfg := s.loadOrganizeConfig(ctx)
	if !cfg.Enabled || !cfg.AutoIngestOn() {
		return false
	}
	if err := cfg.Ready(); err != nil {
		return false
	}
	for _, r := range cfg.ActiveRules() {
		if r.Valid() {
			return true
		}
	}
	return false
}

// autoOrganizeAfterIngest 在「有东西刚入库」之后跑一轮整理。
//
// 源目录用规则自己的「待整理」目录：TG 转存、机器人离线、订阅下载分别落在
// 115 和夸克各自的转存目录里，硬套同一个 cid 只会让另一个网盘永远扫不到东西。
// 规则没填源目录（老配置）时才回落到资源保存目录。
// 整理完 runOrganize 自己会触发同步，同步跑完再按设置自动刮削、自动补字幕 ——
// 聊天窗口里最终只会看到「整理完成 → 同步完成 → 刮削完成 → 🆕 入库」。
//
// 失败只记日志、不改 TG 那条回执：回执早就发出去了，回头再改反而更乱；
// 网页面板的整理台账里每条都有失败原因。
func (s *Server) autoOrganizeAfterIngest(ctx context.Context, acctID int64, tag string) {
	cfg := s.loadOrganizeConfig(ctx)
	if !cfg.Enabled || !cfg.AutoIngestOn() {
		s.log.Debug("入库后自动整理没开，跳过", "tag", tag)
		return
	}
	if err := cfg.Ready(); err != nil {
		s.log.Debug("入库后自动整理配置不全，跳过", "tag", tag, "err", err)
		return
	}
	fallbackCID := strings.TrimSpace(s.setting(ctx, "res_save_cid"))
	fallbackName := s.setting(ctx, "res_save_path")

	var picked []organize.Rule
	for _, r := range cfg.ActiveRules() {
		acct := r.AccountID
		if acct == 0 {
			acct = s.st.FirstAccountID(ctx)
		}
		if acct == 0 {
			continue
		}
		// 有 acctID 时按账号过滤：转存到哪个账号，就只整理那个账号的规矩。
		if acctID != 0 && acct != acctID {
			continue
		}
		if strings.TrimSpace(r.SourceCID) == "" {
			if fallbackCID == "" || fallbackCID == "0" {
				continue
			}
			r.SourceCID = fallbackCID
			r.SourceName = fallbackName
		}
		r.AccountID = acct
		picked = append(picked, r)
	}
	if len(picked) == 0 {
		s.log.Debug("没有可用的整理规则，入库后不整理", "tag", tag)
		return
	}
	// 自动整理：只有真的搬了或失败了才推，没东西可搬不打扰。
	res, err := s.runOrganizeRules(ctx, cfg, picked, nil, false)
	if err != nil {
		if errors.Is(err, errOrganizeBusy) {
			// 网页正跑着一轮，这次跳过就行：定时扫描每 5 分钟还会再来。
			s.log.Debug("上一轮整理还没结束，跳过这一轮", "tag", tag)
			return
		}
		s.log.Warn("入库后自动整理失败", "tag", tag, "err", err)
		return
	}
	if res.Moved == 0 && res.Failed == 0 {
		s.log.Debug("入库后自动整理：没有需要搬的东西", "tag", tag, "skipped", res.Skipped)
		return
	}
	s.log.Info("入库后自动整理完成",
		"tag", tag, "moved", res.Moved, "skipped", res.Skipped, "failed", res.Failed)
}

// ingestWatchInterval 是「待整理目录」的巡检间隔。
//
// 5 分钟：115 的列表接口按次算，再快没有意义；再慢用户会觉得「东西丢了」。
// 每条启用规则多一次列目录请求（115 与夸克各一个转存目录，也就是两次），
// 没有新东西时什么都不做。
const ingestWatchInterval = 5 * time.Minute

// ingestWatchFirstDelay 是启动后第一次巡检的等待时间。
//
// 短一点：容器重启前积压的东西不该再等 5 分钟。也不能是 0 —— 启动瞬间
// 还在恢复账号 / 网盘连接，抢着调 115 的接口只会报一堆无关的错。
const ingestWatchFirstDelay = 90 * time.Second

// watchIngestFolder 定期看一眼各条规则配的「待整理」目录，把不经过程序转存
// 的东西也收进来（115 和夸克各扫各的，不再只盯 115 的资源保存目录）。
//
// 为什么需要它：入库后自动整理只挂在「程序自己完成了一次转存/下载」的事件上
// （TG 机器人、网页转存、离线完成、订阅）。用户在 115 App、夸克 App 或网页里
// 手动转存到那个目录时，没有任何事件能通知到程序 —— 文件就一直躺着，看起来像
// 「自动整理坏了」。巡检补的就是这个缺口。
//
// 开关跟「入库后自动整理」共用：它表达的是「落进资源目录的东西就该被收走」，
// 至于东西是怎么进去的，用户并不关心。
func (s *Server) watchIngestFolder(ctx context.Context) {
	tick := time.NewTicker(ingestWatchInterval)
	defer tick.Stop()
	// 只触发一次的「第一轮」计时器：之后一直 select 它也不会再响。
	first := time.NewTimer(ingestWatchFirstDelay)
	defer first.Stop()
	fresh := true

	for {
		select {
		case <-ctx.Done():
			return
		case <-first.C:
		case <-tick.C:
		}
		if !s.organizerIngestOn(ctx) {
			fresh = false
			continue
		}
		if s.offline.Watching() > 0 {
			// 有离线任务在下载时，115 侧正在写文件；等它们下完再来收拾。
			continue
		}
		runCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		s.autoOrganizeAfterIngest(runCtx, 0, "定时巡检")
		cancel()
		if fresh {
			// 第一轮无论有没有东西都记一条：巡检这条链到底有没有跑起来，
			// 看日志就能确认，不用等下次真入库。
			s.log.Info("待整理目录：巡检已开始工作", "interval", ingestWatchInterval)
			fresh = false
		}
	}
}

// kickOrganizeIngest 在后台跑一轮「入库后整理」。
//
// delay 是留给 115 的落盘时间：刚转存完立刻列目录，偶尔还看不到那个文件；
// 离线任务是「刚下完」，网盘侧同样可能晚一两秒才出现在列表里。
func (s *Server) kickOrganizeIngest(acctID int64, delay time.Duration, tag string) {
	go func() {
		if delay > 0 {
			time.Sleep(delay)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		s.autoOrganizeAfterIngest(ctx, acctID, tag)
	}()
}

// ==================== 台账 ====================

// handleOrganizeRecords 列台账。status 为空表示全部。
func (s *Server) handleOrganizeRecords(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, total, err := s.st.ListOrganizeRecords(r.Context(), strings.TrimSpace(q.Get("status")), limit, offset)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	stats, _ := s.st.OrganizeStats(r.Context())
	if stats == nil {
		stats = map[string]int{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": rows, "total": total, "stats": stats})
}

// handleOrganizeIgnore 把一条标记为忽略，之后扫描不再出现。
func (s *Server) handleOrganizeIgnore(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	rec, err := s.st.OrganizeRecord(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("记录不存在"))
		return
	}
	if err := s.st.SetOrganizeStatus(r.Context(), id, store.OrganizeIgnored, "已忽略", rec.Target); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": store.OrganizeIgnored})
}

// handleOrganizeRetry 重试一条失败 / 跳过的记录。
//
// 直接复用 run 的整条链路：重新识别、重新建目录、重新移动。
// 拿旧的目标路径再 Move 一次是错的——期间用户可能改过配置（换了目标库）。
func (s *Server) handleOrganizeRetry(w http.ResponseWriter, r *http.Request) {
	id := pathID(r, "id")
	rec, err := s.st.OrganizeRecord(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, errors.New("记录不存在"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	cfg := s.loadOrganizeConfig(ctx)
	if err := cfg.Ready(); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	mgr, err := s.pool.Get(ctx, rec.AccountID)
	if err != nil {
		writeErr(w, statusOf(err), err)
		return
	}

	it := organize.Recognize(organize.Entry{
		ID: rec.FileID, Name: rec.Name, IsDir: rec.IsDir, Size: rec.Size,
	}, cfg.MinSizeMB)
	if it.Skip != "" {
		_ = s.st.SetOrganizeStatus(ctx, id, store.OrganizeSkipped, it.Skip, rec.Target)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": it.Skip})
		return
	}
	// 重试的是某一条台账，得用「它当初属于哪条规则」的目标库：
	// 一个库挂两个网盘时，拿错规则就会把夸克的文件往 115 的 cid 上搬。
	rule := organize.Rule{}
	found := false
	for _, r := range cfg.ActiveRules() {
		if r.AccountID == rec.AccountID || r.AccountID == 0 {
			rule = r
			found = true
			break
		}
	}
	if !found {
		if rules := cfg.ActiveRules(); len(rules) > 0 {
			rule = rules[0]
		}
	}
	if rule.SourceCID == "" {
		_ = s.st.SetOrganizeStatus(ctx, id, store.OrganizeFailed, "没有可用的整理规则", rec.Target)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": "没有可用的整理规则"})
		return
	}
	rule.AccountID = rec.AccountID
	lib, root, err := s.organizeTargetLib(ctx, rule, it.NeedLib())
	if err != nil {
		_ = s.st.SetOrganizeStatus(ctx, id, store.OrganizeFailed, err.Error(), rec.Target)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}

	cache := newDirCache(mgr)
	segs := []string{}
	if it.Dir != "" {
		segs = strings.Split(it.Dir, "/")
	}
	toCID, err := cache.ensure(ctx, root, segs)
	if err == nil {
		_, err = mgr.Move(ctx, toCID, rec.FileID)
	}
	target := path.Join(lib.Name, it.Target)
	if err != nil {
		_ = s.st.SetOrganizeStatus(ctx, id, store.OrganizeFailed, err.Error(), target)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	_ = s.st.SetOrganizeStatus(ctx, id, store.OrganizeMoved, "已移到 "+target, target)
	if cfg.SyncAfter {
		if job, err := s.beginSync(lib); err == nil {
			s.watchSyncAndNotify(job, lib)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "已移到 " + target})
}

// handleOrganizeClear 清台账（不会删网盘文件）。
func (s *Server) handleOrganizeClear(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Status string `json:"status"`
	}
	_ = readJSON(r, &body)
	n, err := s.st.ClearOrganizeRecords(r.Context(), strings.TrimSpace(body.Status))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed": n})
}

// handleOrganizePreview 用真实识别器试算一个名字。
//
// 页面上「识别规则」那一栏如果不给试算入口，用户永远不知道
// `Some.Show.S02E05.2160p.WEB-DL.mkv` 会被认成什么、进哪个目录。
func (s *Server) handleOrganizePreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		IsDir bool   `json:"is_dir"`
		Size  int64  `json:"size"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeErr(w, http.StatusBadRequest, errors.New("给个文件名或目录名"))
		return
	}
	cfg := s.loadOrganizeConfig(r.Context())
	it := organize.Recognize(organize.Entry{
		ID: "preview", Name: name, IsDir: body.IsDir, Size: body.Size,
	}, cfg.MinSizeMB)
	writeJSON(w, http.StatusOK, map[string]any{"item": it})
}
