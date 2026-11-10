package httpapi

// Telegram 机器人（接收端）的接线。
//
// 为什么接在这里而不是 internal/tgbot 里：机器人要调的东西全在这个包里
// —— 转存、离线、搜索、账号、设置。把它们抽成接口再实现一遍只会多一层
// 转发代码；反过来让 tgbot 只依赖一组函数字段，它能独立单测，这里也只
// 需要写几个闭包。

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"strm115/internal/resource"
	"strm115/internal/tgbot"
	"strm115/internal/tmdb"
)

const (
	// settingTGBotChats 是机器人白名单：逗号分隔的 chat id。
	// 空表示还没绑定，第一个说话的会话会被记进来。
	settingTGBotChats = "tg_bot_chat_ids"
	// settingTGBotOffset 是 getUpdates 游标。存盘的意义：
	// 服务重启后不会把「上次已经转存过的分享」当新消息再处理一遍。
	settingTGBotOffset = "tg_bot_offset"
	// settingTGBotStatus 是最近一次状态（Token 错了 / 被 webhook 占了 / 正常运行）。
	settingTGBotStatus = "tg_bot_status"
)

// startTGBot 启动接收端。重复调用无副作用。
func (s *Server) startTGBot() {
	s.botMu.Lock()
	defer s.botMu.Unlock()
	if s.botCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.botCancel = cancel
	s.bot = tgbot.New(s.botDeps())
	go s.bot.Run(ctx)
}

// Close 停掉所有常驻后台 goroutine（进程退出时调）。
//
// 订阅轮询器是随 New 一起起的，如果这里不 cancel，Close 之后它还会
// 每分钟扫一次到期订阅；单测里每个 newServerWithStore 都会漏一个。
func (s *Server) Close() {
	s.botMu.Lock()
	cancel := s.botCancel
	s.botCancel = nil
	s.botMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if s.subCancel != nil {
		s.subCancel()
		s.subCancel = nil
	}
}

// botDeps 把 Server 的能力喂给 tgbot。所有函数都读实时设置：
// 用户在设置页改 Token / 白名单，不用重启容器。
func (s *Server) botDeps() tgbot.Deps {
	return tgbot.Deps{
		Token: func() string {
			cfg := s.notify.Config()
			return strings.TrimSpace(cfg.TG.Token)
		},
		Proxy: func() string {
			if v := strings.TrimSpace(s.notify.Config().ProxyURL); v != "" {
				return v
			}
			return s.netProxy(context.Background())
		},
		Allow:      s.botAllow,
		Bind:       s.botBind,
		Search:     s.botSearch,
		SearchTMDB: s.botSearchTMDB,
		TMDBName:   s.botTMDBName,
		// 订阅：TG 里的「⭐订阅」与网页共用 createSubscription，
		// 剧集一样会生成专有追更日历。
		Subscribe:        s.botSubscribe,
		SubscribeSeasons: s.botSubSeasons,
		// 夸克分享：走「网页 Cookie 转存进夸克网盘」，播放另由夸克TV 直链负责。
		TransferQuark: func(ctx context.Context, url, pwd string) (*tgbot.Ingest, error) {
			out, err := s.saveQuarkShareTo(ctx, 0, url, pwd, "")
			if err != nil {
				return nil, err
			}
			ing := &tgbot.Ingest{
				Title: out.Title, Files: out.Files,
				Dir: out.Dir, Account: out.Account,
			}
			if out.Synced {
				ing.Note = "已自动跑一轮同步，稍后就能在媒体库里看到。"
			}
			return ing, nil
		},
		Transfer: func(ctx context.Context, code, pwd string) (*tgbot.Ingest, error) {
			out, err := s.saveShareTo(ctx, 0, "", code, pwd, "")
			if err != nil {
				return nil, err
			}
			ing := &tgbot.Ingest{
				Title: out.Title, Files: out.Files,
				Dir: out.Dir, Account: out.Account,
			}
			if out.Already {
				ing.Note = "这条之前存过，没有重复转存。"
			}
			// 转存只是「东西进了网盘」：不整理它就一直堆在资源目录里，
			// 不会进媒体库、不会同步、不会刮削。这里把后半段自动接上。
			if !out.Already {
				if s.organizerIngestOn(ctx) {
					ing.Note = joinNote(ing.Note, "接着自动整理 → 同步 → 刮削，好了我再推给你。")
					s.kickOrganizeIngest(out.AccountID, 3*time.Second, "TG 转存")
				} else if ing.Note == "" {
					ing.Note = "自动整理没开，文件会留在资源目录。"
				}
			}
			return ing, nil
		},
		Offline: s.botOffline,
		Offset:  s.botOffset,
		SaveOffset: func(ctx context.Context, n int64) {
			_ = s.st.SetSetting(ctx, settingTGBotOffset, strconv.FormatInt(n, 10))
		},
		State: s.botState,
		Log:   s.log,
	}
}

// botSearch 走和网页同一个搜索实现（同样的频道、站点、代理与英文名替换）。
//
// en 是「TMDB 认片」锁定的英文原名（original_title）：把它交给 resourceSearch
// 的 EN 形参，种子站换名 / TG 频道英文搜索就能直接用规范名，不必再猜。
// 没认片时 en 为空，行为和以前一致。
func (s *Server) botSearch(ctx context.Context, q, en string, limit int) (*resource.Result, error) {
	// resourceSearch 的形参顺序是 (q, kind, en, limit)：kind 留空、en 放第 4 位，
	// 别把英文原名错塞进 kind（那是 movie/tv 过滤位）。
	res, err := s.resourceSearch(ctx, q, "", en, limit)
	// 一个源都没成功时 resource.Search 会返回带 Err 的结果；这里保留结果，
	// 让机器人能把「哪个源挂了」写进回复，而不是干说一句搜索失败。
	return res, err
}

// botSearchTMDB 给机器人提供「先认片」的候选。
//
// 复用 tmdb_search.go 的 fetchTMDBMeta：它默认就是
// tmdb.NewClient(s.tmdbKey(ctx)).SearchMeta(...)，一次返回多候选，而且本身是
// 个可替换的变量（单测不连外网）。这里只做排序截断 + 字段映射：tmdb.MetaHit
// 带海报、简介等一堆 bot 用不上的字段，映射成轻量的 tgbot.TitleHit。
//
// 出错（没 Key / 反代挂了 / 限流）原样返回 err：tgbot 收到错误会自动降级成
// 直接搜资源，不会让 bot 哑掉。
func (s *Server) botSearchTMDB(ctx context.Context, q string, limit int) ([]tgbot.TitleHit, error) {
	hits, err := fetchTMDBMeta(s, ctx, q, "", "")
	if err != nil {
		return nil, err
	}
	chosen := sortAndTrimHits(hits, limit)
	out := make([]tgbot.TitleHit, 0, len(chosen))
	for _, h := range chosen {
		out = append(out, tgbot.TitleHit{
			ID:       h.ID,
			Title:    h.Title,
			Original: h.Original,
			Year:     h.Year,
			Kind:     h.Kind,
			Rating:   h.Vote,
		})
	}
	return out, nil
}

// botTMDBName 查「英文名」：用 language=en-US 搜一次 TMDB。
//
// 和 botSearchTMDB 的区别不只是语言：那边给的 Title 是中文名、Original 是原始
// 语言名；这里 Title 是英文标题（用户真正要复制的东西），Original 退回原始
// 语言名，日韩片能看出「英文名 vs 原名」的差异。
//
// 不能复用 botSearchTMDB 再取 Original —— 对日本片 original_title 是日文，
// 那不叫英文名。
func (s *Server) botTMDBName(ctx context.Context, q string, limit int) ([]tgbot.TitleHit, error) {
	hits, err := tmdb.NewClient(s.tmdbKey(ctx)).SearchMetaLang(ctx, q, "", "", "en-US")
	if err != nil {
		return nil, err
	}
	chosen := sortAndTrimHits(hits, limit)
	out := make([]tgbot.TitleHit, 0, len(chosen))
	for _, h := range chosen {
		out = append(out, tgbot.TitleHit{
			ID:       h.ID,
			Title:    h.Title,    // en-US 标题 = 英文名
			Original: h.Original, // 原始语言名
			Year:     h.Year,
			Kind:     h.Kind,
			Rating:   h.Vote,
		})
	}
	return out, nil
}

// botOffline 提交离线下载。目录沿用「资源保存目录」，和转存落在同一个地方。
func (s *Server) botOffline(ctx context.Context, uri, label string) (*tgbot.Ingest, error) {
	cid := strings.TrimSpace(s.setting(ctx, "res_save_cid"))
	acctID, hashes, err := s.submitOffline(ctx, 0, []string{uri}, cid, label)
	if err != nil {
		return nil, err
	}
	ing := &tgbot.Ingest{
		Title:   strings.TrimSpace(label),
		Pending: true,
		Hashes:  hashes,
		Account: s.accountName(ctx, acctID),
	}
	if cid == "" || cid == "0" {
		ing.Dir = "根目录"
	} else if mgr, err := s.pool.Get(ctx, acctID); err == nil {
		ing.Dir = s.dirLabel(ctx, mgr, cid)
	}
	if s.settingBool(ctx, settingOfflineAutoSync, true) {
		if s.organizerIngestOn(ctx) {
			ing.Note = "下载完成后会自动整理进媒体库、再同步和刮削。"
		} else {
			ing.Note = "下载完成后会自动同步到媒体库。"
		}
	}
	return ing, nil
}

// joinNote 拼两句提示，空的那句不占地方。
func joinNote(a, b string) string {
	if a == "" {
		return b
	}
	return a + "\n" + b
}

// botAllow 解析白名单。除了自己存的列表，也认「消息配置里的 Chat ID」，
// 这样用户在消息页填过 Chat ID 就直接能用，不必再配一遍。
func (s *Server) botAllow(ctx context.Context) []int64 {
	var out []int64
	seen := map[int64]bool{}
	add := func(id int64) {
		if id != 0 && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	for _, f := range strings.FieldsFunc(s.setting(ctx, settingTGBotChats), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == ';'
	}) {
		if id, err := strconv.ParseInt(f, 10, 64); err == nil {
			add(id)
		}
	}
	if id, err := strconv.ParseInt(strings.TrimSpace(s.notify.Config().TG.ChatID), 10, 64); err == nil {
		add(id)
	}
	return out
}

// botBind 把第一个说话的会话记成主人。
func (s *Server) botBind(ctx context.Context, chatID int64) error {
	if err := s.st.SetSetting(ctx, settingTGBotChats, strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	// 顺手把推送用的 Chat ID 补上：同一个会话，用户不用再填一遍
	if strings.TrimSpace(s.notify.Config().TG.ChatID) == "" {
		s.setTGNotifyChatID(ctx, strconv.FormatInt(chatID, 10))
	}
	s.log.Info("Telegram 机器人已绑定首个会话", "chat", chatID)
	return nil
}

// setTGNotifyChatID 只改推送用的 Chat ID，其它字段原样保留。
func (s *Server) setTGNotifyChatID(ctx context.Context, chatID string) {
	cfg := s.notify.Config()
	cfg.TG.ChatID = chatID
	raw, err := json.Marshal(&cfg)
	if err != nil {
		return
	}
	if err := s.st.SetSetting(ctx, settingNotifyConfig, string(raw)); err != nil {
		return
	}
	s.notify.Reload(cfg)
}

func (s *Server) botOffset(ctx context.Context) int64 {
	v, _ := s.st.GetSetting(ctx, settingTGBotOffset)
	n, _ := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
	return n
}

// botState 把状态写进设置项给界面看。同样的状态不重复写库。
func (s *Server) botState(ctx context.Context, msg string) {
	old, _ := s.st.GetSetting(ctx, settingTGBotStatus)
	if old == msg {
		return
	}
	_ = s.st.SetSetting(ctx, settingTGBotStatus, msg)
}

// ==================== 接口 ====================

// handleTGBotStatus 返回机器人现状，给消息配置页的「机器人」面板用。
func (s *Server) handleTGBotStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cfg := s.notify.Config()
	allowed := s.botAllow(ctx)
	status, _ := s.st.GetSetting(ctx, settingTGBotStatus)

	names := make([]map[string]any, 0, len(allowed))
	for _, id := range allowed {
		names = append(names, map[string]any{
			"chat_id": strconv.FormatInt(id, 10),
			"label":   "会话 " + strconv.FormatInt(id, 10),
		})
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"token_set": strings.TrimSpace(cfg.TG.Token) != "",
		// bound=false 时界面要提示「先去 Telegram 给 Bot 发句话」
		"bound":   len(allowed) > 0,
		"allowed": names,
		"status":  status,
		"commands": []map[string]string{
			{"command": "/start", "desc": "开始使用 / 绑定会话"},
			{"command": "/search 片名", "desc": "按片名搜资源"},
			{"command": "/id", "desc": "查看 Chat ID"},
		},
	})
}

// handleTGBotConfig 改白名单。reset=true 清空（回到「先到先得」）。
func (s *Server) handleTGBotConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ChatIDs string `json:"chat_ids"`
		Reset   bool   `json:"reset"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, errors.New("请求体不是合法 JSON"))
		return
	}

	if body.Reset {
		if err := s.st.SetSetting(r.Context(), settingTGBotChats, ""); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		s.log.Info("Telegram 机器人白名单已清空，下一个说话的会话会重新绑定")
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat_ids": ""})
		return
	}

	// 只留数字：用户可能把「-1001234567890」这样的群 id 连负号一起贴进来
	var ids []string
	for _, f := range strings.FieldsFunc(body.ChatIDs, func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == ';'
	}) {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if _, err := strconv.ParseInt(f, 10, 64); err != nil {
			writeErr(w, http.StatusBadRequest,
				errors.New("Chat ID 只能是数字（群组会是负数）：「"+f+"」不认识"))
			return
		}
		ids = append(ids, f)
	}
	if len(ids) == 0 {
		writeErr(w, http.StatusBadRequest, errors.New("至少要填一个 Chat ID；想重新绑定请用「解绑」"))
		return
	}
	val := strings.Join(ids, ",")
	if err := s.st.SetSetting(r.Context(), settingTGBotChats, val); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "chat_ids": val})
}
